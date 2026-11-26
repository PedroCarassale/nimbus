package firecracker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/PedroCarassale/nimbus/compute-plane/internal/cancel"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/warm"
)

type frame struct {
	Type        string          `json:"type"`
	ExecutionID string          `json:"executionId"`
	Stream      string          `json:"stream"`
	Line        string          `json:"line"`
	OK          bool            `json:"ok"`
	Output      json.RawMessage `json:"output"`
	Error       string          `json:"error"`
	ExitCode    int             `json:"exitCode"`
}

// Session is one booted microVM. Invoke sends one JSON line and reads
// until done, the same frames a warm container writes on stdout.
type Session struct {
	mu        sync.Mutex
	id        string
	conn      io.ReadWriteCloser
	frames    chan frame
	dying     chan struct{}
	closeOnce sync.Once
	readErr   error
	initMs    int64
	readyFrom time.Time
	stop      func()
	notes     func() string
	destroyed bool
	stderrBuf bytes.Buffer
}

func newSession(id string, conn io.ReadWriteCloser, stop func()) *Session {
	s := &Session{
		id:     id,
		conn:   conn,
		frames: make(chan frame, 256),
		dying:  make(chan struct{}),
		stop:   stop,
	}
	go s.readLoop()
	return s
}

func (s *Session) ID() string { return s.id }

func (s *Session) InitMs() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.initMs
}

func (s *Session) waitReady(ctx context.Context) error {
	// readyFrom is set by Start to the moment boot began, so initMs includes
	// the code drive and the microVM, the same window Docker counts from
	// docker run until the ready frame. Tests that skip Start measure only
	// the wait.
	started := s.readyFrom
	if started.IsZero() {
		started = time.Now()
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case fr, ok := <-s.frames:
			if !ok {
				return fmt.Errorf("el runtime cerró antes de estar listo: %s", s.stderrText())
			}
			switch fr.Type {
			case "ready":
				ms := time.Since(started).Milliseconds()
				if ms < 1 {
					ms = 1
				}
				s.mu.Lock()
				s.initMs = ms
				s.mu.Unlock()
				return nil
			case "fatal":
				if fr.Error == "" {
					return fmt.Errorf("el runtime no pudo arrancar")
				}
				return fmt.Errorf("%s", fr.Error)
			case "log":
				log.Printf("[firecracker] startup %s: %s", fr.Stream, fr.Line)
			default:
				return fmt.Errorf("frame inesperado durante el arranque: %s", fr.Type)
			}
		}
	}
}

