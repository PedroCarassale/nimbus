package firecracker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// startJailer runs Firecracker inside its jailer (chroot, uid/gid, cgroup v2).
// The kernel and drives are hardlinked into the jail when they share a
// filesystem, so a cold start does not copy the rootfs. NIMBUS_FC_JAILER
// selects this path; a cgroup that will not delegate controllers fails here
// and the caller surfaces that error instead of silently skipping the jail.
func startJailer(ctx context.Context, cfg Config, id, rootfs, codeImage string, mem, vcpu int) (*VM, error) {
	if _, err := os.Stat(cfg.JailerBin); err != nil {
		return nil, fmt.Errorf("no está el jailer en %s", cfg.JailerBin)
	}
	// The jailed process opens /dev/kvm after the uid drop.
	_ = os.Chmod("/dev/kvm", 0o666)

	base := filepath.Join(cfg.WorkDir, "fc", "jail")
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, err
	}
	execName := filepath.Base(cfg.Bin)
	root := filepath.Join(base, execName, id, "root")
	logPath := filepath.Join(cfg.runDir(), id+".log")
	pidPath := filepath.Join(cfg.runDir(), id+".pid")
	if err := os.MkdirAll(cfg.runDir(), 0o755); err != nil {
		return nil, err
	}
	cmd := exec.Command(cfg.JailerBin,
		"--id", id,
		"--exec-file", cfg.Bin,
		"--uid", "123",
		"--gid", "123",
		"--cgroup-version", "2",
		"--chroot-base-dir", base,
		"--new-pid-ns",
		"--",
		// The jailer already forwards its own --id. Passing it again makes
		// Firecracker exit with DuplicateArgument before the socket exists.
		"--api-sock", "/fc.sock",
	)
	logf, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		logf.Close()
		return nil, fmt.Errorf("no se pudo iniciar el jailer: %w", err)
	}
	logf.Close()
	_ = os.WriteFile(pidPath, []byte(fmt.Sprintf("%d\n", cmd.Process.Pid)), 0o644)

	vm := &VM{
		ID:      id,
		cmd:     cmd,
		logPath: logPath,
		exited:  make(chan struct{}),
		remove:  []string{logPath, pidPath, filepath.Join(base, execName, id)},
	}
	go func() {
		_ = cmd.Wait()
		close(vm.exited)
	}()
	fail := func(err error) (*VM, error) {
		extra := waitTail(logPath)
		vm.Kill()
		os.RemoveAll(filepath.Dir(root))
		if extra != "" {
			err = fmt.Errorf("%w: %s", err, extra)
		}
		return nil, err
	}

	// --new-pid-ns makes the jailer parent exit 0 as soon as it has cloned
	// Firecracker into the new namespace. That exit is not a failed boot.
	// The socket shows up after the parent is already gone.
	apiSock := filepath.Join(root, "fc.sock")
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := waitFile(wctx, apiSock); err != nil {
		return fail(fmt.Errorf("el jailer no abrió el socket: %w", err))
	}
	if raw, err := os.ReadFile(filepath.Join(root, execName+".pid")); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid > 1 {
			vm.guestPid = pid
			_ = os.WriteFile(pidPath, []byte(strconv.Itoa(pid)+"\n"), 0o644)
		}
	}
	if err := linkOrCopy(cfg.Kernel, filepath.Join(root, "vmlinux")); err != nil {
		return fail(err)
	}
	if err := linkOrCopy(rootfs, filepath.Join(root, "rootfs.ext4")); err != nil {
		return fail(err)
	}
	if err := linkOrCopy(codeImage, filepath.Join(root, "code.ext4")); err != nil {
		return fail(err)
	}
	vsockPath := filepath.Join(root, "vsock.sock")
	if err := configure(ctx, apiSock, "/vmlinux", "/rootfs.ext4", "/code.ext4", "/vsock.sock", mem, vcpu, "/vmm.log"); err != nil {
		return fail(err)
	}
	conn, err := Dial(ctx, vsockPath)
	if err != nil {
		return fail(fmt.Errorf("vsock: %w", err))
	}
	vm.conn = conn
	return vm, nil
}

func waitFile(ctx context.Context, path string) error {
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func linkOrCopy(src, dst string) error {
	_ = os.Remove(dst)
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	return copyFile(src, dst, 0o644)
}
