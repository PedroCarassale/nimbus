package executor

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/PedroCarassale/nimbus/compute-plane/internal/isolate"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/logs"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/runtime"
	ntrace "github.com/PedroCarassale/nimbus/compute-plane/internal/trace"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/warm"
	"go.opentelemetry.io/otel/attribute"
)

func (e *Executor) isolation() string {
	if e == nil || e.backend == nil {
		return isolate.Docker
	}
	return e.backend.Name()
}

// ensureRuntime makes the Docker image available. Firecracker checks the
// ext4 inside Start, so a missing rootfs is the same error as a failed boot.
func (e *Executor) ensureRuntime(ctx context.Context, image string) error {
	if e.isolation() == isolate.Firecracker {
		return nil
	}
	return e.ensureImage(ctx, image)
}

// executeOnce is the Firecracker one-shot: boot, one invoke, destroy.
// initMs is the boot, unlike the Docker cold path which reports 0.
func (e *Executor) executeOnce(ctx context.Context, req *ExecutionRequest) *ExecutionResult {
	startTime := time.Now()
	result := &ExecutionResult{
		ExecutionID: req.ExecutionID,
		ExitCode:    1,
		ColdStart:   true,
	}
	logWriter := logs.NewStreamWriter(e.rdb, req.ExecutionID)
	defer func() {
		result.DurationMs = time.Since(startTime).Milliseconds()
		logWriter.Close(context.WithoutCancel(ctx))
	}()

	ntrace.Logf(ctx, "[executor] Iniciando ejecución %s (fn: %s, version: %s)", req.ExecutionID, req.FunctionID, req.Version)
	logWriter.WriteStderr(ctx, fmt.Sprintf("[nimbus] Iniciando ejecución %s", req.ExecutionID))
	writeTraceLog(ctx, logWriter)

	dlCtx, dlSpan := ntrace.Start(ctx, "artifact.download")
	dlSpan.SetAttributes(attribute.String("nimbus.function_id", req.FunctionID))
	tempDir, err := e.store.DownloadArtifact(dlCtx, req.FunctionID, req.Version)
	if err != nil {
		ntrace.Fail(dlSpan, err)
		dlSpan.End()
		result.Error = fmt.Sprintf("Error descargando artifact: %v", err)
		logWriter.WriteStderr(ctx, fmt.Sprintf("[nimbus] Error: %s", result.Error))
		return result
	}
	defer os.RemoveAll(tempDir)

	workDir := filepath.Join(tempDir, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		dlSpan.End()
		result.Error = fmt.Sprintf("Error creando directorio de trabajo: %v", err)
		logWriter.WriteStderr(ctx, fmt.Sprintf("[nimbus] Error: %s", result.Error))
		return result
	}
	if err := unzip(filepath.Join(tempDir, "code.zip"), workDir); err != nil {
		ntrace.Fail(dlSpan, err)
		dlSpan.End()
		result.Error = fmt.Sprintf("Error extrayendo zip: %v", err)
		logWriter.WriteStderr(ctx, fmt.Sprintf("[nimbus] Error: %s", result.Error))
		return result
	}
	dlSpan.End()

	cfg, cfgErr := req.Config.Complete()
	if cfgErr != nil {
		result.Error = fmt.Sprintf("Configuración inválida: %s", cfgErr.Error())
		logWriter.WriteStderr(ctx, fmt.Sprintf("[nimbus] Error: %s", result.Error))
		return result
	}
	cfg.Runtime = req.Config.Runtime
	if err := makeGoBinaryRunnable(workDir, cfg); err != nil {
		result.Error = err.Error()
		logWriter.WriteStderr(ctx, fmt.Sprintf("[nimbus] Error: %s", result.Error))
		return result
	}

	logWriter.WriteStderr(ctx, fmt.Sprintf("[nimbus] Ejecutando handler '%s' (runtime: %s, isolation: %s, timeout: %ds, memory: %dMB, cpus: %s, env: %d)",
		cfg.Handler, runtime.Canonical(cfg.Runtime), e.isolation(), cfg.TimeoutSec, cfg.MemoryMB, cpusLabel(cfg), len(cfg.Env)))

	timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutSec)*time.Second)
	defer cancel()

	event := req.Event
	if event == nil {
		event = map[string]interface{}{}
	}
	_, acquireSpan := ntrace.Start(ctx, "container.acquire")
	acquireSpan.SetAttributes(
		attribute.String("nimbus.execution_id", req.ExecutionID),
		attribute.String("nimbus.isolation", e.isolation()),
		attribute.Bool("nimbus.cold_start", true),
	)
	sess, err := e.backend.Start(timeoutCtx, warm.Spec{
		FunctionID:  req.FunctionID,
		Version:     req.Version,
		WorkDir:     workDir,
		Image:       e.imageFor(cfg),
		Config:      cfg,
		TraceID:     ntrace.ID(ctx),
		TraceParent: ntrace.Header(ctx),
	})
	if err != nil {
		ntrace.Fail(acquireSpan, err)
		acquireSpan.End()
		if cancelIsDeadline(timeoutCtx) {
			result.ExitCode = 124
			result.Error = outcomeError(124, false, "", cfg.TimeoutSec, cfg.MemoryMB)
		} else {
			result.Error = err.Error()
		}
		logWriter.WriteStderr(ctx, fmt.Sprintf("[nimbus] Error: %s", result.Error))
		return result
	}
	result.InitMs = sess.InitMs()
	acquireSpan.SetAttributes(attribute.Int64("nimbus.init_ms", result.InitMs))
	acquireSpan.End()
	logWriter.WriteStderr(ctx, fmt.Sprintf("[nimbus] coldStart=true initMs=%d", result.InitMs))

	_, runSpan := ntrace.Start(ctx, "container.run")
	runSpan.SetAttributes(
		attribute.String("nimbus.execution_id", req.ExecutionID),
		attribute.String("nimbus.isolation", e.isolation()),
		attribute.Bool("nimbus.cold_start", true),
	)
	out := sess.Invoke(timeoutCtx, warm.Invocation{
		ExecutionID: req.ExecutionID,
		Event:       event,
		TraceID:     ntrace.ID(ctx),
		TraceParent: ntrace.Header(ctx),
		Log: func(stream, line string) {
			writeUserLog(ctx, logWriter, stream, line)
		},
	})
	runSpan.End()
	_ = sess.Destroy(context.Background())

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
		logWriter.WriteStderr(ctx, fmt.Sprintf("[nimbus] Error: %s", result.Error))
	}
	log.Printf("[executor] Ejecución %s completada (success: %v, exit: %d, coldStart: true, initMs: %d, duration: %dms)",
		req.ExecutionID, result.Success, result.ExitCode, result.InitMs, time.Since(startTime).Milliseconds())
	return result
}

func cancelIsDeadline(ctx context.Context) bool {
	return ctx.Err() == context.DeadlineExceeded
}