func (s *Session) Invoke(ctx context.Context, inv warm.Invocation) warm.Outcome {
	s.mu.Lock()
	if s.destroyed {
		s.mu.Unlock()
		return warm.Outcome{ExitCode: 1, Tainted: true, Error: "El microVM ya no existe"}
	}
	conn := s.conn
	s.mu.Unlock()

	event := inv.Event
	if event == nil {
		event = map[string]interface{}{}
	}
	body := map[string]interface{}{
		"type":        "invoke",
		"executionId": inv.ExecutionID,
		"event":       event,
	}
	if inv.TraceID != "" {
		body["traceId"] = inv.TraceID
	}
	if inv.TraceParent != "" {
		body["traceparent"] = inv.TraceParent
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return warm.Outcome{ExitCode: 1, Tainted: true, Error: "No se pudo armar el pedido"}
	}
	// A blocked write (guest not reading, or a test pipe with no reader)
	// must still honor cancel and timeout. Closing the conn unblocks it.
	writeErr := make(chan error, 1)
	go func() {
		_, werr := conn.Write(append(payload, '\n'))
		writeErr <- werr
	}()
	select {
	case <-ctx.Done():
		return s.stoppedOutcome(ctx, bytes.Buffer{}, bytes.Buffer{})
	case err := <-writeErr:
		if err != nil {
			if ctx.Err() != nil {
				return s.stoppedOutcome(ctx, bytes.Buffer{}, bytes.Buffer{})
			}
			return s.closedOutcome()
		}
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	for {
		select {
		case <-ctx.Done():
			return s.stoppedOutcome(ctx, stdoutBuf, stderrBuf)
		case fr, ok := <-s.frames:
			if !ok {
				if cancel.Is(ctx) {
					_ = s.Destroy(context.Background())
					return warm.Outcome{ExitCode: cancel.ExitCode, Error: cancel.Message, Tainted: true, Stdout: stdoutBuf.String(), Stderr: stderrBuf.String()}
				}
				out := s.closedOutcome()
				out.Stdout = stdoutBuf.String() + out.Stdout
				if out.Stderr == "" {
					out.Stderr = stderrBuf.String()
				}
				return out
			}
			if fr.ExecutionID != "" && inv.ExecutionID != "" && fr.ExecutionID != inv.ExecutionID {
				return warm.Outcome{ExitCode: 1, Tainted: true, Error: "El runtime mezcló dos ejecuciones", Stdout: stdoutBuf.String(), Stderr: stderrBuf.String()}
			}
			switch fr.Type {
			case "log":
				line := fr.Line
				stream := fr.Stream
				if stream != "stderr" {
					stream = "stdout"
				}
				if stream == "stderr" {
					stderrBuf.WriteString(line + "\n")
				} else {
					stdoutBuf.WriteString(line + "\n")
				}
				if inv.Log != nil {
					inv.Log(stream, line)
				}
			case "done":
				var output interface{}
				if len(fr.Output) > 0 && string(fr.Output) != "null" {
					_ = json.Unmarshal(fr.Output, &output)
				}
				out := warm.Outcome{
					Success:  fr.OK && fr.ExitCode == 0,
					Output:   output,
					Error:    fr.Error,
					ExitCode: fr.ExitCode,
					Stdout:   stdoutBuf.String(),
					Stderr:   stderrBuf.String(),
				}
				if fr.ExitCode == 137 || fr.ExitCode == 134 {
					out.Success = false
					out.OOM = true
					out.Tainted = true
					out.Error = ""
				}
				return out
			case "fatal":
				code := fr.ExitCode
				if code == 0 {
					code = 1
				}
				oom := code == 137 || code == 134 || strings.Contains(strings.ToLower(fr.Error), "oom")
				out := warm.Outcome{
					ExitCode: code,
					OOM:      oom,
					Tainted:  true,
					Stdout:   stdoutBuf.String(),
					Stderr:   stderrBuf.String(),
				}
				if !oom {
					out.Error = fr.Error
				}
				return out
			}
		}
	}
}

func (s *Session) stoppedOutcome(ctx context.Context, stdoutBuf, stderrBuf bytes.Buffer) warm.Outcome {
	out := warm.Outcome{Stdout: stdoutBuf.String(), Stderr: stderrBuf.String(), Tainted: true, ExitCode: 1}
	// Kill before returning so a sleep cannot finish and the pool
	// cannot put this VM back.
	_ = s.Destroy(context.Background())
	if cancel.Is(ctx) {
		out.ExitCode = cancel.ExitCode
		out.Error = cancel.Message
	} else if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		out.ExitCode = 124
	} else {
		out.Error = "La ejecución fue cancelada"
	}
	return out
}

func (s *Session) Destroy(context.Context) error {
	s.mu.Lock()
	if s.destroyed {
		s.mu.Unlock()
		return nil
	}
	s.destroyed = true
	if s.conn != nil {
		_ = s.conn.Close()
	}
	select {
	case <-s.dying:
	default:
		close(s.dying)
	}
	stop := s.stop
	s.mu.Unlock()
	if stop != nil {
		stop()
	}
	return nil
}

func (s *Session) readLoop() {
	defer s.finish(io.EOF)
	sc := bufio.NewScanner(s.conn)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var fr frame
		if err := json.Unmarshal(line, &fr); err != nil {
			s.mu.Lock()
			s.readErr = fmt.Errorf("protocolo: %w", err)
			s.mu.Unlock()
			return
		}
		select {
		case s.frames <- fr:
		case <-s.dying:
			return
		}
	}
	if err := sc.Err(); err != nil {
		s.mu.Lock()
		if s.readErr == nil {
			s.readErr = err
		}
		s.mu.Unlock()
	}
}

func (s *Session) finish(err error) {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		if s.readErr == nil {
			s.readErr = err
		}
		s.mu.Unlock()
		close(s.frames)
	})
}

func (s *Session) stderrText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.TrimSpace(s.stderrBuf.String())
}

func (s *Session) closedOutcome() warm.Outcome {
	s.mu.Lock()
	extra := s.stderrBuf.String()
	errText := ""
	if s.readErr != nil {
		errText = s.readErr.Error()
	}
	s.mu.Unlock()
	if strings.TrimSpace(extra) == "" {
		extra = "El microVM cerró la conexión\n"
	}
	if errText != "" {
		extra = strings.TrimSpace(extra + " " + errText)
	}
	if s.notes != nil {
		if tail := strings.TrimSpace(s.notes()); tail != "" {
			extra = strings.TrimSpace(extra + "\n" + tail)
		}
	}
	return warm.Outcome{ExitCode: 1, Tainted: true, Stderr: extra}
}
