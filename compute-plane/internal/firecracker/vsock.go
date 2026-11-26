package firecracker

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/PedroCarassale/nimbus/compute-plane/internal/fcwire"
)

// prefixConn keeps bytes the handshake buffered past the OK line.
// The guest can write "ready" before the host finishes reading OK.
type prefixConn struct {
	net.Conn
	r *bufio.Reader
}

func (p *prefixConn) Read(b []byte) (int, error) {
	return p.r.Read(b)
}

// Dial connects to the Firecracker vsock unix socket and asks for the
// guest port. It retries until ctx ends: the socket appears only after
// the VM is configured, and the guest accepts only after init runs.
func Dial(ctx context.Context, uds string) (net.Conn, error) {
	var last error
	for {
		if err := ctx.Err(); err != nil {
			if last != nil {
				return nil, fmt.Errorf("vsock: %w (último: %v)", err, last)
			}
			return nil, err
		}
		conn, err := (&net.Dialer{Timeout: 200 * time.Millisecond}).DialContext(ctx, "unix", uds)
		if err != nil {
			last = err
			sleep(ctx, 20*time.Millisecond)
			continue
		}
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		if _, err := fmt.Fprintf(conn, "CONNECT %d\n", fcwire.VsockPort); err != nil {
			conn.Close()
			last = err
			sleep(ctx, 20*time.Millisecond)
			continue
		}
		br := bufio.NewReader(conn)
		line, err := br.ReadString('\n')
		if err != nil || !strings.HasPrefix(line, "OK") {
			conn.Close()
			last = fmt.Errorf("respuesta %q: %v", strings.TrimSpace(line), err)
			sleep(ctx, 20*time.Millisecond)
			continue
		}
		_ = conn.SetDeadline(time.Time{})
		return &prefixConn{Conn: conn, r: br}, nil
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
