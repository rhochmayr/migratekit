package vmware_nbdkit

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/vexxhost/migratekit/internal/vmware"
	"github.com/vmware/govmomi/vim25/types"
)

// fakeTarget is a test double for target.Target that records Connect and
// Disconnect calls and can be configured to return controlled paths/errors.
type fakeTarget struct {
	disk            *types.VirtualDisk
	connectCalls    int
	disconnectCalls int
	path            string
	connectErr      error
	getPathErr      error
}

func (f *fakeTarget) GetDisk() *types.VirtualDisk { return f.disk }

func (f *fakeTarget) Connect(_ context.Context) error {
	f.connectCalls++
	return f.connectErr
}

func (f *fakeTarget) Disconnect(_ context.Context) error {
	f.disconnectCalls++
	return nil
}

func (f *fakeTarget) GetPath(_ context.Context) (string, error) {
	return f.path, f.getPathErr
}

func (f *fakeTarget) Exists(_ context.Context) (bool, error)                    { return true, nil }
func (f *fakeTarget) GetCurrentChangeID(_ context.Context) (*vmware.ChangeID, error) {
	return &vmware.ChangeID{}, nil
}
func (f *fakeTarget) WriteChangeID(_ context.Context, _ *vmware.ChangeID) error { return nil }

// TestConnectAllForV2V_DetachesOnSuccess verifies that cleanup() disconnects
// every disk that was connected when all connections and path lookups succeed.
func TestConnectAllForV2V_DetachesOnSuccess(t *testing.T) {
	ctx := context.Background()
	ft1 := &fakeTarget{path: "/dev/vda"}
	ft2 := &fakeTarget{path: "/dev/vdb"}

	disks := []diskTarget{
		{target: ft1},
		{target: ft2},
	}

	paths, cleanup, err := connectAllForV2V(ctx, disks, "test-vm")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(paths) != 2 {
		t.Fatalf("expected 2 paths, got %d", len(paths))
	}
	if paths[0] != "/dev/vda" || paths[1] != "/dev/vdb" {
		t.Errorf("unexpected paths: %v", paths)
	}
	if ft1.connectCalls != 1 || ft2.connectCalls != 1 {
		t.Errorf("expected 1 Connect per disk, got %d and %d", ft1.connectCalls, ft2.connectCalls)
	}

	// Before cleanup, no disconnects yet.
	if ft1.disconnectCalls != 0 || ft2.disconnectCalls != 0 {
		t.Error("expected no Disconnect before cleanup()")
	}

	cleanup()

	if ft1.disconnectCalls != 1 || ft2.disconnectCalls != 1 {
		t.Errorf("expected 1 Disconnect per disk after cleanup(), got %d and %d",
			ft1.disconnectCalls, ft2.disconnectCalls)
	}
}

// TestConnectAllForV2V_DetachesOnConnectError verifies that if the second
// disk's Connect fails, the first disk (already connected) is still
// disconnected by the internal cleanup before the error is returned.
func TestConnectAllForV2V_DetachesOnConnectError(t *testing.T) {
	ctx := context.Background()
	ft1 := &fakeTarget{path: "/dev/vda"}
	ft2 := &fakeTarget{connectErr: fmt.Errorf("attach failed")}

	disks := []diskTarget{
		{target: ft1},
		{target: ft2},
	}

	_, cleanup, err := connectAllForV2V(ctx, disks, "test-vm")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if cleanup != nil {
		cleanup() // should be safe to call even though it's nil in error path
	}

	// The first disk must have been disconnected as part of the error cleanup.
	if ft1.disconnectCalls != 1 {
		t.Errorf("expected 1 Disconnect for first disk on error, got %d", ft1.disconnectCalls)
	}
	// The second disk never connected, so no Disconnect expected.
	if ft2.disconnectCalls != 0 {
		t.Errorf("expected 0 Disconnect for failed disk, got %d", ft2.disconnectCalls)
	}
}

