package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/PedroCarassale/nimbus/compute-plane/internal/async"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/cancel"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/concurrency"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/executor"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/fnconfig"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/isolate"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/metrics"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/runtime"
	ntrace "github.com/PedroCarassale/nimbus/compute-plane/internal/trace"
	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// runner executes user code. *executor.Executor implements it.
type runner interface {
	Execute(ctx context.Context, req *executor.ExecutionRequest) *executor.ExecutionResult
}

// locker admits executions through the Redis semaphore. *concurrency.Manager implements it.
type locker interface {
	Acquire(ctx context.Context, functionID, executionID string, maxConcurrency int) (queuedMs int64, err error)
	Release(ctx context.Context, functionID, executionID string) error
	MarkForCancellation(ctx context.Context, executionID string) error
}

type Handler struct {
	executor    runner
	concurrency locker
	defaults    fnconfig.Defaults
	queue       async.Queuer
	runs        *runStops
}

func New(exec *executor.Executor, concMgr *concurrency.Manager) *Handler {
	return NewWithDefaults(exec, concMgr, fnconfig.LoadDefaults())
}

func NewWithDefaults(exec *executor.Executor, concMgr *concurrency.Manager, defaults fnconfig.Defaults) *Handler {
	return &Handler{
		executor:    exec,
		concurrency: concMgr,
		defaults:    defaults,
		runs:        newRunStops(),
	}
}

// SetQueue attaches the durable async queue. Without it the async endpoints
// answer 503 and the sync path is unchanged.
func (h *Handler) SetQueue(q async.Queuer) {
	h.queue = q
}

func (h *Handler) effectiveDefaults() fnconfig.Defaults {
	if h.defaults.TimeoutSec == 0 || h.defaults.MemoryMB == 0 || h.defaults.Handler == "" {
		return fnconfig.Builtin()
	}
	return h.defaults
}

func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":    "ok",
		"service":   "compute-plane",
		"isolation": isolate.Current(),
	})
}

type StartExecutionRequest struct {
	ExecutionID    string                 `json:"executionId"`
	FunctionID     string                 `json:"functionId"`
	Version        string                 `json:"version"`
	Event          map[string]interface{} `json:"event"`
	TimeoutSec     json.RawMessage        `json:"timeoutSec"`
	MemoryMB       json.RawMessage        `json:"memoryMb"`
	CPUs           json.RawMessage        `json:"cpus"`
	Handler        json.RawMessage        `json:"handler"`
	EnvVars        json.RawMessage        `json:"envVars"`
	Runtime        string                 `json:"runtime,omitempty"`
	MaxConcurrency int                    `json:"maxConcurrency,omitempty"`
}

type StartExecutionResponse struct {
	ExecutionID string      `json:"executionId"`
	Success     bool        `json:"success"`
	Output      interface{} `json:"output"`
	Error       string      `json:"error,omitempty"`
	ExitCode    int         `json:"exitCode"`
	DurationMs  int64       `json:"durationMs"`
	QueuedMs    int64       `json:"queuedMs"`
	ColdStart   bool        `json:"coldStart"`
	InitMs      int64       `json:"initMs"`
}

func (h *Handler) StartExecution(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req StartExecutionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		msg := "JSON inválido"
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			msg = "El cuerpo de la solicitud supera 1 MB"
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}

	ctx := ntrace.ExtractHTTP(r.Context(), r.Header)
	ctx, span := ntrace.Start(ctx, "nimbus.executions", trace.WithSpanKind(trace.SpanKindServer))
	span.SetAttributes(
		attribute.String("nimbus.function_id", req.FunctionID),
		attribute.String("nimbus.mode", "sync"),
	)
	defer span.End()

	status, payload := h.dispatch(ctx, req, "sync")
	span.SetAttributes(attribute.Int("http.response.status_code", status))
	if status >= 500 {
		span.SetStatus(codes.Error, http.StatusText(status))
	}
	if status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "5")
	}
	writeJSON(w, status, payload)
}

