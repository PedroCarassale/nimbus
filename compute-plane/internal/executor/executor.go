package executor

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/PedroCarassale/nimbus/compute-plane/internal/fnconfig"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/logs"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/warm"
	"github.com/redis/go-redis/v9"
)

// artifactDownloader fetches a function zip into a local directory.
// The returned directory is owned by the caller and contains code.zip.
type artifactDownloader interface {
	DownloadArtifact(ctx context.Context, functionID, version string) (string, error)
}

type Executor struct {
	store        artifactDownloader
	rdb          *redis.Client
	defaultImage string
	pool         *warm.Pool

	codeMu    sync.Mutex
	codeDirs  map[string]string
	codeLocks map[string]*sync.Mutex
}

type ExecutionRequest struct {
	ExecutionID string                 `json:"executionId"`
	FunctionID  string                 `json:"functionId"`
	Version     string                 `json:"version"`
	Event       map[string]interface{} `json:"event"`
	Config      fnconfig.Resolved      `json:"-"`
}

type ExecutionResult struct {
	ExecutionID string      `json:"executionId"`
	Success     bool        `json:"success"`
	Output      interface{} `json:"output"`
	Error       string      `json:"error,omitempty"`
	ExitCode    int         `json:"exitCode"`
	DurationMs  int64       `json:"durationMs"`
	// ColdStart is true when this invocation started a new container.
	// A warm hit reports false. Clients that ignore unknown fields are unchanged.
	ColdStart bool `json:"coldStart"`
	// InitMs is how long the new container took to become ready.
	// It is 0 on a warm hit and on the cold one-shot path, which does not
	// split initialization from the handler.
	InitMs int64  `json:"initMs"`
	Stdout string `json:"stdout,omitempty"`
	Stderr string `json:"stderr,omitempty"`
}

func New(store artifactDownloader, rdb *redis.Client) *Executor {
	e := &Executor{
		store:        store,
		rdb:          rdb,
		defaultImage: getEnv("NIMBUS_IMAGE", "nimbus-node"),
		codeDirs:     map[string]string{},
		codeLocks:    map[string]*sync.Mutex{},
	}
	cfg := warm.LoadConfig()
	if cfg.Enabled {
		e.pool = warm.New(cfg, dockerStarter{image: e.defaultImage})
		log.Printf("[executor] Warm pool activo (idle=%s, por función=%d, global=%d)", cfg.IdleTTL, cfg.MaxPerFunction, cfg.GlobalMax)
	} else {
		log.Printf("[executor] Warm pool apagado (NIMBUS_WARM)")
	}
	return e
}

// StartBackground launches the idle reaper and drops warm containers and
// code directories left by a previous process.
func (e *Executor) StartBackground() {
	if e.pool == nil {
		return
	}
	e.cleanWarmDirs()
	e.pool.StartReaper()
}

// Shutdown destroys every warm container and the code directories they mounted.
func (e *Executor) Shutdown(ctx context.Context) {
	if e.pool != nil {
		e.pool.Shutdown(ctx)
	}
	e.codeMu.Lock()
	dirs := e.codeDirs
	e.codeDirs = map[string]string{}
	e.codeMu.Unlock()
	for _, dir := range dirs {
		os.RemoveAll(dir)
	}
}

func (e *Executor) Execute(ctx context.Context, req *ExecutionRequest) *ExecutionResult {
	if e.pool != nil {
		return e.executeWarm(ctx, req)
	}
	return e.executeCold(ctx, req)
}

