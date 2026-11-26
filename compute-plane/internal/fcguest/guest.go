// Package fcguest is PID 1 inside a Nimbus microVM.
// It mounts the code drive, drops to uid 1000, and proxies the runtime
// bootstrap's stdin/stdout over the Firecracker vsock so the host can
// speak the same JSON lines a warm container speaks on its pipes.
package fcguest

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/PedroCarassale/nimbus/compute-plane/internal/fcwire"
	"golang.org/x/sys/unix"
)

const (
	codeMount = "/mnt/code"
	taskMount = "/var/task"
	codeDev   = "/dev/vdb"
)

// Run is the guest init. It returns only when the runtime has exited.
func Run() error {
	klog("start")
	if os.Getenv("PATH") == "" {
		_ = os.Setenv("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	if err := mountBasics(); err != nil {
		return fail(err)
	}
	// Overcommit lets the runtime touch pages until the OOM killer fires.
	// Without it a big malloc returns ENOMEM and the handler exits 1, which
	// is not the 137 Docker's cgroup reports for the same limit.
	// oom_kill_allocating_task makes that victim the allocator, not init.
	_ = os.WriteFile("/proc/sys/vm/overcommit_memory", []byte("1\n"), 0o644)
	_ = os.WriteFile("/proc/sys/vm/oom_kill_allocating_task", []byte("1\n"), 0o644)
	_ = os.WriteFile("/proc/sys/vm/panic_on_oom", []byte("0\n"), 0o644)
	// Leave the kernel a free-page reserve so the fault path can run the
	// OOM killer instead of oopsing when the last page is gone.
	_ = os.WriteFile("/proc/sys/vm/min_free_kbytes", []byte("24576\n"), 0o644)

	if err := mountCode(); err != nil {
		return fail(err)
	}
	cfg, err := loadConfig(codeMount + "/" + fcwire.ConfigName)
	if err != nil {
		return fail(err)
	}
	if err := bindTask(codeMount + "/" + fcwire.TaskDir); err != nil {
		return fail(err)
	}

	conn, err := acceptOnce()
	if err != nil {
		return fail(err)
	}
	defer conn.Close()

	return fail(supervise(conn, cfg))
}

func mountBasics() error {
	mounts := []struct {
		src, target, fstype, data string
		flags                     uintptr
	}{
		{"proc", "/proc", "proc", "", 0},
		{"sysfs", "/sys", "sysfs", "", 0},
		{"devtmpfs", "/dev", "devtmpfs", "mode=0755", 0},
		{"tmpfs", "/tmp", "tmpfs", "mode=1777,size=16m", 0},
		{"tmpfs", "/run", "tmpfs", "mode=0755,size=16m", 0},
	}
	for _, m := range mounts {
		if err := mount(m.src, m.target, m.fstype, m.flags, m.data); err != nil {
			return err
		}
	}
	return nil
}

// limitRuntime puts the bootstrap in a memory cgroup of exactly the
// requested MiB. Init and the kernel sit in the reserve added on the
// host (kernelReserveMiB), so this SIGKILL does not panic the guest.
func limitRuntime(pid int, memoryMB string) error {
	mb, err := strconv.Atoi(strings.TrimSpace(memoryMB))
	if err != nil || mb < 1 {
		mb = 128
	}
	// /run is the tmpfs init mounted. sysfs does not always allow creating
	// /sys/fs/cgroup, and cgroup2 can be mounted on any empty directory.
	const root = "/run/cgroup"
	if err := os.MkdirAll(root, 0o755); err != nil && !errors.Is(err, syscall.EROFS) {
		return err
	}
	if err := unix.Mount("cgroup2", root, "cgroup2", 0, ""); err != nil && !errors.Is(err, unix.EBUSY) {
		return fmt.Errorf("mount cgroup2: %w", err)
	}
	if err := os.MkdirAll(root+"/init", 0o755); err != nil {
		return err
	}
	// Controllers cannot be enabled in a cgroup that still has processes.
	if err := os.WriteFile(root+"/init/cgroup.procs", []byte("1\n"), 0o644); err != nil {
		return fmt.Errorf("mover init: %w", err)
	}
	if err := os.WriteFile(root+"/cgroup.subtree_control", []byte("+memory\n"), 0o644); err != nil {
		return fmt.Errorf("memory controller: %w", err)
	}
	rt := root + "/runtime"
	if err := os.MkdirAll(rt, 0o755); err != nil {
		return err
	}
	max := strconv.Itoa(mb * 1024 * 1024)
	if err := os.WriteFile(rt+"/memory.max", []byte(max+"\n"), 0o644); err != nil {
		return fmt.Errorf("memory.max: %w", err)
	}
	_ = os.WriteFile(rt+"/memory.swap.max", []byte("0\n"), 0o644)
	if err := os.WriteFile(rt+"/cgroup.procs", []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		return fmt.Errorf("cgroup.procs: %w", err)
	}
	klog("cgroup memory.max=" + max)
	return nil
}

// watchMemory SIGKILLs the runtime process group once its RSS, or the
// cgroup's memory.current, passes the requested limit. The cgroup is the
// precise kill; this loop is the backstop when that controller is missing,
// so a fast allocator cannot consume the kernel reserve and panic the guest.
func watchMemory(pid int, memoryMB string, stop <-chan struct{}) {
	mb, err := strconv.Atoi(strings.TrimSpace(memoryMB))
	if err != nil || mb < 1 {
		mb = 128
	}
	limitBytes := mb * 1024 * 1024
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			used := cgroupUsage()
			if used == 0 {
				used = treeRSS(pid)
			}
			if used > limitBytes {
				_ = syscall.Kill(-pid, syscall.SIGKILL)
				return
			}
		}
	}
}

