// Hello function for the go1.x runtime.
//
// Build a static linux binary named bootstrap and zip that file:
//
//	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bootstrap .
//	zip hello-go.zip bootstrap
//
// The platform bootstrap feeds the event JSON on stdin and reads a single
// JSON value from stdout. Logs go to stderr. A non-zero exit is an error.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"time"
)

func main() {
	event := readEvent()
	mode, _ := event["mode"].(string)
	switch mode {
	case "sleep":
		ms := number(event["ms"])
		if ms <= 0 {
			ms = 5000
		}
		time.Sleep(time.Duration(ms) * time.Millisecond)
		write(map[string]any{"slept": true, "ms": ms})
	case "mem":
		mb := number(event["mb"])
		if mb <= 0 {
			mb = 512
		}
		block := 8 << 20
		target := mb * 1024 * 1024
		var chunks [][]byte
		got := 0
		for got < target {
			buf := make([]byte, block)
			// A fresh page of zeros can stay on the kernel's shared zero
			// page and never hit the cgroup. Store one byte per page.
			for i := 0; i < len(buf); i += 4096 {
				buf[i] = 1
			}
			chunks = append(chunks, buf)
			got += block
		}
		write(map[string]any{"bytes": got})
	case "error":
		msg, _ := event["message"].(string)
		if msg == "" {
			msg = "boom"
		}
		fmt.Fprintln(os.Stderr, msg)
		os.Exit(1)
	case "env":
		write(map[string]any{"greeting": os.Getenv("GREETING")})
	case "probe":
		write(probe())
	default:
		name, _ := event["name"].(string)
		if name == "" {
			name = "mundo"
		}
		fmt.Fprintln(os.Stderr, "hola desde go")
		write(map[string]any{
			"message": "Hola, " + name,
			"runtime": "go1.x",
			"uid":     os.Getuid(),
		})
	}
}

func readEvent() map[string]any {
	raw, _ := io.ReadAll(os.Stdin)
	if len(bytes.TrimSpace(raw)) == 0 {
		if path := os.Getenv("NIMBUS_EVENT_PATH"); path != "" {
			raw, _ = os.ReadFile(path)
		}
	}
	event := map[string]any{}
	if len(bytes.TrimSpace(raw)) > 0 {
		_ = json.Unmarshal(raw, &event)
	}
	return event
}

func number(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	default:
		return 0
	}
}

func write(v any) {
	enc := json.NewEncoder(os.Stdout)
	_ = enc.Encode(v)
}

func probe() map[string]any {
	return map[string]any{
		"uid":      os.Getuid(),
		"traceId":  os.Getenv("NIMBUS_TRACE_ID"),
		"taskWrite": writeFile("/var/task/pwn.txt"),
		"tmpWrite":  writeFile("/tmp/pwn.txt"),
		"net":       dial(),
	}
}

func writeFile(path string) string {
	err := os.WriteFile(path, []byte("x"), 0o644)
	if err != nil {
		return err.Error()
	}
	return "ok"
}

func dial() string {
	conn, err := net.DialTimeout("tcp", "1.1.1.1:443", time.Second)
	if err != nil {
		return err.Error()
	}
	_ = conn.Close()
	return "ok"
}
