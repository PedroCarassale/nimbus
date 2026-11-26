package firecracker

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/PedroCarassale/nimbus/compute-plane/internal/runtime"
)

// Config is where the microVM files live. Paths are inside the compute
// plane mount namespace: the kernel and rootfs are read-only bind mounts,
// the code images are written under NIMBUS_WORK_DIR.
type Config struct {
	Bin       string
	JailerBin string
	UseJailer bool
	Kernel    string
	RootFSDir string
	WorkDir   string
}

func LoadConfig() Config {
	work := os.Getenv("NIMBUS_WORK_DIR")
	if work == "" {
		work = "/tmp/nimbus-work"
	}
	return Config{
		Bin:       getenv("NIMBUS_FC_BIN", "/usr/local/bin/firecracker"),
		JailerBin: getenv("NIMBUS_FC_JAILER_BIN", "/usr/local/bin/jailer"),
		UseJailer: enabled(os.Getenv("NIMBUS_FC_JAILER")),
		Kernel:    getenv("NIMBUS_FC_KERNEL", "/var/lib/nimbus/fc/vmlinux"),
		RootFSDir: getenv("NIMBUS_FC_ROOTFS_DIR", "/var/lib/nimbus/fc/rootfs"),
		WorkDir:   work,
	}
}

func getenv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func enabled(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "on", "yes":
		return true
	default:
		return false
	}
}

// Rootfs is the ext4 image built from the runtime's Docker image.
func (c Config) Rootfs(rt string) string {
	return filepath.Join(c.RootFSDir, runtime.Canonical(rt)+".ext4")
}

// VCPUCount maps the Docker --cpus quota onto Firecracker's vcpu_count.
// The API allows 1 or an even number, and this platform's cpus field stops
// at 4, so 0.1..1 stays 1 vCPU, anything above 1 rounds up to the next even
// number and 4 is the cap. A fractional CPU is not a CFS quota inside the guest.
func VCPUCount(milli int) int {
	if milli <= 1000 {
		return 1
	}
	n := (milli + 999) / 1000
	if n%2 == 1 {
		n++
	}
	if n > 4 {
		return 4
	}
	return n
}

// kernelReserveMiB is RAM the guest kernel and init keep outside the
// workload cgroup. memoryMb stays the limit of the runtime process.
const kernelReserveMiB = 64

// MemMiB is the guest's mem_size_mib. The requested memoryMb is enforced
// with a cgroup on the runtime; this adds the kernel reserve so that
// filling the limit kills the handler instead of panicking the VM.
func MemMiB(memoryMB int) int {
	if memoryMB < 1 {
		memoryMB = 128
	}
	return memoryMB + kernelReserveMiB
}

func (c Config) runDir() string {
	return filepath.Join(c.WorkDir, "fc", "run")
}

func (c Config) codeDir() string {
	return filepath.Join(c.WorkDir, "fc", "code")
}