func cgroupUsage() int {
	raw, err := os.ReadFile("/run/cgroup/runtime/memory.current")
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0
	}
	return n
}

func treeRSS(pid int) int {
	total := rssBytes(pid)
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return total
	}
	for _, entry := range entries {
		child, err := strconv.Atoi(entry.Name())
		if err != nil || child == pid {
			continue
		}
		if parentOf(child) == pid {
			total += rssBytes(child)
		}
	}
	return total
}

func parentOf(pid int) int {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	// pid (comm) state ppid — comm can contain spaces and parentheses.
	text := string(raw)
	end := strings.LastIndex(text, ")")
	if end < 0 || end+2 >= len(text) {
		return 0
	}
	fields := strings.Fields(text[end+2:])
	if len(fields) < 2 {
		return 0
	}
	ppid, _ := strconv.Atoi(fields[1])
	return ppid
}

func rssBytes(pid int) int {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, _ := strconv.Atoi(fields[1])
		return kb * 1024
	}
	return 0
}

func mount(src, target, fstype string, flags uintptr, data string) error {
	if err := os.MkdirAll(target, 0o755); err != nil && !errors.Is(err, syscall.EROFS) {
		return err
	}
	err := unix.Mount(src, target, fstype, flags, data)
	if err == nil || errors.Is(err, unix.EBUSY) {
		return nil
	}
	return fmt.Errorf("mount %s: %w", target, err)
}

