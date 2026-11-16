// Command bootstrap is the Go runtime entrypoint.
//
// The user zip carries a static linux binary (provided-style). This process
// does not compile anything. It speaks the same line protocol as the Node
// bootstrap and, for each invocation, execs that binary:
//
//	stdin  = event JSON
//	stdout = result JSON
//	stderr = logs
//	exit 0 = success
//
// One-shot copies the child's stdout and stderr through and exits with the
// child's code. Warm keeps this process up and turns each child into log
// frames plus one done frame. The child does not stay resident: a warm hit
// skips docker run, not the user process.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

const maxCapture = 1 << 20

func main() {
	if os.Getenv("NIMBUS_WARM") == "1" {
		if err := serve(); err != nil {
			fatal(err)
		}
		return
	}
	code := runOnce()
	os.Exit(code)
}

func binaryPath() (string, error) {
	path := strings.TrimSpace(os.Getenv("NIMBUS_HANDLER_PATH"))
	if path == "" {
		path = "/var/task/bootstrap"
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return "", fmt.Errorf("binario go no encontrado: %s", path)
	}
	return path, nil
}

func eventBytes() ([]byte, error) {
	path := os.Getenv("NIMBUS_EVENT_PATH")
	if path == "" {
		path = "/var/task/event.json"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []byte("{}"), nil
		}
		return nil, fmt.Errorf("error leyendo evento: %w", err)
	}
	return raw, nil
}

func runOnce() int {
	bin, err := binaryPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[nimbus] %s\n", err)
		return 1
	}
	event, err := eventBytes()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[nimbus] %s\n", err)
		return 1
	}
	traceID := os.Getenv("NIMBUS_TRACE_ID")
	traceparent := os.Getenv("TRACEPARENT")
	if traceID != "" || traceparent != "" {
		fmt.Fprintf(os.Stderr, "[nimbus] traceId=%s traceparent=%s\n", traceID, traceparent)
	}
	cmd := exec.Command(bin)
	cmd.Stdin = bytes.NewReader(event)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	return childExit(cmd.Run())
}

// childExit maps a child status to the code the compute plane already
// understands. A cgroup OOM sends SIGKILL to the binary, not to this
// process. Exit 137 lets the cold path classify it as OOM. Exit 134 is
// SIGABRT, the same code Node uses for a heap abort.
func childExit(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		fmt.Fprintf(os.Stderr, "[nimbus] %s\n", err)
		return 1
	}
	if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		switch ws.Signal() {
		case syscall.SIGKILL:
			return 137
		case syscall.SIGABRT:
			return 134
		default:
			return 1
		}
	}
	code := exitErr.ExitCode()
	if code < 0 {
		return 1
	}
	return code
}

type invokeMsg struct {
	ExecutionID string          `json:"executionId"`
	Event       json.RawMessage `json:"event"`
	TraceID     string          `json:"traceId"`
	TraceParent string          `json:"traceparent"`
}

func serve() error {
	bin, err := binaryPath()
	if err != nil {
		return err
	}
	started := time.Now()
	emit(map[string]any{"type": "ready", "initMs": time.Since(started).Milliseconds()})

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), maxCapture)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		handle(bin, line)
	}
	return sc.Err()
}

func handle(bin, line string) {
	var msg invokeMsg
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		emit(map[string]any{
			"type":        "done",
			"executionId": "",
			"ok":          false,
			"output":      nil,
			"error":       "Pedido warm inválido: " + err.Error(),
			"exitCode":    1,
		})
		return
	}
	event := msg.Event
	if len(bytes.TrimSpace(event)) == 0 || string(event) == "null" {
		event = []byte("{}")
	}
	if msg.TraceID != "" {
		_ = os.Setenv("NIMBUS_TRACE_ID", msg.TraceID)
	}
	if msg.TraceParent != "" {
		_ = os.Setenv("TRACEPARENT", msg.TraceParent)
	}
	if msg.TraceID != "" || msg.TraceParent != "" {
		emitLog(msg.ExecutionID, "stderr", fmt.Sprintf("[nimbus] traceId=%s traceparent=%s", msg.TraceID, msg.TraceParent))
	}

	cmd := exec.Command(bin)
	cmd.Stdin = bytes.NewReader(event)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &limitedWriter{buf: &stdout, limit: maxCapture}
	cmd.Stderr = &limitedWriter{buf: &stderr, limit: maxCapture}
	cmd.Env = os.Environ()
	err := cmd.Run()
	code := childExit(err)
	if code == 137 || code == 134 {
		// The container has to die. A done frame would look like a handler
		// error and the warm pool would keep a process that just lost a child
		// to the OOM killer.
		os.Exit(code)
	}
	if err != nil && code == 1 && stderr.Len() == 0 {
		stderr.WriteString(err.Error())
	}
	for _, ln := range splitLines(stderr.String()) {
		emitLog(msg.ExecutionID, "stderr", ln)
	}
	if code != 0 {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = fmt.Sprintf("el binario terminó con exit %d", code)
		}
		emit(map[string]any{
			"type":        "done",
			"executionId": msg.ExecutionID,
			"ok":          false,
			"output":      nil,
			"error":       message,
			"exitCode":    code,
		})
		return
	}
	rawOut := bytes.TrimSpace(stdout.Bytes())
	var output any
	if len(rawOut) == 0 {
		output = nil
	} else if err := json.Unmarshal(rawOut, &output); err != nil {
		emit(map[string]any{
			"type":        "done",
			"executionId": msg.ExecutionID,
			"ok":          false,
			"output":      nil,
			"error":       "El binario no escribió un JSON: " + err.Error(),
			"exitCode":    1,
		})
		return
	}
	emit(map[string]any{
		"type":        "done",
		"executionId": msg.ExecutionID,
		"ok":          true,
		"output":      output,
		"error":       "",
		"exitCode":    0,
	})
}

func splitLines(text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if text == "" {
		return nil
	}
	parts := strings.Split(text, "\n")
	var out []string
	for _, part := range parts {
		if part == "" && len(out) > 0 {
			continue
		}
		if part == "" {
			continue
		}
		out = append(out, part)
	}
	return out
}

func emitLog(executionID, stream, line string) {
	emit(map[string]any{
		"type":        "log",
		"stream":      stream,
		"executionId": executionID,
		"line":        line,
	})
}

func emit(obj map[string]any) {
	raw, err := json.Marshal(obj)
	if err != nil {
		return
	}
	raw = append(raw, '\n')
	_, _ = os.Stdout.Write(raw)
}

func fatal(err error) {
	emit(map[string]any{
		"type":        "fatal",
		"error":       err.Error(),
		"executionId": "",
	})
	os.Exit(1)
}

// limitedWriter stops a noisy child from filling the runtime process.
type limitedWriter struct {
	buf   *bytes.Buffer
	limit int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if w.buf.Len() >= w.limit {
		return len(p), nil
	}
	remain := w.limit - w.buf.Len()
	if len(p) > remain {
		p = p[:remain]
	}
	_, _ = w.buf.Write(p)
	return len(p), nil
}

// Compile-time check that the limited writer is an io.Writer.
var _ io.Writer = (*limitedWriter)(nil)
