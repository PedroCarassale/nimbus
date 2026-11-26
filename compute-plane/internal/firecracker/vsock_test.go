package firecracker

import (
	"bufio"
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/PedroCarassale/nimbus/compute-plane/internal/fcwire"
)

func TestDialHandshake(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "vsock.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		br := bufio.NewReader(c)
		line, _ := br.ReadString('\n')
		if line != "CONNECT 5000\n" && line != "CONNECT "+itoa(fcwire.VsockPort)+"\n" {
			return
		}
		_, _ = c.Write([]byte("OK 5000\n"))
		_, _ = c.Write([]byte("{\"type\":\"ready\"}\n"))
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := Dial(ctx, sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil || line != "{\"type\":\"ready\"}\n" {
		t.Fatalf("ready %q %v", line, err)
	}
}

func itoa(n uint32) string {
	return string(rune('0'+n/1000)) + string(rune('0'+(n/100)%10)) + string(rune('0'+(n/10)%10)) + string(rune('0'+n%10))
}