func mountCode() error {
	if err := os.MkdirAll(codeMount, 0o755); err != nil && !errors.Is(err, syscall.EROFS) {
		return err
	}
	var last error
	for i := 0; i < 200; i++ {
		err := unix.Mount(codeDev, codeMount, "ext4", unix.MS_RDONLY, "")
		if err == nil {
			return nil
		}
		last = err
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("mount %s: %w", codeDev, last)
}

func bindTask(src string) error {
	if err := os.MkdirAll(taskMount, 0o755); err != nil && !errors.Is(err, syscall.EROFS) {
		return err
	}
	err := unix.Mount(src, taskMount, "", unix.MS_BIND, "")
	if err != nil {
		// A read-only root sometimes rejects the bind. Remounting the
		// virtio disk read-write fails when the drive is read-only; the
		// error is kept so a failed boot says which call rejected it.
		rwErr := unix.Mount("", "/", "", unix.MS_REMOUNT, "")
		if rwErr != nil {
			return fmt.Errorf("bind /var/task: %w (remount rw: %v)", err, rwErr)
		}
		err = unix.Mount(src, taskMount, "", unix.MS_BIND, "")
		roErr := unix.Mount("", "/", "", unix.MS_REMOUNT|unix.MS_RDONLY, "")
		if err != nil {
			return fmt.Errorf("bind /var/task: %w (remount ro: %v)", err, roErr)
		}
	}
	if err != nil {
		return fmt.Errorf("bind /var/task: %w", err)
	}
	// The code drive is read-only. Say so on the bind too, so a later
	// remount of / cannot widen it.
	_ = unix.Mount("", taskMount, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, "")
	return nil
}

func loadConfig(path string) (fcwire.Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fcwire.Config{}, fmt.Errorf("leyendo %s: %w", path, err)
	}
	var cfg fcwire.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fcwire.Config{}, fmt.Errorf("nimbus.json: %w", err)
	}
	if len(cfg.Argv) == 0 || cfg.Argv[0] == "" {
		return fcwire.Config{}, fmt.Errorf("nimbus.json no trae argv")
	}
	if cfg.UID == 0 {
		cfg.UID = 1000
	}
	if cfg.GID == 0 {
		cfg.GID = 1000
	}
	return cfg, nil
}

func acceptOnce() (*os.File, error) {
	var fd int
	var err error
	for i := 0; i < 200; i++ {
		fd, err = unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		return nil, fmt.Errorf("socket vsock: %w", err)
	}
	unix.CloseOnExec(fd)
	defer unix.Close(fd)

	sa := &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: fcwire.VsockPort}
	if err := unix.Bind(fd, sa); err != nil {
		return nil, fmt.Errorf("bind vsock %d: %w", fcwire.VsockPort, err)
	}
	if err := unix.Listen(fd, 1); err != nil {
		return nil, fmt.Errorf("listen vsock: %w", err)
	}
	klog("vsock listening")
	nfd, _, err := unix.Accept(fd)
	if err != nil {
		return nil, fmt.Errorf("accept vsock: %w", err)
	}
	unix.CloseOnExec(nfd)
	return os.NewFile(uintptr(nfd), "vsock"), nil
}