// dispatch validates, takes a concurrency slot and runs the container.
// Sync HTTP and the async worker both go through here.
// mode is "sync" or "async" and is a metric label, not a trace id.
func (h *Handler) dispatch(ctx context.Context, req StartExecutionRequest, mode string) (int, any) {
	resolved, status, payload := h.validate(req)
	if status != 0 {
		return status, payload
	}

	ntrace.Logf(ctx, "[handler] StartExecution: %s (fn: %s, v: %s, runtime: %s, timeout: %ds, memory: %dMB, cpus: %s, handler: %s, env: %d, mode: %s)",
		req.ExecutionID, req.FunctionID, req.Version, resolved.Runtime, resolved.TimeoutSec, resolved.MemoryMB, cpusLabel(resolved), resolved.Handler, len(resolved.Env), mode)
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("nimbus.runtime", resolved.Runtime))

	maxConcurrency := req.MaxConcurrency
	if maxConcurrency < 0 {
		maxConcurrency = 0
	}
	qctx, qspan := ntrace.Start(ctx, "queue.wait")
	qspan.SetAttributes(attribute.String("nimbus.function_id", req.FunctionID))
	queuedMs, err := h.concurrency.Acquire(qctx, req.FunctionID, req.ExecutionID, maxConcurrency)
	qspan.SetAttributes(attribute.Int64("nimbus.queued_ms", queuedMs))
	if err != nil {
		ntrace.Fail(qspan, err)
	}
	qspan.End()
	metrics.ObserveQueueWait(req.FunctionID, queuedMs)
	if err != nil {
		switch {
		case errors.Is(err, concurrency.ErrQueueFull):
			metrics.Rejection(req.FunctionID, "queue_full")
		case errors.Is(err, concurrency.ErrWaitTimeout):
			metrics.Rejection(req.FunctionID, "queue_timeout")
		}
		return admissionResult(req.ExecutionID, queuedMs, err)
	}

	metrics.ActiveInc(req.FunctionID)
	defer func() {
		metrics.ActiveDec(req.FunctionID)
		if releaseErr := h.concurrency.Release(context.WithoutCancel(ctx), req.FunctionID, req.ExecutionID); releaseErr != nil {
			log.Printf("[handler] Error liberando slot: %v", releaseErr)
		}
	}()

	execReq := &executor.ExecutionRequest{
		ExecutionID: req.ExecutionID,
		FunctionID:  req.FunctionID,
		Version:     req.Version,
		Event:       req.Event,
		Config:      resolved,
	}

	// The request context stays alive for the HTTP call. Cancel is a second
	// request, so it has to abort this one explicitly.
	runCtx, stop := context.WithCancelCause(ctx)
	if h.runs != nil {
		h.runs.track(req.ExecutionID, stop)
	}
	if alreadyCancelled(h.concurrency, ctx, req.ExecutionID) {
		stop(cancel.ErrRun)
	}
	result := h.executor.Execute(runCtx, execReq)
	wasCancelled := cancel.Is(runCtx) || (result != nil && (result.ExitCode == cancel.ExitCode || result.Error == cancel.Message))
	if h.runs != nil {
		h.runs.untrack(req.ExecutionID)
	}
	if !wasCancelled {
		stop(context.Canceled)
	}
	if result == nil {
		result = &executor.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 1}
	}
	if wasCancelled {
		result.Success = false
		result.Output = nil
		result.Error = cancel.Message
		result.ExitCode = cancel.ExitCode
	}
	resp := StartExecutionResponse{
		ExecutionID: result.ExecutionID,
		Success:     result.Success,
		Output:      result.Output,
		Error:       result.Error,
		ExitCode:    result.ExitCode,
		DurationMs:  result.DurationMs,
		QueuedMs:    queuedMs,
		ColdStart:   result.ColdStart,
		InitMs:      result.InitMs,
	}
	outcome := metrics.Classify(result.Success, result.ExitCode, result.Error)
	metrics.RecordInvocation(req.FunctionID, mode, outcome, resolved.Runtime, result.DurationMs, result.ColdStart, result.InitMs)
	if wasCancelled {
		log.Printf("[handler] Ejecución %s cancelada (duration: %dms)", req.ExecutionID, result.DurationMs)
		return http.StatusConflict, resp
	}
	if !result.Success && result.ExitCode == 124 {
		return http.StatusGatewayTimeout, resp
	}
	return http.StatusOK, resp
}

// cancelState is implemented by *concurrency.Manager. Tests that use a fake
// locker do not have to report a flag.
type cancelState interface {
	IsCancelled(ctx context.Context, executionID string) bool
}

func alreadyCancelled(l locker, ctx context.Context, executionID string) bool {
	checker, ok := l.(cancelState)
	if !ok {
		return false
	}
	return checker.IsCancelled(ctx, executionID)
}