func (e *Executor) executeCold(ctx context.Context, req *ExecutionRequest) *ExecutionResult {
	startTime := time.Now()
	result := &ExecutionResult{
		ExecutionID: req.ExecutionID,
		ExitCode:    1,
		ColdStart:   true,
	}

	logWriter := logs.NewStreamWriter(e.rdb, req.ExecutionID)
	defer logWriter.Close(ctx)

	log.Printf("[executor] Iniciando ejecución %s (fn: %s, version: %s)", req.ExecutionID, req.FunctionID, req.Version)
	logWriter.WriteStderr(ctx, fmt.Sprintf("[nimbus] Iniciando ejecución %s", req.ExecutionID))

	tempDir, err := e.store.DownloadArtifact(ctx, req.FunctionID, req.Version)
	if err != nil {
		result.Error = fmt.Sprintf("Error descargando artifact: %v", err)
		result.DurationMs = time.Since(startTime).Milliseconds()
		logWriter.WriteStderr(ctx, fmt.Sprintf("[nimbus] Error: %s", result.Error))
		return result
	}
	defer os.RemoveAll(tempDir)

	zipPath := filepath.Join(tempDir, "code.zip")
	workDir := filepath.Join(tempDir, "work")
	if err := os.MkdirAll(workDir, 0755); err != nil {
		result.Error = fmt.Sprintf("Error creando directorio de trabajo: %v", err)
		result.DurationMs = time.Since(startTime).Milliseconds()
		logWriter.WriteStderr(ctx, fmt.Sprintf("[nimbus] Error: %s", result.Error))
		return result
	}

	if err := unzip(zipPath, workDir); err != nil {
		result.Error = fmt.Sprintf("Error extrayendo zip: %v", err)
		result.DurationMs = time.Since(startTime).Milliseconds()
		logWriter.WriteStderr(ctx, fmt.Sprintf("[nimbus] Error: %s", result.Error))
		return result
	}

	eventPath := filepath.Join(workDir, "event.json")
	eventBytes, _ := json.Marshal(req.Event)
	if err := os.WriteFile(eventPath, eventBytes, 0644); err != nil {
		result.Error = fmt.Sprintf("Error escribiendo evento: %v", err)
		result.DurationMs = time.Since(startTime).Milliseconds()
		logWriter.WriteStderr(ctx, fmt.Sprintf("[nimbus] Error: %s", result.Error))
		return result
	}

	if err := e.ensureImage(ctx); err != nil {
		result.Error = fmt.Sprintf("Error con imagen Docker: %v", err)
		result.DurationMs = time.Since(startTime).Milliseconds()
		logWriter.WriteStderr(ctx, fmt.Sprintf("[nimbus] Error: %s", result.Error))
		return result
	}

	cfg, cfgErr := req.Config.Complete()
	if cfgErr != nil {
		result.Error = fmt.Sprintf("Configuración inválida: %s", cfgErr.Error())
		result.DurationMs = time.Since(startTime).Milliseconds()
		logWriter.WriteStderr(ctx, fmt.Sprintf("[nimbus] Error: %s", result.Error))
		return result
	}

	logWriter.WriteStderr(ctx, fmt.Sprintf("[nimbus] Ejecutando handler '%s' (timeout: %ds, memory: %dMB, cpus: %s, env: %d)",
		cfg.Handler, cfg.TimeoutSec, cfg.MemoryMB, cpusLabel(cfg), len(cfg.Env)))
	dockerResult := e.runDockerWithStreaming(ctx, req.ExecutionID, workDir, cfg, logWriter)

	result.Success = dockerResult.success
	result.Output = dockerResult.output
	result.ExitCode = dockerResult.exitCode
	result.Stdout = dockerResult.stdout
	result.Stderr = dockerResult.stderr
	result.DurationMs = time.Since(startTime).Milliseconds()

	if !dockerResult.success && result.Error == "" {
		result.Error = outcomeError(dockerResult.exitCode, dockerResult.oom, dockerResult.stderr, cfg.TimeoutSec, cfg.MemoryMB)
		if dockerResult.oom || dockerResult.exitCode == 124 || dockerResult.exitCode == 0 || result.Error == "Error en la ejecución" {
			logWriter.WriteStderr(ctx, fmt.Sprintf("[nimbus] Error: %s", result.Error))
		}
	}

	log.Printf("[executor] Ejecución %s completada (success: %v, exit: %d, duration: %dms)",
		req.ExecutionID, result.Success, result.ExitCode, result.DurationMs)
	logWriter.WriteStderr(ctx, fmt.Sprintf("[nimbus] Ejecución completada (success: %v, duration: %dms)", result.Success, result.DurationMs))

	return result
}

type dockerResult struct {
	success  bool
	output   interface{}
	exitCode int
	stdout   string
	stderr   string
	oom      bool
}

