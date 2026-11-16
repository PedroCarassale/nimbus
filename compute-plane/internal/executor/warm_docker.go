package executor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/PedroCarassale/nimbus/compute-plane/internal/fnconfig"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/warm"
)

// dockerStarter keeps one container process attached to stdin/stdout.
// The bootstrap speaks a line-delimited JSON protocol on that pipe.
// network=none makes a port inside the container unreachable, so the
// pipe is the channel: one process, no extra socket, no published port.
type dockerStarter struct {
	image string
}

func (d dockerStarter) Start(ctx context.Context, spec warm.Spec) (warm.Session, error) {
	image := spec.Image
	if image == "" {
		image = d.image
	}
	name := warmContainerName()
	args := withExtraEnv(dockerWarmArgs(name, spec.WorkDir, image, spec.Config, spec.FunctionID, spec.Version), specTraceEnv(spec))
	log.Printf("[executor] docker %s", formatArgsForLog(args))

	sess, err := launchWarm(name, args)
	if err != nil {
		return nil, err
	}
	if err := sess.waitReady(ctx); err != nil {
		_ = sess.Destroy(context.Background())
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	return sess, nil
}

// dockerWarmArgs is the long-lived form of dockerRunArgs.
// The container stays up (--name, no --rm) and reads invocations from
// stdin (-i). Isolation matches the cold path, and the process runs as
// the image's unprivileged node user.
func dockerWarmArgs(name, workDir, image string, cfg fnconfig.Resolved, functionID, version string) []string {
	args := []string{
		"run", "-i",
		"--name", name,
		"--label", "nimbus.warm=1",
		"--label", "nimbus.functionId=" + labelValue(functionID),
		"--label", "nimbus.version=" + labelValue(version),
		fmt.Sprintf("--memory=%dm", cfg.MemoryMB),
		fmt.Sprintf("--memory-swap=%dm", cfg.MemoryMB),
		"--pids-limit=256",
		"--network=none",
		"--read-only",
		"--tmpfs", "/tmp:rw,nosuid,size=16m",
		"--user", "node",
	}
	if cfg.CPUsFlag != "" {
		args = append(args, "--cpus="+cfg.CPUsFlag)
	}
	args = append(args, "-v", fmt.Sprintf("%s:/var/task:ro", workDir))
	for _, ev := range cfg.Env {
		args = append(args, "-e", ev.Name+"="+ev.Value)
	}
	args = append(args,
		"-e", "NIMBUS_WARM=1",
		"-e", "HOME=/tmp",
		"-e", "NIMBUS_HANDLER="+cfg.Handler,
		"-e", "NIMBUS_HANDLER_FUNCTION="+cfg.HandlerExport,
		"-e", "NIMBUS_HANDLER_PATH=/var/task/"+cfg.HandlerFile+".js",
		"-e", fmt.Sprintf("NIMBUS_MEMORY_MB=%d", cfg.MemoryMB),
		"-e", fmt.Sprintf("NIMBUS_TIMEOUT_SEC=%d", cfg.TimeoutSec),
		image,
	)
	return args
}

func specTraceEnv(spec warm.Spec) []string {
	var env []string
	if spec.TraceID != "" {
		env = append(env, "NIMBUS_TRACE_ID="+spec.TraceID)
	}
	if spec.TraceParent != "" {
		env = append(env, "TRACEPARENT="+spec.TraceParent)
	}
	return env
}

func warmContainerName() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("nimbus-w-%d", time.Now().UnixNano())
	}
	return "nimbus-w-" + hex.EncodeToString(b[:])
}

type warmFrame struct {
	Type        string          `json:"type"`
	ExecutionID string          `json:"executionId"`
	Stream      string          `json:"stream"`
	Line        string          `json:"line"`
	OK          bool            `json:"ok"`
	Output      json.RawMessage `json:"output"`
	Error       string          `json:"error"`
	ExitCode    int             `json:"exitCode"`
	InitMs      int64           `json:"initMs"`
}

