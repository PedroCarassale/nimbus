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

	"github.com/PedroCarassale/nimbus/compute-plane/internal/logs"
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
}

type ExecutionRequest struct {
	ExecutionID string                 `json:"executionId"`
	FunctionID  string                 `json:"functionId"`
	Version     string                 `json:"version"`
	Event       map[string]interface{} `json:"event"`
	TimeoutSec  int                    `json:"timeoutSec"`
	MemoryMB    int                    `json:"memoryMb"`
	Handler     string                 `json:"handler"`
}

type ExecutionResult struct {
	ExecutionID string      `json:"executionId"`
	Success     bool        `json:"success"`
	Output      interface{} `json:"output"`
	Error       string      `json:"error,omitempty"`
	ExitCode    int         `json:"exitCode"`
	DurationMs  int64       `json:"durationMs"`
	Stdout      string      `json:"stdout,omitempty"`
	Stderr      string      `json:"stderr,omitempty"`
}

func New(store artifactDownloader, rdb *redis.Client) *Executor {
	return &Executor{
		store:        store,
		rdb:          rdb,
		defaultImage: getEnv("NIMBUS_IMAGE", "nimbus-node"),
	}
}

func (e *Executor) Execute(ctx context.Context, req *ExecutionRequest) *ExecutionResult {
	startTime := time.Now()
	result := &ExecutionResult{
		ExecutionID: req.ExecutionID,
		ExitCode:    1,
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

	timeoutSec, memoryMB, handler := applyDefaults(req.TimeoutSec, req.MemoryMB, req.Handler)

	logWriter.WriteStderr(ctx, fmt.Sprintf("[nimbus] Ejecutando handler '%s' (timeout: %ds, memory: %dMB)", handler, timeoutSec, memoryMB))
	dockerResult := e.runDockerWithStreaming(ctx, req.ExecutionID, workDir, timeoutSec, memoryMB, handler, logWriter)

	result.Success = dockerResult.success
	result.Output = dockerResult.output
	result.ExitCode = dockerResult.exitCode
	result.Stdout = dockerResult.stdout
	result.Stderr = dockerResult.stderr
	result.DurationMs = time.Since(startTime).Milliseconds()

	if !dockerResult.success && result.Error == "" {
		switch {
		case dockerResult.exitCode == 124:
			result.Error = fmt.Sprintf("Timeout excedido (%ds)", timeoutSec)
			logWriter.WriteStderr(ctx, fmt.Sprintf("[nimbus] Error: %s", result.Error))
		case dockerResult.exitCode == 0:
			result.Error = "La función terminó sin un JSON de salida"
			logWriter.WriteStderr(ctx, fmt.Sprintf("[nimbus] Error: %s", result.Error))
		case dockerResult.stderr != "":
			result.Error = dockerResult.stderr
		default:
			result.Error = "Error en la ejecución"
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
}

func (e *Executor) runDockerWithStreaming(ctx context.Context, executionID, workDir string, timeoutSec, memoryMB int, handler string, logWriter *logs.StreamWriter) dockerResult {
	name := containerName(executionID)
	args := dockerRunArgs(name, workDir, e.defaultImage, handler, memoryMB)
	log.Printf("[executor] docker %s", strings.Join(args, " "))

	timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
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
	if exitCode != 0 {
		// Killing the docker CLI does not stop the container. --rm only runs
		// if the CLI exits on its own.
		removeContainer(name)
	}

	stdoutStr := stdoutBuf.String()
	stderrStr := stderrBuf.String()
	output, ok := parseOutput(stdoutStr)

	return dockerResult{
		success:  exitCode == 0 && ok,
		output:   output,
		exitCode: exitCode,
		stdout:   stdoutStr,
		stderr:   stderrStr,
	}
}

// dockerRunArgs builds the docker CLI arguments for one user container.
// workDir must be a host path the Docker daemon can see. When the compute
// plane itself runs in a container, NIMBUS_WORK_DIR has to be bind-mounted
// at the same absolute path on the host.
func dockerRunArgs(containerName, workDir, image, handler string, memoryMB int) []string {
	return []string{
		"run",
		"--name", containerName,
		"--rm",
		fmt.Sprintf("--memory=%dm", memoryMB),
		"--pids-limit=256",
		"--network=none",
		"--read-only",
		"-v", fmt.Sprintf("%s:/var/task:ro", workDir),
		"-e", fmt.Sprintf("NIMBUS_HANDLER_FUNCTION=%s", handler),
		"-e", "NIMBUS_EVENT_PATH=/var/task/event.json",
		image,
	}
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

func applyDefaults(timeoutSec, memoryMB int, handler string) (int, int, string) {
	if timeoutSec <= 0 {
		timeoutSec = 5
	}
	if memoryMB <= 0 {
		memoryMB = 128
	}
	if handler == "" {
		handler = "handler"
	}
	return timeoutSec, memoryMB, handler
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
