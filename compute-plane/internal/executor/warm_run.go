package executor

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/PedroCarassale/nimbus/compute-plane/internal/logs"
	ntrace "github.com/PedroCarassale/nimbus/compute-plane/internal/trace"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/warm"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

func (e *Executor) executeWarm(ctx context.Context, req *ExecutionRequest) *ExecutionResult {
	startTime := time.Now()
	result := &ExecutionResult{
		ExecutionID: req.ExecutionID,
		ExitCode:    1,
		ColdStart:   true,
	}

	logWriter := logs.NewStreamWriter(e.rdb, req.ExecutionID)
	defer func() {
		result.DurationMs = time.Since(startTime).Milliseconds()
		// WithoutCancel: a user cancel must still write the _end marker and the TTL.
		logWriter.Close(context.WithoutCancel(ctx))
	}()

	ntrace.Logf(ctx, "[executor] Iniciando ejecución %s (fn: %s, version: %s)", req.ExecutionID, req.FunctionID, req.Version)
	logWriter.WriteStderr(ctx, fmt.Sprintf("[nimbus] Iniciando ejecución %s", req.ExecutionID))
	writeTraceLog(ctx, logWriter)

	cfg, cfgErr := req.Config.Complete()
	if cfgErr != nil {
		result.Error = fmt.Sprintf("Configuración inválida: %s", cfgErr.Error())
		logWriter.WriteStderr(ctx, fmt.Sprintf("[nimbus] Error: %s", result.Error))
		return result
	}

	dlCtx, dlSpan := ntrace.Start(ctx, "artifact.download")
	dlSpan.SetAttributes(attribute.String("nimbus.function_id", req.FunctionID))
	workDir, err := e.prepareWarmCode(dlCtx, req.FunctionID, req.Version)
	if err != nil {
		ntrace.Fail(dlSpan, err)
		dlSpan.End()
		result.Error = fmt.Sprintf("Error %s", err.Error())
		logWriter.WriteStderr(ctx, fmt.Sprintf("[nimbus] Error: %s", result.Error))
		return result
	}
	dlSpan.End()

	if err := e.ensureImage(ctx); err != nil {
		result.Error = fmt.Sprintf("Error con imagen Docker: %v", err)
		logWriter.WriteStderr(ctx, fmt.Sprintf("[nimbus] Error: %s", result.Error))
		return result
	}

	logWriter.WriteStderr(ctx, fmt.Sprintf("[nimbus] Ejecutando handler '%s' (timeout: %ds, memory: %dMB, cpus: %s, env: %d)",
		cfg.Handler, cfg.TimeoutSec, cfg.MemoryMB, cpusLabel(cfg), len(cfg.Env)))

	timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutSec)*time.Second)
	defer cancel()

	event := req.Event
	if event == nil {
		event = map[string]interface{}{}
	}

	_, acquireSpan := ntrace.Start(ctx, "container.acquire")
	acquireSpan.SetAttributes(attribute.String("nimbus.function_id", req.FunctionID))
	var runSpan trace.Span
	runStarted := false
	out, meta := e.pool.Run(timeoutCtx, warm.Spec{
		Key:         warm.PoolKey(req.FunctionID, req.Version, cfg),
		FunctionID:  req.FunctionID,
		Version:     req.Version,
		WorkDir:     workDir,
		Image:       e.defaultImage,
		Config:      cfg,
		TraceID:     ntrace.ID(ctx),
		TraceParent: ntrace.Header(ctx),
	}, warm.Invocation{
		ExecutionID: req.ExecutionID,
		Event:       event,
		TraceID:     ntrace.ID(ctx),
		TraceParent: ntrace.Header(ctx),
		Log: func(stream, line string) {
			writeUserLog(ctx, logWriter, stream, line)
		},
		Begin: func(m warm.Meta) {
			acquireSpan.SetAttributes(
				attribute.Bool("nimbus.cold_start", m.ColdStart),
				attribute.Int64("nimbus.init_ms", m.InitMs),
			)
			acquireSpan.End()
			_, runSpan = ntrace.Start(ctx, "container.run")
			runSpan.SetAttributes(
				attribute.String("nimbus.function_id", req.FunctionID),
				attribute.String("nimbus.execution_id", req.ExecutionID),
				attribute.Bool("nimbus.cold_start", m.ColdStart),
			)
			runStarted = true
			logWriter.WriteStderr(ctx, fmt.Sprintf("[nimbus] coldStart=%t initMs=%d", m.ColdStart, m.InitMs))
		},
	})
	if !runStarted {
		acquireSpan.End()
	} else {
		runSpan.End()
	}

	result.ColdStart = meta.ColdStart
	result.InitMs = meta.InitMs
	result.Success = out.Success
	result.Output = out.Output
	result.ExitCode = out.ExitCode
	result.Stdout = out.Stdout
	result.Stderr = out.Stderr
	if out.Error != "" {
		result.Error = out.Error
	}
	if !out.Success && result.Error == "" {
		result.Error = outcomeError(out.ExitCode, out.OOM, out.Stderr, cfg.TimeoutSec, cfg.MemoryMB)
		if out.OOM || out.ExitCode == 124 || out.ExitCode == 0 || result.Error == "Error en la ejecución" {
			logWriter.WriteStderr(ctx, fmt.Sprintf("[nimbus] Error: %s", result.Error))
		}
	}

	log.Printf("[executor] Ejecución %s completada (success: %v, exit: %d, coldStart: %t, initMs: %d, duration: %dms)",
		req.ExecutionID, result.Success, result.ExitCode, result.ColdStart, result.InitMs, time.Since(startTime).Milliseconds())
	logWriter.WriteStderr(ctx, fmt.Sprintf("[nimbus] Ejecución completada (success: %v, coldStart: %t, duration: %dms)",
		result.Success, result.ColdStart, time.Since(startTime).Milliseconds()))
	return result
}