func (e *Executor) runDockerWithStreaming(ctx context.Context, executionID, workDir string, cfg fnconfig.Resolved, logWriter *logs.StreamWriter) dockerResult {
	name := containerName(executionID)
	args := dockerRunArgs(name, workDir, e.defaultImage, cfg)
	// User env values are secrets. The log line keeps the keys and redacts the values.
	log.Printf("[executor] docker %s", formatArgsForLog(args))

	timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutSec)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(timeoutCtx, "docker", args...)
	cmd.WaitDelay = 3 * time.Second

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return dockerResult{exitCode: 1, stderr: fmt.Sprintf("Error creating stdout pipe: %v", err)}
	}

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return dockerResult{exitCode: 1, stderr: fmt.Sprintf("Error creating stderr pipe: %v", err)}
	}

	if err := cmd.Start(); err != nil {
		return dockerResult{exitCode: 1, stderr: fmt.Sprintf("Error starting command: %v", err)}
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stdoutPipe)
		for scanner.Scan() {
			line := scanner.Text()
			stdoutBuf.WriteString(line + "\n")
			logWriter.WriteStdout(ctx, line)
		}
	}()

	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stderrPipe)
		for scanner.Scan() {
			line := scanner.Text()
			stderrBuf.WriteString(line + "\n")
			logWriter.WriteStderr(ctx, line)
		}
	}()

	wg.Wait()
	waitErr := cmd.Wait()

	timedOut := errors.Is(timeoutCtx.Err(), context.DeadlineExceeded)
	exitCode := exitCodeFrom(waitErr, timedOut)

	stdoutStr := stdoutBuf.String()
	stderrStr := stderrBuf.String()
	// A deadline kill is SIGKILL too. Inspect OOM only when the deadline
	// did not fire, then always remove the container (--rm would drop it
	// before we can read OOMKilled).
	oom := false
	if !timedOut && exitCode != 0 {
		oom = containerOOMKilled(name) || exitCode == 137 || heapOOM(stderrStr)
	}
	removeContainer(name)

	output, ok := parseOutput(stdoutStr)
	return dockerResult{
		success:  exitCode == 0 && ok,
		output:   output,
		exitCode: exitCode,
		stdout:   stdoutStr,
		stderr:   stderrStr,
		oom:      oom,
	}
}

// dockerRunArgs builds the docker CLI arguments for one user container.
// workDir must be a host path the Docker daemon can see. When the compute
// plane itself runs in a container, NIMBUS_WORK_DIR has to be bind-mounted
// at the same absolute path on the host.
//
// The container is not started with --rm: the caller inspects OOMKilled
// and then removes it. User environment variables are passed with -e and
// must not be written to logs; use formatArgsForLog.
func dockerRunArgs(containerName, workDir, image string, cfg fnconfig.Resolved) []string {
	args := []string{
		"run",
		"--name", containerName,
		fmt.Sprintf("--memory=%dm", cfg.MemoryMB),
		fmt.Sprintf("--memory-swap=%dm", cfg.MemoryMB),
		"--pids-limit=256",
		"--network=none",
		"--read-only",
	}
	if cfg.CPUsFlag != "" {
		args = append(args, "--cpus="+cfg.CPUsFlag)
	}
	args = append(args,
		"-v", fmt.Sprintf("%s:/var/task:ro", workDir),
	)
	// User variables go first. Platform variables go after them so a
	// request cannot override NIMBUS_* even if validation missed one.
	for _, ev := range cfg.Env {
		args = append(args, "-e", ev.Name+"="+ev.Value)
	}
	args = append(args,
		"-e", "NIMBUS_EVENT_PATH=/var/task/event.json",
		"-e", "NIMBUS_HANDLER="+cfg.Handler,
		"-e", "NIMBUS_HANDLER_FUNCTION="+cfg.HandlerExport,
		"-e", "NIMBUS_HANDLER_PATH=/var/task/"+cfg.HandlerFile+".js",
		"-e", fmt.Sprintf("NIMBUS_MEMORY_MB=%d", cfg.MemoryMB),
		"-e", fmt.Sprintf("NIMBUS_TIMEOUT_SEC=%d", cfg.TimeoutSec),
		image,
	)
	return args
}

// platformEnvKeys are set by the compute plane. Their values are not secrets.
var platformEnvKeys = map[string]struct{}{
	"NIMBUS_EVENT_PATH":       {},
	"NIMBUS_HANDLER":          {},
	"NIMBUS_HANDLER_FUNCTION": {},
	"NIMBUS_HANDLER_PATH":     {},
	"NIMBUS_MEMORY_MB":        {},
	"NIMBUS_TIMEOUT_SEC":      {},
	"NIMBUS_FUNCTION_NAME":    {},
	"NIMBUS_WARM":             {},
	"HOME":                    {},
}

