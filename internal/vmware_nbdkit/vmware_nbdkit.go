package vmware_nbdkit

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	log "github.com/sirupsen/logrus"
	"github.com/vexxhost/migratekit/internal/nbdcopy"
	"github.com/vexxhost/migratekit/internal/nbdkit"
	"github.com/vexxhost/migratekit/internal/progress"
	"github.com/vexxhost/migratekit/internal/target"
	"github.com/vexxhost/migratekit/internal/vmware"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
	"libguestfs.org/libnbd"
)

const MaxChunkSize = 64 * 1024 * 1024

type VddkConfig struct {
	Debug       bool
	Endpoint    *url.URL
	Thumbprint  string
	Compression nbdkit.CompressionMethod
}

type NbdkitServers struct {
	VddkConfig     *VddkConfig
	VirtualMachine *object.VirtualMachine
	SnapshotRef    types.ManagedObjectReference
	Servers        []*NbdkitServer
}

type NbdkitServer struct {
	Servers *NbdkitServers
	Disk    *types.VirtualDisk
	Nbdkit  *nbdkit.NbdkitServer
}

func NewNbdkitServers(vddk *VddkConfig, vm *object.VirtualMachine) *NbdkitServers {
	return &NbdkitServers{
		VddkConfig:     vddk,
		VirtualMachine: vm,
		Servers:        []*NbdkitServer{},
	}
}

func (s *NbdkitServers) createSnapshot(ctx context.Context) error {
	task, err := s.VirtualMachine.CreateSnapshot(ctx, "migratekit", "Ephemeral snapshot for MigrateKit", false, false)
	if err != nil {
		return err
	}

	bar := progress.NewVMwareProgressBar("Creating snapshot")
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		bar.Loop(ctx.Done())
	}()
	defer cancel()

	info, err := task.WaitForResult(ctx, bar)
	if err != nil {
		return err
	}

	s.SnapshotRef = info.Result.(types.ManagedObjectReference)
	return nil
}

func (s *NbdkitServers) Start(ctx context.Context) error {
	err := s.createSnapshot(ctx)
	if err != nil {
		return err
	}

	var snapshot mo.VirtualMachineSnapshot
	err = s.VirtualMachine.Properties(ctx, s.SnapshotRef, []string{"config.hardware"}, &snapshot)
	if err != nil {
		return err
	}

	for _, device := range snapshot.Config.Hardware.Device {
		switch disk := device.(type) {
		case *types.VirtualDisk:
			backing := disk.Backing.(types.BaseVirtualDeviceFileBackingInfo)
			info := backing.GetVirtualDeviceFileBackingInfo()

			password, _ := s.VddkConfig.Endpoint.User.Password()
			server, err := nbdkit.NewNbdkitBuilder().
				Server(s.VddkConfig.Endpoint.Host).
				Username(s.VddkConfig.Endpoint.User.Username()).
				Password(password).
				Thumbprint(s.VddkConfig.Thumbprint).
				VirtualMachine(s.VirtualMachine.Reference().Value).
				Snapshot(s.SnapshotRef.Value).
				Filename(info.FileName).
				Compression(s.VddkConfig.Compression).
				Build()
			if err != nil {
				return err
			}

			if err := server.Start(); err != nil {
				return err
			}

			s.Servers = append(s.Servers, &NbdkitServer{
				Servers: s,
				Disk:    disk,
				Nbdkit:  server,
			})
		}
	}

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		log.Warn("Received interrupt signal, cleaning up...")

		err := s.Stop(ctx)
		if err != nil {
			log.WithError(err).Fatal("Failed to stop nbdkit servers")
		}

		os.Exit(1)
	}()

	return nil
}

func (s *NbdkitServers) removeSnapshot(ctx context.Context) error {
	consolidate := true
	task, err := s.VirtualMachine.RemoveSnapshot(ctx, s.SnapshotRef.Value, false, &consolidate)
	if err != nil {
		return err
	}

	bar := progress.NewVMwareProgressBar("Removing snapshot")
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		bar.Loop(ctx.Done())
	}()
	defer cancel()

	_, err = task.WaitForResult(ctx, bar)
	if err != nil {
		return err
	}

	return nil
}

func (s *NbdkitServers) Stop(ctx context.Context) error {
	for _, server := range s.Servers {
		if err := server.Nbdkit.Stop(); err != nil {
			return err
		}
	}

	err := s.removeSnapshot(ctx)
	if err != nil {
		return err
	}

	return nil
}

