package nbdkit

import (
	"fmt"
	"os"
	"os/exec"
)

type CompressionMethod string

const (
	NoCompression     CompressionMethod = "none"
	ZlibCompression   CompressionMethod = "zlib"
	FastLzCompression CompressionMethod = "fastlz"
	SkipzCompression  CompressionMethod = "skipz"
)

type NbdkitBuilder struct {
	server      string
	username    string
	password    string
	thumbprint  string
	vm          string
	snapshot    string
	filename    string
	compression CompressionMethod
}

func NewNbdkitBuilder() *NbdkitBuilder {
	return &NbdkitBuilder{}
}

func (b *NbdkitBuilder) Server(server string) *NbdkitBuilder {
	b.server = server
	return b
}

func (b *NbdkitBuilder) Username(username string) *NbdkitBuilder {
	b.username = username
	return b
}

func (b *NbdkitBuilder) Password(password string) *NbdkitBuilder {
	b.password = password
	return b
}

func (b *NbdkitBuilder) Thumbprint(thumbprint string) *NbdkitBuilder {
	b.thumbprint = thumbprint
	return b
}

func (b *NbdkitBuilder) VirtualMachine(vm string) *NbdkitBuilder {
	b.vm = vm
	return b
}

func (b *NbdkitBuilder) Snapshot(snapshot string) *NbdkitBuilder {
	b.snapshot = snapshot
	return b
}

func (b *NbdkitBuilder) Filename(filename string) *NbdkitBuilder {
	b.filename = filename
	return b
}

func (b *NbdkitBuilder) Compression(method CompressionMethod) *NbdkitBuilder {
	b.compression = method
	return b
}

func (b *NbdkitBuilder) Build() (*NbdkitServer, error) {
	tmp, err := os.MkdirTemp("", "migratekit-")
	if err != nil {
		return nil, err
	}

	socket := fmt.Sprintf("%s/nbdkit.sock", tmp)
	pidFile := fmt.Sprintf("%s/nbdkit.pid", tmp)

	cmd := exec.Command(
		"nbdkit",
		"--exit-with-parent",
		"--readonly",
		"--foreground",
		fmt.Sprintf("--unix=%s", socket),
		fmt.Sprintf("--pidfile=%s", pidFile),
		"vddk",
		fmt.Sprintf("server=%s", b.server),
		fmt.Sprintf("user=%s", b.username),
		fmt.Sprintf("password=%s", b.password),
		fmt.Sprintf("thumbprint=%s", b.thumbprint),
		fmt.Sprintf("compression=%s", b.compression),
		fmt.Sprintf("vm=moref=%s", b.vm),
		fmt.Sprintf("snapshot=%s", b.snapshot),
		"transports=file:nbdssl:nbd",
		b.filename,
	)

	// Scope LD_LIBRARY_PATH to this nbdkit child only.
	//
	// nbdkit's VDDK plugin needs VMware's bundled libraries under
	// /usr/lib64/vmware-vix-disklib/lib64 on its library search path.
	// Previously this was done with os.Setenv(), which mutates migratekit's
	// own process environment and is therefore inherited by every subsequent
	// child — including virt-v2v-in-place and the supermin it spawns.
	//
	// VMware VDDK ships its own libcrypto.so.3 / libssl.so.3 under that
	// directory. When they take precedence over Fedora's /lib64 versions,
	// librpm_sequoia.so.1 (used by supermin) ends up linked against VDDK's
	// libcrypto, which lacks the EVP_idea_cfb64 symbol that Fedora's
	// rpm-sequoia requires, producing:
	//
	//   /usr/bin/supermin: symbol lookup error: /lib64/librpm_sequoia.so.1:
	//   undefined symbol: EVP_idea_cfb64, version OPENSSL_3.0.0
	//
	// Using cmd.Env (instead of os.Setenv) confines LD_LIBRARY_PATH to the
	// nbdkit invocation so downstream children see a clean environment.
	cmd.Env = append(os.Environ(), "LD_LIBRARY_PATH=/usr/lib64/vmware-vix-disklib/lib64")

	return &NbdkitServer{
		cmd:     cmd,
		socket:  socket,
		pidFile: pidFile,
	}, nil
}
