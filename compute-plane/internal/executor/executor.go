package executor

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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
	minioClient "github.com/PedroCarassale/nimbus/compute-plane/internal/minio"
	"github.com/redis/go-redis/v9"
)

type Executor struct {
	minio        *minioClient.Client
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

func New(minio *minioClient.Client, rdb *redis.Client) *Executor {
	return &Executor{
		minio:        minio,
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

	tempDir, err := e.minio.DownloadArtifact(ctx, req.FunctionID, req.Version)
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

	if err := e.unzip(zipPath, workDir); err != nil {
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

	timeoutSec := req.TimeoutSec
	if timeoutSec <= 0 {
		timeoutSec = 5
	}
	memoryMB := req.MemoryMB
	if memoryMB <= 0 {
		memoryMB = 128
	}
	handler := req.Handler
	if handler == "" {
		handler = "handler"
	}

	logWriter.WriteStderr(ctx, fmt.Sprintf("[nimbus] Ejecutando handler '%s' (timeout: %ds, memory: %dMB)", handler, timeoutSec, memoryMB))
	dockerResult := e.runDockerWithStreaming(ctx, workDir, timeoutSec, memoryMB, handler, logWriter)

	result.Success = dockerResult.success
	result.Output = dockerResult.output
	result.ExitCode = dockerResult.exitCode
	result.Stdout = dockerResult.stdout
	result.Stderr = dockerResult.stderr
	result.DurationMs = time.Since(startTime).Milliseconds()

	if !dockerResult.success && result.Error == "" {
		if dockerResult.exitCode == 124 {
			result.Error = fmt.Sprintf("Timeout excedido (%ds)", timeoutSec)
			logWriter.WriteStderr(ctx, fmt.Sprintf("[nimbus] Error: %s", result.Error))
		} else if dockerResult.stderr != "" {
			result.Error = dockerResult.stderr
		} else {
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

func (e *Executor) runDockerWithStreaming(ctx context.Context, workDir string, timeoutSec, memoryMB int, handler string, logWriter *logs.StreamWriter) dockerResult {
	args := []string{
		"run",
		"--rm",
		fmt.Sprintf("--memory=%dm", memoryMB),
		"--pids-limit=256",
		"--network=none",
		"--read-only",
		"-v", fmt.Sprintf("%s:/var/task:ro", workDir),
		"-e", fmt.Sprintf("NIMBUS_HANDLER_FUNCTION=%s", handler),
		"-e", "NIMBUS_EVENT_PATH=/var/task/event.json",
		e.defaultImage,
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second+2*time.Second)
	defer cancel()

	cmd := exec.CommandContext(timeoutCtx, "timeout", append([]string{fmt.Sprintf("%ds", timeoutSec), "docker"}, args...)...)

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

	err = cmd.Wait()

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	stdoutStr := stdoutBuf.String()
	stderrStr := stderrBuf.String()

	output := e.parseOutput(stdoutStr)

	return dockerResult{
		success:  exitCode == 0 && output != nil,
		output:   output,
		exitCode: exitCode,
		stdout:   stdoutStr,
		stderr:   stderrStr,
	}
}

func (e *Executor) parseOutput(stdout string) interface{} {
	lines := strings.Split(strings.TrimSpace(stdout), "\n")

	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "{") || strings.HasPrefix(line, "[") {
			var result interface{}
			if err := json.Unmarshal([]byte(line), &result); err == nil {
				return result
			}
		}
	}

	return nil
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

func (e *Executor) unzip(src, dest string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()

	for _, f := range r.File {
		fpath := filepath.Join(dest, f.Name)

		if !strings.HasPrefix(fpath, filepath.Clean(dest)+string(os.PathSeparator)) {
			return fmt.Errorf("illegal file path: %s", fpath)
		}

		if f.FileInfo().IsDir() {
			os.MkdirAll(fpath, os.ModePerm)
			continue
		}

		if err := os.MkdirAll(filepath.Dir(fpath), os.ModePerm); err != nil {
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