type dockerWarmSession struct {
	mu        sync.Mutex
	baseName  string
	current   string
	stdin     io.WriteCloser
	frames    chan warmFrame
	dying     chan struct{}
	readErr   error
	closeOnce sync.Once
	started   time.Time
	initMs    int64
	destroyed bool
	stderrBuf bytes.Buffer
	activeLog func(stream, line string)
}

func launchWarm(name string, args []string) (*dockerWarmSession, error) {
	cmd := exec.Command("docker", args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	sess := &dockerWarmSession{
		baseName: name,
		current:  name,
		stdin:    stdin,
		frames:   make(chan warmFrame, 256),
		dying:    make(chan struct{}),
		started:  time.Now(),
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("error iniciando container warm: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	go sess.readStdout(stdout)
	go sess.readStderr(stderr)
	return sess, nil
}

func (s *dockerWarmSession) ID() string { return s.baseName }

func (s *dockerWarmSession) InitMs() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.initMs
}

func (s *dockerWarmSession) waitReady(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case frame, ok := <-s.frames:
			if !ok {
				return fmt.Errorf("el runtime cerró antes de estar listo: %v", s.stderrText())
			}
			switch frame.Type {
			case "ready":
				s.mu.Lock()
				s.initMs = time.Since(s.started).Milliseconds()
				if s.initMs < 1 {
					s.initMs = 1
				}
				s.mu.Unlock()
				return nil
			case "fatal":
				if frame.Error == "" {
					return fmt.Errorf("el runtime warm no pudo arrancar")
				}
				return fmt.Errorf("%s", frame.Error)
			case "log":
				log.Printf("[executor] warm startup %s: %s", frame.Stream, frame.Line)
			default:
				return fmt.Errorf("frame inesperado durante el arranque: %s", frame.Type)
			}
		}
	}
}

func (s *dockerWarmSession) Invoke(ctx context.Context, inv warm.Invocation) warm.Outcome {
	s.bind(inv.ExecutionID)
	defer s.unbind()

	s.mu.Lock()
	if s.destroyed {
		s.mu.Unlock()
		return warm.Outcome{ExitCode: 1, Tainted: true, Error: "El container warm ya no existe"}
	}
	s.activeLog = inv.Log
	stdin := s.stdin
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.activeLog = nil
		s.mu.Unlock()
	}()

	event := inv.Event
	if event == nil {
		event = map[string]interface{}{}
	}
	frame := map[string]interface{}{
		"type":        "invoke",
		"executionId": inv.ExecutionID,
		"event":       event,
	}
	if inv.TraceID != "" {
		frame["traceId"] = inv.TraceID
	}
	if inv.TraceParent != "" {
		frame["traceparent"] = inv.TraceParent
	}
	payload, err := json.Marshal(frame)
	if err != nil {
		return warm.Outcome{ExitCode: 1, Tainted: true, Error: "No se pudo armar el pedido warm"}
	}
	if _, err := stdin.Write(append(payload, '\n')); err != nil {
		return s.closedOutcome()
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	for {
		select {
		case <-ctx.Done():
			out := warm.Outcome{
				Stdout:   stdoutBuf.String(),
				Stderr:   stderrBuf.String(),
				Tainted:  true,
				ExitCode: 1,
			}
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				out.ExitCode = 124
			} else {
				out.Error = "La ejecución fue cancelada"
			}
			return out
		case frame, ok := <-s.frames:
			if !ok {
				out := s.closedOutcome()
				if stdoutBuf.Len() > 0 {
					out.Stdout = stdoutBuf.String() + out.Stdout
				}
				if stderrBuf.Len() > 0 && out.Stderr == "" {
					out.Stderr = stderrBuf.String()
				}
				return out
			}
			if frame.ExecutionID != "" && inv.ExecutionID != "" && frame.ExecutionID != inv.ExecutionID {
				return warm.Outcome{
					ExitCode: 1,
					Tainted:  true,
					Error:    "El runtime warm mezcló dos ejecuciones",
					Stdout:   stdoutBuf.String(),
					Stderr:   stderrBuf.String(),
				}
			}
			switch frame.Type {
			case "log":
				line := frame.Line
				stream := frame.Stream
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
				if len(frame.Output) > 0 {
					_ = json.Unmarshal(frame.Output, &output)
				}
				return warm.Outcome{
					Success:  frame.OK && frame.ExitCode == 0,
					Output:   output,
					ExitCode: frame.ExitCode,
					Stdout:   stdoutBuf.String(),
					Stderr:   stderrBuf.String(),
				}
			case "fatal":
				return warm.Outcome{
					ExitCode: 1,
					Tainted:  true,
					Error:    frame.Error,
					Stdout:   stdoutBuf.String(),
					Stderr:   stderrBuf.String(),
				}
			}
		}
	}
}

