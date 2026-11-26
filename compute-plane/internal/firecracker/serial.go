package firecracker

import (
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// attachSerial points the VMM's stdout at a pty when one is available.
// Firecracker is a static binary: a regular file is fully buffered, and
// SIGKILL on the way down drops the guest console lines we need (OOM,
// panic). A pty is line-buffered, and this process copies it to path.
func attachSerial(cmd *exec.Cmd, path string) (afterStart func(), err error) {
	logf, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	master, slave, ptyErr := openPTY()
	if ptyErr != nil {
		cmd.Stdout = logf
		cmd.Stderr = logf
		return func() { _ = logf.Close() }, nil
	}
	cmd.Stdout = slave
	cmd.Stderr = slave
	return func() {
		_ = slave.Close()
		go func() {
			_, _ = io.Copy(logf, master)
			_ = logf.Close()
			_ = master.Close()
		}()
	}, nil
}

func openPTY() (master, slave *os.File, err error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, nil, err
	}
	var n uint32
	if err := ioctl(m.Fd(), unix.TIOCGPTN, uintptr(unsafe.Pointer(&n))); err != nil {
		_ = m.Close()
		return nil, nil, err
	}
	unlock := uint32(0)
	if err := ioctl(m.Fd(), unix.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); err != nil {
		_ = m.Close()
		return nil, nil, err
	}
	s, err := os.OpenFile("/dev/pts/"+strconv.FormatUint(uint64(n), 10), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		_ = m.Close()
		return nil, nil, err
	}
	return m, s, nil
}

// waitTail gives the pty copier a moment to drain the VMM's last lines
// after the process has exited. Reading the file immediately sees nothing.
func waitTail(path string) string {
	var extra string
	for i := 0; i < 25; i++ {
		extra = tailFile(path, 800)
		if strings.TrimSpace(extra) != "" {
			return extra
		}
		time.Sleep(20 * time.Millisecond)
	}
	return extra
}

func ioctl(fd uintptr, req uint, arg uintptr) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, fd, uintptr(req), arg)
	if errno != 0 {
		return errno
	}
	return nil
}
