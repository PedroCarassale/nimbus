package firecracker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/PedroCarassale/nimbus/compute-plane/internal/warm"
)

const bootArgs = "console=ttyS0 reboot=k panic=1 pci=off random.trust_cpu=on init=/sbin/nimbus-init"

// VM is one Firecracker process and the vsock connected to its guest.
type VM struct {
	ID       string
	conn     io.ReadWriteCloser
	cmd      *exec.Cmd
	remove   []string
	logPath  string
	once     sync.Once
	exited   chan struct{}
	guestPid int
}

// boot builds the code drive and starts a microVM with no NIC.
// The returned conn speaks the warm JSON-line protocol.
func boot(ctx context.Context, cfg Config, spec warm.Spec) (*VM, error) {
	rootfs, err := checkArtifacts(cfg, spec.Config.Runtime)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.runDir(), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.codeDir(), 0o755); err != nil {
		return nil, err
	}
	id := newVMID()
	codeImage := filepath.Join(cfg.codeDir(), id+".ext4")
	if err := buildCodeImage(spec.WorkDir, codeImage, buildGuestConfig(spec)); err != nil {
		return nil, err
	}

	mem := MemMiB(spec.Config.MemoryMB)
	vcpu := VCPUCount(spec.Config.CPUsMilli)
	log.Printf("[firecracker] vm %s runtime=%s mem=%dMiB vcpu=%d jailer=%t", id, spec.Config.Runtime, mem, vcpu, cfg.UseJailer)

	var vm *VM
	if cfg.UseJailer {
		vm, err = startJailer(ctx, cfg, id, rootfs, codeImage, mem, vcpu)
	} else {
		vm, err = startDirect(ctx, cfg, id, rootfs, codeImage, mem, vcpu)
	}
	if err != nil {
		os.Remove(codeImage)
		return nil, err
	}
	vm.remove = append(vm.remove, codeImage)
	return vm, nil
}

func checkArtifacts(cfg Config, rt string) (string, error) {
	rootfs := cfg.Rootfs(rt)
	if _, err := os.Stat(rootfs); err != nil {
		return "", fmt.Errorf("no está el rootfs %s (make fc-prepare)", rootfs)
	}
	if _, err := os.Stat(cfg.Kernel); err != nil {
		return "", fmt.Errorf("no está el kernel %s (make fc-prepare)", cfg.Kernel)
	}
	if _, err := os.Stat(cfg.Bin); err != nil {
		return "", fmt.Errorf("no está el binario de firecracker en %s", cfg.Bin)
	}
	return rootfs, nil
}

func startDirect(ctx context.Context, cfg Config, id, rootfs, codeImage string, mem, vcpu int) (*VM, error) {
	apiSock := filepath.Join(cfg.runDir(), id+".sock")
	vsockPath := filepath.Join(cfg.runDir(), id+".vsock")
	logPath := filepath.Join(cfg.runDir(), id+".log")
	pidPath := filepath.Join(cfg.runDir(), id+".pid")
	vmmLog := filepath.Join(cfg.runDir(), id+".vmm")
	_ = os.Remove(apiSock)
	_ = os.Remove(vsockPath)
	if f, err := os.Create(vmmLog); err == nil {
		f.Close()
	}

	cmd := exec.Command(cfg.Bin, "--api-sock", apiSock, "--id", id)
	release, err := attachSerial(cmd, logPath)
	if err != nil {
		return nil, err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		release()
		return nil, fmt.Errorf("no se pudo iniciar firecracker: %w", err)
	}
	release()
	_ = os.WriteFile(pidPath, []byte(fmt.Sprintf("%d\n", cmd.Process.Pid)), 0o644)

	vm := &VM{
		ID:      id,
		cmd:     cmd,
		logPath: logPath,
		exited:  make(chan struct{}),
		remove:  []string{apiSock, vsockPath, logPath, pidPath, vmmLog},
	}
	go func() {
		_ = cmd.Wait()
		close(vm.exited)
	}()

	fail := func(err error) (*VM, error) {
		extra := waitTail(logPath)
		if vmm := tailFile(vmmLog, 800); vmm != "" {
			extra = extra + "\n" + vmm
		}
		vm.Kill()
		if extra != "" {
			err = fmt.Errorf("%w: %s", err, extra)
		}
		return nil, err
	}

	if err := waitPath(ctx, apiSock, vm); err != nil {
		return fail(fmt.Errorf("el socket de firecracker no apareció: %w", err))
	}
	if err := configure(ctx, apiSock, cfg.Kernel, rootfs, codeImage, vsockPath, mem, vcpu, vmmLog); err != nil {
		return fail(err)
	}
	conn, err := Dial(ctx, vsockPath)
	if err != nil {
		return fail(fmt.Errorf("vsock: %w", err))
	}
	vm.conn = conn
	return vm, nil
}

