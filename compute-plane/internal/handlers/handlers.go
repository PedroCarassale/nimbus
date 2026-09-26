package handlers

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/PedroCarassale/nimbus/compute-plane/internal/concurrency"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/executor"
	"github.com/go-chi/chi/v5"
)

type Handler struct {
	executor    *executor.Executor
	concurrency *concurrency.Manager
}

func New(exec *executor.Executor, concMgr *concurrency.Manager) *Handler {
	return &Handler{
		executor:    exec,
		concurrency: concMgr,
	}
}

func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "service": "compute-plane"})
}

type StartExecutionRequest struct {
	ExecutionID string                 `json:"executionId"`
	FunctionID  string                 `json:"functionId"`
	Version     string                 `json:"version"`
	Event       map[string]interface{} `json:"event"`
	TimeoutSec  int                    `json:"timeoutSec"`
	MemoryMB    int                    `json:"memoryMb"`
	Handler     string                 `json:"handler"`
}

type StartExecutionResponse struct {
	ExecutionID string      `json:"executionId"`
	Success     bool        `json:"success"`
	Output      interface{} `json:"output"`
	Error       string      `json:"error,omitempty"`
	ExitCode    int         `json:"exitCode"`
	DurationMs  int64       `json:"durationMs"`
}

func (h *Handler) StartExecution(w http.ResponseWriter, r *http.Request) {
	var req StartExecutionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error": "JSON inválido"}`, http.StatusBadRequest)
		return
	}

	if req.ExecutionID == "" || req.FunctionID == "" || req.Version == "" {
		http.Error(w, `{"error": "executionId, functionId y version son requeridos"}`, http.StatusBadRequest)
		return
	}

	log.Printf("[handler] StartExecution: %s (fn: %s, v: %s)", req.ExecutionID, req.FunctionID, req.Version)

	acquired, err := h.concurrency.AcquireLock(r.Context(), req.FunctionID, req.ExecutionID)
	if err != nil {
		log.Printf("[handler] Error adquiriendo lock: %v", err)
		http.Error(w, `{"error": "Error interno adquiriendo lock"}`, http.StatusInternalServerError)
		return
	}
	if !acquired {
		log.Printf("[handler] No se pudo adquirir lock para función %s (concurrencia máxima)", req.FunctionID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error":       "Función ocupada, reintente más tarde",
			"executionId": req.ExecutionID,
			"retryAfter":  5,
		})
		return
	}

	defer h.concurrency.ReleaseLock(r.Context(), req.FunctionID, req.ExecutionID)

	execReq := &executor.ExecutionRequest{
		ExecutionID: req.ExecutionID,
		FunctionID:  req.FunctionID,
		Version:     req.Version,
		Event:       req.Event,
		TimeoutSec:  req.TimeoutSec,
		MemoryMB:    req.MemoryMB,
		Handler:     req.Handler,
	}

	result := h.executor.Execute(r.Context(), execReq)

	resp := StartExecutionResponse{
		ExecutionID: result.ExecutionID,
		Success:     result.Success,
		Output:      result.Output,
		Error:       result.Error,
		ExitCode:    result.ExitCode,
		DurationMs:  result.DurationMs,
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