func (h *Handler) validate(req StartExecutionRequest) (fnconfig.Resolved, int, any) {
	if req.ExecutionID == "" || req.FunctionID == "" || req.Version == "" {
		return fnconfig.Resolved{}, http.StatusBadRequest, map[string]string{
			"error": "executionId, functionId y version son requeridos",
		}
	}
	resolved, err := fnconfig.Resolve(fnconfig.Raw{
		TimeoutSec: req.TimeoutSec,
		MemoryMB:   req.MemoryMB,
		CPUs:       req.CPUs,
		Handler:    req.Handler,
		EnvVars:    req.EnvVars,
	}, h.effectiveDefaults())
	if err != nil {
		return fieldRejection(req.ExecutionID, err)
	}
	rt, err := runtime.Normalize(req.Runtime)
	if err != nil {
		return fieldRejection(req.ExecutionID, err)
	}
	resolved.Runtime = rt
	// Go ships a binary, not a file.function export. An omitted handler is
	// the binary "bootstrap" at the root of the zip. An explicit handler is
	// that relative path ("bootstrap", "bin/hello").
	if rt == runtime.Go1x && handlerOmitted(req.Handler) {
		resolved.Handler = "bootstrap"
		resolved.HandlerFile = "bootstrap"
		resolved.HandlerExport = "bootstrap"
	}
	return resolved, 0, nil
}

func handlerOmitted(raw json.RawMessage) bool {
	text := strings.TrimSpace(string(raw))
	return text == "" || text == "null"
}

func fieldRejection(executionID string, err error) (fnconfig.Resolved, int, any) {
	var ferr *fnconfig.FieldError
	if errors.As(err, &ferr) {
		log.Printf("[handler] Configuración rechazada para %s: %s", executionID, ferr.Message)
		return fnconfig.Resolved{}, http.StatusBadRequest, map[string]string{
			"error": ferr.Message,
			"field": ferr.Field,
		}
	}
	log.Printf("[handler] Configuración rechazada para %s", executionID)
	return fnconfig.Resolved{}, http.StatusBadRequest, map[string]string{"error": "Configuración inválida"}
}

type CancelExecutionResponse struct {
	ExecutionID string `json:"executionId"`
	Cancelled   bool   `json:"cancelled"`
	Message     string `json:"message"`
}

func cpusLabel(cfg fnconfig.Resolved) string {
	if cfg.CPUsFlag == "" {
		return "sin límite"
	}
	return cfg.CPUsFlag
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(payload)
}

func admissionResult(executionID string, queuedMs int64, err error) (int, any) {
	switch {
	case errors.Is(err, concurrency.ErrQueueFull):
		log.Printf("[handler] Cola llena, ejecución %s rechazada", executionID)
		return http.StatusTooManyRequests, map[string]interface{}{
			"error":       "Cola llena para esta función",
			"executionId": executionID,
			"retryAfter":  5,
			"reason":      "queue_full",
			"queuedMs":    queuedMs,
		}
	case errors.Is(err, concurrency.ErrWaitTimeout):
		log.Printf("[handler] Tiempo de espera en cola agotado para %s", executionID)
		return http.StatusTooManyRequests, map[string]interface{}{
			"error":       "Tiempo máximo de espera en cola excedido",
			"executionId": executionID,
			"retryAfter":  5,
			"reason":      "queue_timeout",
			"queuedMs":    queuedMs,
		}
	case errors.Is(err, concurrency.ErrCancelled):
		log.Printf("[handler] Ejecución %s cancelada en la cola", executionID)
		return http.StatusConflict, map[string]interface{}{
			"error":       "Ejecución cancelada mientras esperaba en la cola",
			"executionId": executionID,
			"queuedMs":    queuedMs,
		}
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		return http.StatusRequestTimeout, map[string]interface{}{
			"error":       "La solicitud de ejecución fue cancelada",
			"executionId": executionID,
			"queuedMs":    queuedMs,
		}
	default:
		log.Printf("[handler] Error de admisión: %v", err)
		return http.StatusInternalServerError, map[string]interface{}{
			"error":       "Error interno de admisión",
			"executionId": executionID,
		}
	}
}

func (h *Handler) CancelExecution(w http.ResponseWriter, r *http.Request) {
	executionID := chi.URLParam(r, "id")
	if executionID == "" {
		http.Error(w, `{"error": "ID de ejecución requerido"}`, http.StatusBadRequest)
		return
	}

	log.Printf("[handler] CancelExecution: %s", executionID)

	err := h.concurrency.MarkForCancellation(r.Context(), executionID)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(CancelExecutionResponse{
			ExecutionID: executionID,
			Cancelled:   false,
			Message:     err.Error(),
		})
		return
	}
	// Queued and pending executions observe the Redis flag. A running one
	// is in this process: abort kills its container and discards a warm slot.
	if h.runs != nil {
		h.runs.abort(executionID)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(CancelExecutionResponse{
		ExecutionID: executionID,
		Cancelled:   true,
		Message:     "Ejecución marcada para cancelación",
	})
}
