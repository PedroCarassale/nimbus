package firecracker

import (
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PedroCarassale/nimbus/compute-plane/internal/cancel"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/warm"
)

func TestInvokeDoneAndLog(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	sess := newSession("vm1", server, func() {})
	go func() {
		br := newReader(client)
		line, _ := br.ReadString('\n')
		if !strings.Contains(line, `"type":"invoke"`) || !strings.Contains(line, `"executionId":"e1"`) {
			t.Errorf("invoke frame %s", line)
		}
		_, _ = io.WriteString(client, `{"type":"log","stream":"stdout","executionId":"e1","line":"hola"}`+"\n")
		_, _ = io.WriteString(client, `{"type":"done","executionId":"e1","ok":true,"output":{"message":"Hola"},"exitCode":0}`+"\n")
	}()
	var logs []string
	out := sess.Invoke(context.Background(), warm.Invocation{
		ExecutionID: "e1",
		Event:       map[string]interface{}{"name": "Ada"},
		Log: func(stream, line string) {
			logs = append(logs, stream+":"+line)
		},
	})
	if !out.Success || out.ExitCode != 0 {
		t.Fatalf("%+v", out)
	}
	msg, _ := out.Output.(map[string]interface{})
	if msg["message"] != "Hola" {
		t.Fatalf("output %#v", out.Output)
	}
	if len(logs) != 1 || logs[0] != "stdout:hola" {
		t.Fatalf("logs %#v", logs)
	}
}

func TestInvokeFatalIsOOM(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	sess := newSession("vm1", server, func() {})
	go func() {
		br := newReader(client)
		_, _ = br.ReadString('\n')
		_, _ = io.WriteString(client, `{"type":"fatal","error":"oom","exitCode":137}`+"\n")
	}()
	out := sess.Invoke(context.Background(), warm.Invocation{ExecutionID: "e1", Event: map[string]interface{}{}})
	if !out.OOM || !out.Tainted || out.ExitCode != 137 || out.Error != "" {
		t.Fatalf("%+v", out)
	}
}

func TestInvokeCancelKills(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	var killed bool
	var mu sync.Mutex
	sess := newSession("vm1", server, func() {
		mu.Lock()
		killed = true
		mu.Unlock()
		client.Close()
	})
	ctx, stop := context.WithCancelCause(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		stop(cancel.ErrRun)
	}()
	// Nobody reads the other end of the pipe. The write blocks, and cancel
	// has to unblock it by destroying the session.
	out := sess.Invoke(ctx, warm.Invocation{ExecutionID: "e1", Event: map[string]interface{}{}})
	if out.ExitCode != cancel.ExitCode || out.Error != cancel.Message || !out.Tainted {
		t.Fatalf("%+v", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if !killed {
		t.Fatal("vm was not killed")
	}
}

func TestReadyThenInvoke(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	sess := newSession("vm1", server, func() {})
	go func() {
		_, _ = io.WriteString(client, `{"type":"ready","initMs":1}`+"\n")
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := sess.waitReady(ctx); err != nil {
		t.Fatal(err)
	}
	if sess.InitMs() < 1 {
		t.Fatalf("init %d", sess.InitMs())
	}
}

func newReader(c net.Conn) *jsonReader {
	return &jsonReader{c: c}
}

type jsonReader struct{ c net.Conn }

func (r *jsonReader) ReadString(delim byte) (string, error) {
	var buf []byte
	tmp := make([]byte, 1)
	for {
		n, err := r.c.Read(tmp)
		if n == 1 {
			buf = append(buf, tmp[0])
			if tmp[0] == delim {
				return string(buf), nil
			}
		}
		if err != nil {
			return string(buf), err
		}
	}
}