// buildV2VDomainXML generates a minimal libvirt domain XML referencing each
// supplied block-device path as a virtio disk. The target dev names are vda,
// vdb, … vdz (at most 26 disks). Returns an error if paths is empty or
// contains more than 26 entries.
func buildV2VDomainXML(vmName string, paths []string) (string, error) {
	if len(paths) == 0 {
		return "", fmt.Errorf("no disk paths provided")
	}
	if len(paths) > 26 {
		return "", fmt.Errorf("too many disks (%d); at most 26 are supported", len(paths))
	}

	var disks strings.Builder
	for i, p := range paths {
		if p == "" {
			return "", fmt.Errorf("disk %d has empty source path", i)
		}
		devName := fmt.Sprintf("vd%c", 'a'+rune(i))
		fmt.Fprintf(&disks,
			"    <disk type='block' device='disk'>\n"+
				"      <driver name='qemu' type='raw'/>\n"+
				"      <source dev='%s'/>\n"+
				"      <target dev='%s' bus='virtio'/>\n"+
				"    </disk>\n",
			p, devName)
	}

	xml := fmt.Sprintf(`<domain type='kvm'>
  <name>%s</name>
  <memory unit='MiB'>2048</memory>
  <vcpu>1</vcpu>
  <os>
    <type arch='x86_64'>hvm</type>
  </os>
  <devices>
%s  </devices>
</domain>`, vmName, disks.String())

	return xml, nil
}

type diskTarget struct {
	path   string
	target target.Target
}

// connectAllForV2V re-attaches each disk in disks to its target and resolves
// its device path for use by virt-v2v-in-place. It returns the resolved paths
// and a cleanup function that disconnects every successfully connected target.
// The caller must invoke cleanup (typically via defer) to ensure volumes are
// returned to "available" status after virt-v2v-in-place finishes.
// If an error occurs mid-loop, cleanup is called before returning.
func connectAllForV2V(ctx context.Context, disks []diskTarget, vmName string) ([]string, func(), error) {
	paths := make([]string, len(disks))
	connected := make([]target.Target, 0, len(disks))
	cleanup := func() {
		for i, t := range connected {
			if derr := t.Disconnect(ctx); derr != nil {
				log.WithError(derr).WithField("disk", i).
					Warn("Failed to detach target volume after virt-v2v-in-place")
			}
		}
	}

	for i, dt := range disks {
		if err := dt.target.Connect(ctx); err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("re-attach disk %d for v2v: %w", i, err)
		}
		connected = append(connected, dt.target)

		p, err := dt.target.GetPath(ctx)
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("get path for disk %d for v2v: %w", i, err)
		}
		if p == "" {
			cleanup()
			return nil, nil, fmt.Errorf("disk %d (volume on VM %s) has no resolvable device path; cannot run virt-v2v-in-place", i, vmName)
		}
		paths[i] = p
	}

	return paths, cleanup, nil
}

func (s *NbdkitServers) MigrationCycle(ctx context.Context, runV2V bool) error {
	err := s.Start(ctx)
	if err != nil {
		return err
	}
	defer func() {
		err := s.Stop(ctx)
		if err != nil {
			log.WithError(err).Fatal("Failed to stop nbdkit servers")
		}
	}()

	var synced []diskTarget

	for _, server := range s.Servers {
		t, err := target.NewOpenStack(ctx, s.VirtualMachine, server.Disk)
		if err != nil {
			return err
		}

		err = server.SyncToTarget(ctx, t)
		if err != nil {
			return err
		}

		path, err := t.GetPath(ctx)
		if err != nil {
			return err
		}

		synced = append(synced, diskTarget{path: path, target: t})
	}

	if runV2V {
		paths, cleanup, err := connectAllForV2V(ctx, synced, s.VirtualMachine.Name())
		if err != nil {
			return err
		}
		defer cleanup()

		log.WithFields(log.Fields{
			"paths": paths,
		}).Info("Resolved device paths for virt-v2v-in-place")

		xmlContent, err := buildV2VDomainXML(s.VirtualMachine.Name(), paths)
		if err != nil {
			return err
		}

		tmpFile, err := os.CreateTemp("", "migratekit-v2v-*.xml")
		if err != nil {
			return err
		}
		defer os.Remove(tmpFile.Name())

		if _, err := tmpFile.WriteString(xmlContent); err != nil {
			tmpFile.Close()
			return err
		}
		tmpFile.Close()

		log.Info("Running virt-v2v-in-place")

		var cmd *exec.Cmd
		if s.VddkConfig.Debug {
			cmd = exec.Command("virt-v2v-in-place", "-v", "-x", "-i", "libvirtxml", tmpFile.Name())
		} else {
			cmd = exec.Command("virt-v2v-in-place", "-i", "libvirtxml", tmpFile.Name())
		}

		cmd.Env = append(os.Environ(), "LIBGUESTFS_BACKEND=direct")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		if err := cmd.Run(); err != nil {
			return err
		}

		for _, dt := range synced {
			if err := dt.target.WriteChangeID(ctx, &vmware.ChangeID{}); err != nil {
				return err
			}
		}
	}

	return nil
}

