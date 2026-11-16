package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/PedroCarassale/nimbus/compute-plane/internal/concurrency"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/executor"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/fnconfig"
	"github.com/go-chi/chi/v5"
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
}

func New(exec *executor.Executor, concMgr *concurrency.Manager) *Handler {
	return NewWithDefaults(exec, concMgr, fnconfig.LoadDefaults())
}

func NewWithDefaults(exec *executor.Executor, concMgr *concurrency.Manager, defaults fnconfig.Defaults) *Handler {
	return &Handler{
		executor:    exec,
		concurrency: concMgr,
		defaults:    defaults,
	}
}

func (h *Handler) effectiveDefaults() fnconfig.Defaults {
	if h.defaults.TimeoutSec == 0 || h.defaults.MemoryMB == 0 || h.defaults.Handler == "" {
		return fnconfig.Builtin()
	}
	return h.defaults
}

func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "service": "compute-plane"})
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

	if req.ExecutionID == "" || req.FunctionID == "" || req.Version == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "executionId, functionId y version son requeridos",
		})
		return
	}

	resolved, err := fnconfig.Resolve(fnconfig.Raw{
		TimeoutSec: req.TimeoutSec,
		MemoryMB:   req.MemoryMB,
		CPUs:       req.CPUs,
		Handler:    req.Handler,
		EnvVars:    req.EnvVars,
	}, h.effectiveDefaults())
	if err != nil {
		var ferr *fnconfig.FieldError
		if errors.As(err, &ferr) {
			log.Printf("[handler] Configuración rechazada para %s: %s", req.ExecutionID, ferr.Message)
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": ferr.Message,
				"field": ferr.Field,
			})
			return
		}
		log.Printf("[handler] Configuración rechazada para %s", req.ExecutionID)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Configuración inválida"})
		return
	}

	log.Printf("[handler] StartExecution: %s (fn: %s, v: %s, timeout: %ds, memory: %dMB, cpus: %s, handler: %s, env: %d)",
		req.ExecutionID, req.FunctionID, req.Version, resolved.TimeoutSec, resolved.MemoryMB, cpusLabel(resolved), resolved.Handler, len(resolved.Env))

	maxConcurrency := req.MaxConcurrency
	if maxConcurrency < 0 {
		maxConcurrency = 0
	}
	queuedMs, err := h.concurrency.Acquire(r.Context(), req.FunctionID, req.ExecutionID, maxConcurrency)
	if err != nil {
		writeAdmissionError(w, req.ExecutionID, queuedMs, err)
		return
	}

	defer func() {
		if releaseErr := h.concurrency.Release(context.WithoutCancel(r.Context()), req.FunctionID, req.ExecutionID); releaseErr != nil {
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

	result := h.executor.Execute(r.Context(), execReq)

	resp := StartExecutionResponse{
		ExecutionID: result.ExecutionID,
		Success:     result.Success,
		Output:      result.Output,
		Error:       result.Error,
		ExitCode:    result.ExitCode,
		DurationMs:  result.DurationMs,
		QueuedMs:    queuedMs,
	}

	w.Header().Set("Content-Type", "application/json")
	if !result.Success {
		if result.ExitCode == 124 {
			w.WriteHeader(http.StatusGatewayTimeout)
		} else {
			w.WriteHeader(http.StatusOK)
		}
	}
	json.NewEncoder(w).Encode(resp)
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

func writeAdmissionError(w http.ResponseWriter, executionID string, queuedMs int64, err error) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case errors.Is(err, concurrency.ErrQueueFull):
		log.Printf("[handler] Cola llena, ejecución %s rechazada", executionID)
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error":       "Cola llena para esta función",
			"executionId": executionID,
			"retryAfter":  5,
			"reason":      "queue_full",
			"queuedMs":    queuedMs,
		})
	case errors.Is(err, concurrency.ErrWaitTimeout):
		log.Printf("[handler] Tiempo de espera en cola agotado para %s", executionID)
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error":       "Tiempo máximo de espera en cola excedido",
			"executionId": executionID,
			"retryAfter":  5,
			"reason":      "queue_timeout",
			"queuedMs":    queuedMs,
		})
	case errors.Is(err, concurrency.ErrCancelled):
		log.Printf("[handler] Ejecución %s cancelada en la cola", executionID)
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error":       "Ejecución cancelada mientras esperaba en la cola",
			"executionId": executionID,
			"queuedMs":    queuedMs,
		})
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		w.WriteHeader(http.StatusRequestTimeout)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error":       "La solicitud de ejecución fue cancelada",
			"executionId": executionID,
			"queuedMs":    queuedMs,
		})
	default:
		log.Printf("[handler] Error de admisión: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error":       "Error interno de admisión",
			"executionId": executionID,
		})
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

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(CancelExecutionResponse{
		ExecutionID: executionID,
		Cancelled:   true,
		Message:     "Ejecución marcada para cancelación",
	})
}