func writeUserLog(ctx context.Context, logWriter *logs.StreamWriter, stream, line string) {
	if stream == "stderr" {
		logWriter.WriteStderr(ctx, line)
		return
	}
	logWriter.WriteStdout(ctx, line)
}

// prepareWarmCode downloads and extracts a version once per process.
// Later invocations of that version mount the same directory. The directory
// outlives the call so a warm container can keep it. Shutdown removes it.
func (e *Executor) prepareWarmCode(ctx context.Context, functionID, version string) (string, error) {
	key := functionID + "\x00" + version
	if dir, ok := e.cachedCode(key); ok {
		return dir, nil
	}

	lock := e.codeLock(key)
	lock.Lock()
	defer lock.Unlock()
	if dir, ok := e.cachedCode(key); ok {
		return dir, nil
	}

	tempDir, err := e.store.DownloadArtifact(ctx, functionID, version)
	if err != nil {
		return "", fmt.Errorf("descargando artifact: %w", err)
	}
	defer os.RemoveAll(tempDir)

	parent, err := warmCodeRoot()
	if err != nil {
		return "", err
	}
	dest, err := os.MkdirTemp(parent, "fn-")
	if err != nil {
		return "", fmt.Errorf("creando directorio de trabajo: %w", err)
	}
	if err := unzip(filepath.Join(tempDir, "code.zip"), dest); err != nil {
		os.RemoveAll(dest)
		return "", fmt.Errorf("extrayendo zip: %w", err)
	}
	if err := makeTreeReadable(dest); err != nil {
		os.RemoveAll(dest)
		return "", err
	}

	e.codeMu.Lock()
	e.codeDirs[key] = dest
	e.codeMu.Unlock()
	return dest, nil
}

func (e *Executor) cachedCode(key string) (string, bool) {
	e.codeMu.Lock()
	defer e.codeMu.Unlock()
	dir, ok := e.codeDirs[key]
	if !ok {
		return "", false
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		delete(e.codeDirs, key)
		return "", false
	}
	return dir, true
}

func (e *Executor) codeLock(key string) *sync.Mutex {
	e.codeMu.Lock()
	defer e.codeMu.Unlock()
	if e.codeLocks == nil {
		e.codeLocks = map[string]*sync.Mutex{}
	}
	m, ok := e.codeLocks[key]
	if !ok {
		m = &sync.Mutex{}
		e.codeLocks[key] = m
	}
	return m
}

func warmCodeRoot() (string, error) {
	parent := os.Getenv("NIMBUS_WORK_DIR")
	if parent == "" {
		parent = os.TempDir()
	}
	dir := filepath.Join(parent, "warm-code")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creando directorio de trabajo: %w", err)
	}
	return dir, nil
}

func (e *Executor) cleanWarmDirs() {
	parent, err := warmCodeRoot()
	if err != nil {
		log.Printf("[executor] no se pudo preparar el directorio warm: %v", err)
		return
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	for _, entry := range entries {
		os.RemoveAll(filepath.Join(parent, entry.Name()))
	}
}

func makeTreeReadable(root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return os.Chmod(path, 0o755)
		}
		return os.Chmod(path, 0o644)
	})
}

func labelValue(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "x"
	}
	return b.String()
}