func supervise(conn *os.File, cfg fcwire.Config) error {
	argv := append([]string(nil), cfg.Argv...)
	path := argv[0]
	if _, err := os.Stat(path); err != nil {
		found, lookErr := exec.LookPath(path)
		if lookErr != nil {
			return fmt.Errorf("no está el bootstrap %s: %w", path, lookErr)
		}
		path = found
		argv[0] = found
	}

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return err
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return err
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		return err
	}

	cmd := exec.Command(path, argv[1:]...)
	cmd.Stdin = stdinR
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	cmd.Dir = taskMount
	cmd.Env = envList(cfg.Env)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uint32(cfg.UID), Gid: uint32(cfg.GID)},
		Setpgid:    true,
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("exec bootstrap: %w", err)
	}
	// oom_score_adj is inherited across exec. Setting it on PID 1 before
	// Start made the runtime unkillable too, and the guest panicked with
	// "no killable processes". Protect init only after the child exists,
	// and mark the child as the victim.
	pid := cmd.Process.Pid
	_ = os.WriteFile("/proc/"+strconv.Itoa(pid)+"/oom_score_adj", []byte("1000\n"), 0o644)
	_ = os.WriteFile("/proc/self/oom_score_adj", []byte("-1000\n"), 0o644)
	memStop := make(chan struct{})
	go watchMemory(pid, cfg.Env["NIMBUS_MEMORY_MB"], memStop)
	if err := limitRuntime(cmd.Process.Pid, cfg.Env["NIMBUS_MEMORY_MB"]); err != nil {
		// No cgroup: the watcher and the global OOM killer are the
		// backstop. A cgroup miss is logged, not fatal.
		klog("cgroup: " + err.Error())
	}
	stdinR.Close()
	stdoutW.Close()
	stderrW.Close()

	var writeMu sync.Mutex
	writeLine := func(line []byte) {
		writeMu.Lock()
		defer writeMu.Unlock()
		if len(line) == 0 || line[len(line)-1] != '\n' {
			line = append(line, '\n')
		}
		_, _ = conn.Write(line)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		forward(stdoutR, writeLine, false)
	}()
	go func() {
		defer wg.Done()
		forward(stderrR, writeLine, true)
	}()
	go func() {
		_, _ = io.Copy(stdinW, conn)
		_ = stdinW.Close()
	}()

	waitErr := cmd.Wait()
	close(memStop)
	stdoutR.Close()
	stderrR.Close()
	wg.Wait()

	code, oom := exitClass(waitErr)
	if code == 0 && !oom {
		// The warm bootstrap loops until its stdin closes. A clean exit
		// means the host hung up, which is how Destroy ends a VM that
		// was still healthy. Nothing to report.
		return nil
	}
	// These two frames live in the binary. json.Marshal here can ask for
	// a page the killer just refused to give out.
	var frame []byte
	switch {
	case oom && code == 134:
		frame = frameOOM134
	case oom:
		frame = frameOOM137
	default:
		msg := "el runtime terminó"
		raw, err := json.Marshal(map[string]any{
			"type":     "fatal",
			"error":    msg,
			"exitCode": code,
		})
		if err != nil {
			frame = frameOOM137
		} else {
			frame = raw
		}
	}
	writeLine(frame)
	// panic=1 makes PID 1 exiting reboot the guest. Firecracker then drops
	// vsock bytes the host has not read, and the invoke sees a bare EOF
	// instead of this frame. Stay until the host closes the socket.
	holdOpen(conn)
	return nil
}

// Prebuilt so an OOM report does not allocate.
var (
	frameOOM137 = []byte("{\"type\":\"fatal\",\"error\":\"oom\",\"exitCode\":137}\n")
	frameOOM134 = []byte("{\"type\":\"fatal\",\"error\":\"oom\",\"exitCode\":134}\n")
)

// Allocated at process start, while the guest still has free pages.
var holdBuf = make([]byte, 256)

func holdOpen(conn *os.File) {
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		if _, err := conn.Read(holdBuf); err != nil {
			return
		}
	}
}

func forward(r io.Reader, writeLine func([]byte), wrap bool) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if wrap {
			frame, err := json.Marshal(map[string]any{
				"type":   "log",
				"stream": "stderr",
				"line":   string(line),
			})
			if err != nil {
				continue
			}
			writeLine(frame)
			continue
		}
		writeLine(append([]byte(nil), line...))
	}
}

func envList(env map[string]string) []string {
	if env == nil {
		env = map[string]string{}
	}
	if _, ok := env["PATH"]; !ok {
		env["PATH"] = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// exitClass maps a wait status to the codes the compute plane already uses.
// SIGKILL and an explicit exit 137 are both the guest OOM killer.
func exitClass(err error) (code int, oom bool) {
	if err == nil {
		return 0, false
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return 1, false
	}
	if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		switch ws.Signal() {
		case syscall.SIGKILL:
			return 137, true
		case syscall.SIGABRT:
			return 134, true
		default:
			return 1, false
		}
	}
	code = exitErr.ExitCode()
	if code < 0 {
		return 1, false
	}
	return code, code == 137 || code == 134
}

func fail(err error) error {
	if err != nil {
		klog(err.Error())
	}
	return err
}

func klog(msg string) {
	f, err := os.OpenFile("/dev/console", os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		fmt.Fprintln(os.Stderr, "nimbus-init:", msg)
		return
	}
	fmt.Fprintln(f, "nimbus-init:", msg)
	f.Close()
}