func configure(ctx context.Context, apiSock, kernel, rootfs, codeImage, vsockPath string, mem, vcpu int, vmmLog string) error {
	// The VMM log is diagnostic. A rejection here still leaves stdout/stderr.
	_ = putJSON(ctx, apiSock, "/logger", map[string]any{
		"log_path":        vmmLog,
		"level":           "Info",
		"show_level":      true,
		"show_log_origin": false,
	})
	if err := putJSON(ctx, apiSock, "/machine-config", machineBody(mem, vcpu)); err != nil {
		return err
	}
	if err := putJSON(ctx, apiSock, "/boot-source", map[string]any{
		"kernel_image_path": kernel,
		"boot_args":         bootArgs,
	}); err != nil {
		return err
	}
	if err := putJSON(ctx, apiSock, "/drives/rootfs", map[string]any{
		"drive_id":       "rootfs",
		"path_on_host":   rootfs,
		"is_root_device": true,
		"is_read_only":   true,
	}); err != nil {
		return err
	}
	if err := putJSON(ctx, apiSock, "/drives/code", map[string]any{
		"drive_id":       "code",
		"path_on_host":   codeImage,
		"is_root_device": false,
		"is_read_only":   true,
	}); err != nil {
		return err
	}
	// No /network-interfaces. The guest has no eth device.
	if err := putJSON(ctx, apiSock, "/vsock", map[string]any{
		"guest_cid": 3,
		"uds_path":  vsockPath,
	}); err != nil {
		return err
	}
	return putJSON(ctx, apiSock, "/actions", map[string]any{"action_type": "InstanceStart"})
}

func waitPath(ctx context.Context, path string, vm *VM) error {
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-vm.exited:
			return fmt.Errorf("firecracker terminó antes de abrir %s", filepath.Base(path))
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// Kill stops the VMM and removes the sockets, the log and the code image.
// It is safe to call more than once.
func (v *VM) Kill() {
	if v == nil {
		return
	}
	v.once.Do(func() {
		if v.conn != nil {
			_ = v.conn.Close()
		}
		// The jailer parent exits on purpose after --new-pid-ns. guestPid is
		// the Firecracker child that keeps running.
		if v.guestPid > 1 {
			_ = syscall.Kill(v.guestPid, syscall.SIGKILL)
		}
		if v.cmd != nil && v.cmd.Process != nil {
			_ = syscall.Kill(-v.cmd.Process.Pid, syscall.SIGKILL)
		}
		if v.exited != nil {
			select {
			case <-v.exited:
			case <-time.After(3 * time.Second):
			}
		}
		for _, p := range v.remove {
			if p == "" {
				continue
			}
			_ = os.RemoveAll(p)
		}
	})
}

func (v *VM) LogTail() string {
	if v == nil {
		return ""
	}
	return tailFile(v.logPath, 6000)
}

func newVMID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("fc%d", time.Now().UnixNano())
	}
	return "fc" + hex.EncodeToString(b[:])
}

func tailFile(path string, max int) string {
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 {
		return ""
	}
	if len(raw) > max {
		raw = raw[len(raw)-max:]
	}
	return string(raw)
}