func (s *NbdkitServer) FullCopyToTarget(t target.Target, path string, targetIsClean bool) error {
	logger := log.WithFields(log.Fields{
		"vm":   s.Servers.VirtualMachine.Name(),
		"disk": s.Disk.Backing.(types.BaseVirtualDeviceFileBackingInfo).GetVirtualDeviceFileBackingInfo().FileName,
	})

	logger.Info("Starting full copy")

	err := nbdcopy.Run(
		s.Nbdkit.LibNBDExportName(),
		path,
		s.Disk.CapacityInBytes,
		targetIsClean,
	)
	if err != nil {
		return err
	}

	logger.Info("Full copy completed")

	return nil
}

func (s *NbdkitServer) IncrementalCopyToTarget(ctx context.Context, t target.Target, path string) error {
	logger := log.WithFields(log.Fields{
		"vm":   s.Servers.VirtualMachine.Name(),
		"disk": s.Disk.Backing.(types.BaseVirtualDeviceFileBackingInfo).GetVirtualDeviceFileBackingInfo().FileName,
	})

	logger.Info("Starting incremental copy")

	currentChangeId, err := t.GetCurrentChangeID(ctx)
	if err != nil {
		return err
	}

	handle, err := libnbd.Create()
	if err != nil {
		return err
	}

	err = handle.ConnectUri(s.Nbdkit.LibNBDExportName())
	if err != nil {
		return err
	}

	fd, err := os.OpenFile(path, os.O_WRONLY|os.O_EXCL|syscall.O_DIRECT, 0644)
	if err != nil {
		return err
	}
	defer fd.Close()

	startOffset := int64(0)
	bar := progress.DataProgressBar("Incremental copy", s.Disk.CapacityInBytes)

	for {
		req := types.QueryChangedDiskAreas{
			This:        s.Servers.VirtualMachine.Reference(),
			Snapshot:    &s.Servers.SnapshotRef,
			DeviceKey:   s.Disk.Key,
			StartOffset: startOffset,
			ChangeId:    currentChangeId.Value,
		}

		res, err := methods.QueryChangedDiskAreas(ctx, s.Servers.VirtualMachine.Client(), &req)
		if err != nil {
			return err
		}

		diskChangeInfo := res.Returnval

		for _, area := range diskChangeInfo.ChangedArea {
			for offset := area.Start; offset < area.Start+area.Length; {
				chunkSize := area.Length - (offset - area.Start)
				if chunkSize > MaxChunkSize {
					chunkSize = MaxChunkSize
				}

				buf := make([]byte, chunkSize)
				err = handle.Pread(buf, uint64(offset), nil)
				if err != nil {
					return err
				}

				_, err = fd.WriteAt(buf, offset)
				if err != nil {
					return err
				}

				bar.Set64(offset + chunkSize)
				offset += chunkSize
			}
		}

		startOffset = diskChangeInfo.StartOffset + diskChangeInfo.Length
		bar.Set64(startOffset)

		if startOffset == s.Disk.CapacityInBytes {
			break
		}
	}

	return nil
}

func (s *NbdkitServer) SyncToTarget(ctx context.Context, t target.Target) error {
	snapshotChangeId, err := vmware.GetChangeID(s.Disk)
	if err != nil {
		return err
	}

	needFullCopy, targetIsClean, err := target.NeedsFullCopy(ctx, t)
	if err != nil {
		return err
	}

	err = t.Connect(ctx)
	if err != nil {
		return err
	}
	defer t.Disconnect(ctx)

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		log.Warn("Received interrupt signal, cleaning up...")

		err := t.Disconnect(ctx)
		if err != nil {
			log.WithError(err).Fatal("Failed to disconnect from target")
		}

		os.Exit(1)
	}()

	path, err := t.GetPath(ctx)
	if err != nil {
		return err
	}

	if needFullCopy {
		err = s.FullCopyToTarget(t, path, targetIsClean)
		if err != nil {
			return err
		}
	} else {
		err = s.IncrementalCopyToTarget(ctx, t, path)
		if err != nil {
			return err
		}
	}

	err = t.WriteChangeID(ctx, snapshotChangeId)
	if err != nil {
		return err
	}

	return nil
}