func (s *dockerWarmSession) Destroy(context.Context) error {
	s.mu.Lock()
	if s.destroyed {
		s.mu.Unlock()
		return nil
	}
	s.destroyed = true
	name := s.current
	if s.stdin != nil {
		_ = s.stdin.Close()
	}
	select {
	case <-s.dying:
	default:
		close(s.dying)
	}
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "rm", "-f", name)
	if err := cmd.Run(); err != nil {
		log.Printf("[executor] no se pudo borrar el container warm %s: %v", name, err)
		return err
	}
	return nil
}

func (s *dockerWarmSession) bind(executionID string) {
	alias := containerName(executionID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.destroyed || alias == "" || alias == s.current {
		return
	}
	from := s.current
	if err := renameContainer(from, alias); err != nil {
		log.Printf("[executor] rename %s -> %s: %v", from, alias, err)
		return
	}
	s.current = alias
}

func (s *dockerWarmSession) unbind() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.destroyed || s.current == s.baseName {
		return
	}
	from := s.current
	if err := renameContainer(from, s.baseName); err != nil {
		log.Printf("[executor] rename %s -> %s: %v", from, s.baseName, err)
		return
	}
	s.current = s.baseName
}

func renameContainer(from, to string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "docker", "rename", from, to).Run()
}

func (s *dockerWarmSession) readStdout(r io.Reader) {
	defer s.finishRead(io.EOF)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var frame warmFrame
		if err := json.Unmarshal(line, &frame); err != nil {
			s.mu.Lock()
			s.readErr = fmt.Errorf("protocolo warm: %w", err)
			s.mu.Unlock()
			return
		}
		select {
		case s.frames <- frame:
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

func (s *dockerWarmSession) readStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		s.mu.Lock()
		s.stderrBuf.WriteString(line + "\n")
		logFn := s.activeLog
		s.mu.Unlock()
		if logFn != nil {
			logFn("stderr", line)
		}
	}
}

func (s *dockerWarmSession) finishRead(err error) {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.readErr = err
		s.mu.Unlock()
		close(s.frames)
	})
}

func (s *dockerWarmSession) stderrText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.TrimSpace(s.stderrBuf.String())
}

func (s *dockerWarmSession) closedOutcome() warm.Outcome {
	s.mu.Lock()
	name := s.current
	extra := s.stderrBuf.String()
	s.mu.Unlock()

	oom, code, found := inspectContainer(name)
	if !found {
		code = 1
	}
	text := strings.ToLower(extra)
	heap := strings.Contains(text, "javascript heap out of memory") || strings.Contains(text, "reached heap limit")
	if oom || code == 137 || code == 134 || heap {
		if code == 0 || code == 1 {
			if heap && !oom {
				code = 134
			} else {
				code = 137
			}
		}
		return warm.Outcome{
			ExitCode: code,
			OOM:      true,
			Tainted:  true,
			Stderr:   extra,
		}
	}
	if code == 0 {
		code = 1
	}
	if strings.TrimSpace(extra) == "" {
		extra = "El runtime warm terminó de forma inesperada\n"
	}
	return warm.Outcome{
		ExitCode: code,
		Tainted:  true,
		Stderr:   extra,
	}
}

func inspectContainer(name string) (oom bool, exitCode int, found bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.State.OOMKilled}} {{.State.ExitCode}}", name)
	out, err := cmd.Output()
	if err != nil {
		return false, 1, false
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 {
		return false, 1, false
	}
	code := 1
	fmt.Sscanf(fields[1], "%d", &code)
	return fields[0] == "true", code, true
}