// formatArgsForLog redacts every -e value that is not a platform variable.
func formatArgsForLog(args []string) string {
	out := make([]string, len(args))
	for i, arg := range args {
		if i > 0 && args[i-1] == "-e" {
			key, _, ok := strings.Cut(arg, "=")
			if ok {
				if _, platform := platformEnvKeys[key]; !platform {
					out[i] = key + "=***"
					continue
				}
			}
		}
		out[i] = arg
	}
	return strings.Join(out, " ")
}

func cpusLabel(cfg fnconfig.Resolved) string {
	if cfg.CPUsFlag == "" {
		return "sin límite"
	}
	return cfg.CPUsFlag
}

func outcomeError(exitCode int, oom bool, stderr string, timeoutSec, memoryMB int) string {
	switch {
	case oom:
		return fmt.Sprintf("La función superó el límite de memoria (%d MB) y fue terminada", memoryMB)
	case exitCode == 124:
		return fmt.Sprintf("Timeout excedido (%ds)", timeoutSec)
	case exitCode == 0:
		return "La función terminó sin un JSON de salida"
	case stderr != "":
		return stderr
	default:
		return "Error en la ejecución"
	}
}

func heapOOM(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "javascript heap out of memory") || strings.Contains(s, "reached heap limit")
}

func containerOOMKilled(name string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.State.OOMKilled}}", name)
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}

func containerName(executionID string) string {
	var b strings.Builder
	b.WriteString("nimbus-")
	for _, r := range executionID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	name := b.String()
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.TrimRight(name, ".-")
}

func removeContainer(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "rm", "-f", name)
	if err := cmd.Run(); err != nil {
		log.Printf("[executor] no se pudo borrar el container %s: %v", name, err)
	}
}

func exitCodeFrom(err error, timedOut bool) int {
	if timedOut {
		return 124
	}
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code := exitErr.ExitCode()
		if code < 0 {
			return 1
		}
		return code
	}
	return 1
}

// parseOutput extracts the handler result from stdout.
// The Node bootstrap pretty-prints JSON, and the handler may log lines
// before that result, so a single-line scan is not enough.
func parseOutput(stdout string) (interface{}, bool) {
	trimmed := strings.TrimSpace(stdout)
	if trimmed == "" {
		return nil, false
	}
	if v, ok := decodeJSONValue(trimmed); ok {
		return v, true
	}

	lines := strings.Split(trimmed, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		chunk := strings.TrimSpace(strings.Join(lines[i:], "\n"))
		if chunk == "" || !looksLikeJSON(chunk) {
			continue
		}
		if v, ok := decodeJSONValue(chunk); ok {
			return v, true
		}
	}
	return nil, false
}

func looksLikeJSON(s string) bool {
	switch s[0] {
	case '{', '[', '"', 't', 'f', 'n', '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return true
	default:
		return false
	}
}

func decodeJSONValue(s string) (interface{}, bool) {
	var v interface{}
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return nil, false
	}
	return v, true
}

func (e *Executor) ensureImage(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "docker", "image", "inspect", e.defaultImage)
	if err := cmd.Run(); err == nil {
		return nil
	}

	log.Printf("[executor] Construyendo imagen %s...", e.defaultImage)

	runtimePath := "/app/runtime-node"
	if _, err := os.Stat(runtimePath); os.IsNotExist(err) {
		runtimePath = "./runtime-node"
		if _, err := os.Stat(runtimePath); os.IsNotExist(err) {
			cwd, _ := os.Getwd()
			return fmt.Errorf("directorio runtime-node no encontrado (cwd: %s)", cwd)
		}
	}

	cmd = exec.CommandContext(ctx, "docker", "build", "-t", e.defaultImage, runtimePath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("error construyendo imagen: %w", err)
	}

	return nil
}

func unzip(src, dest string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()

	destClean := filepath.Clean(dest)
	prefix := destClean + string(os.PathSeparator)

	for _, f := range r.File {
		fpath := filepath.Join(destClean, f.Name)
		if fpath != destClean && !strings.HasPrefix(fpath, prefix) {
			return fmt.Errorf("illegal file path: %s", fpath)
		}

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(fpath, 0o755); err != nil {
				return err
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(fpath), 0o755); err != nil {
			return err
		}

		outFile, err := os.OpenFile(fpath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			return err
		}

		rc, err := f.Open()
		if err != nil {
			outFile.Close()
			return err
		}

		_, err = io.Copy(outFile, rc)
		outFile.Close()
		rc.Close()

		if err != nil {
			return err
		}
	}

	return nil
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