// TestConnectAllForV2V_DetachesOnEmptyPath verifies that cleanup disconnects
// already-connected disks when GetPath returns an empty string.
func TestConnectAllForV2V_DetachesOnEmptyPath(t *testing.T) {
	ctx := context.Background()
	ft1 := &fakeTarget{path: "/dev/vda"}
	ft2 := &fakeTarget{path: ""} // empty path triggers the error

	disks := []diskTarget{
		{target: ft1},
		{target: ft2},
	}

	_, cleanup, err := connectAllForV2V(ctx, disks, "test-vm")
	if err == nil {
		t.Fatal("expected error for empty path, got nil")
	}
	if cleanup != nil {
		cleanup()
	}

	// Both disks connected before the empty-path check, so both should be disconnected.
	if ft1.disconnectCalls != 1 {
		t.Errorf("expected 1 Disconnect for first disk, got %d", ft1.disconnectCalls)
	}
	if ft2.disconnectCalls != 1 {
		t.Errorf("expected 1 Disconnect for second disk (connected before path check), got %d", ft2.disconnectCalls)
	}
}

func TestBuildV2VDomainXML_SingleDisk(t *testing.T) {
	xml, err := buildV2VDomainXML("test-vm", []string{"/dev/vda"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(xml, "<name>test-vm</name>") {
		t.Error("expected VM name in XML")
	}
	if !strings.Contains(xml, "<source dev='/dev/vda'/>") {
		t.Error("expected disk source path in XML")
	}
	if !strings.Contains(xml, "<target dev='vda' bus='virtio'/>") {
		t.Error("expected target dev 'vda' in XML")
	}
	if count := strings.Count(xml, "<disk "); count != 1 {
		t.Errorf("expected 1 disk element, got %d", count)
	}
}

func TestBuildV2VDomainXML_MultiDisk(t *testing.T) {
	paths := []string{"/dev/sda", "/dev/sdb", "/dev/sdc"}
	xml, err := buildV2VDomainXML("multi-disk-vm", paths)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(xml, "<name>multi-disk-vm</name>") {
		t.Error("expected VM name in XML")
	}

	if count := strings.Count(xml, "<disk "); count != 3 {
		t.Errorf("expected 3 disk elements, got %d", count)
	}

	for i, p := range paths {
		devName := fmt.Sprintf("vd%c", 'a'+rune(i))
		if !strings.Contains(xml, "<source dev='"+p+"'/>") {
			t.Errorf("expected source dev '%s' in XML", p)
		}
		if !strings.Contains(xml, "<target dev='"+devName+"' bus='virtio'/>") {
			t.Errorf("expected target dev '%s' in XML", devName)
		}
	}
}

func TestBuildV2VDomainXML_EmptyPaths(t *testing.T) {
	_, err := buildV2VDomainXML("empty-vm", []string{})
	if err == nil {
		t.Error("expected error for empty paths, got nil")
	}
}

func TestBuildV2VDomainXML_TooManyDisks(t *testing.T) {
	paths := make([]string, 27)
	for i := range paths {
		paths[i] = "/dev/sdx"
	}
	_, err := buildV2VDomainXML("big-vm", paths)
	if err == nil {
		t.Error("expected error for >26 disks, got nil")
	}
}

func TestBuildV2VDomainXML_EmptyPathInSlice(t *testing.T) {
	_, err := buildV2VDomainXML("vm", []string{"/dev/sda", "", "/dev/sdc"})
	if err == nil {
		t.Error("expected error for empty path in slice, got nil")
	}
}

func TestBuildV2VDomainXML_UniqueDevNames(t *testing.T) {
	paths := []string{"/dev/sda", "/dev/sdb"}
	xml, err := buildV2VDomainXML("two-disk-vm", paths)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if strings.Count(xml, "dev='vda'") != 1 {
		t.Error("expected exactly one 'vda' target dev")
	}
	if strings.Count(xml, "dev='vdb'") != 1 {
		t.Error("expected exactly one 'vdb' target dev")
	}
}
