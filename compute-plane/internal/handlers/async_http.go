package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/PedroCarassale/nimbus/compute-plane/internal/async"
	"github.com/go-chi/chi/v5"
)

type asyncEnqueueRequest struct {
	InvocationID string `json:"invocationId"`
	StartExecutionRequest
}

// EnqueueAsync validates the execution and leaves it on the durable stream.
// The HTTP call returns as soon as Redis accepts the job.
func (h *Handler) EnqueueAsync(w http.ResponseWriter, r *http.Request) {
	if h.queue == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "La cola async no está disponible",
		})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		msg := "JSON inválido"
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			msg = "El cuerpo de la solicitud supera 1 MB"
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	var body asyncEnqueueRequest
	if err := json.Unmarshal(raw, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "JSON inválido"})
		return
	}
	if body.InvocationID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invocationId es requerido",
		})
		return
	}
	if _, status, payload := h.validate(body.StartExecutionRequest); status != 0 {
		writeJSON(w, status, payload)
		return
	}
	executionID, duplicate, err := h.queue.Enqueue(r.Context(), body.InvocationID, body.ExecutionID, raw)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "No se pudo encolar la invocación",
		})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"invocationId": body.InvocationID,
		"executionId":  executionID,
		"status":       "queued",
		"duplicate":    duplicate,
	})
}

// ListDLQ returns invocations that exhausted their attempts.
func (h *Handler) ListDLQ(w http.ResponseWriter, r *http.Request) {
	if h.queue == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "La cola async no está disponible",
		})
		return
	}
	entries, err := h.queue.ListDLQ(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "No se pudo leer la DLQ",
		})
		return
	}
	if entries == nil {
		entries = []async.DLQEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

// RedriveDLQ puts one dead letter back on the job stream as a fresh attempt.
func (h *Handler) RedriveDLQ(w http.ResponseWriter, r *http.Request) {
	if h.queue == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "La cola async no está disponible",
		})
		return
	}
	id := chi.URLParam(r, "id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id requerido"})
		return
	}
	entry, err := h.queue.Redrive(r.Context(), id)
	if errors.Is(err, async.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "La invocación no está en la DLQ",
		})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "No se pudo reencolar la invocación",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":            entry.ID,
		"invocationId":  entry.InvocationID,
		"executionId":   entry.ExecutionID,
		"status":        "queued",
		"previousError": entry.LastError,
	})
}

// RunJob is the async worker's view of dispatch.
func (h *Handler) RunJob(ctx context.Context, body json.RawMessage) async.RunResult {
	var req StartExecutionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return async.RunResult{Kind: async.KindValidation, Error: "JSON inválido"}
	}
	status, payload := h.dispatch(ctx, req)
	return classify(status, payload)
}

func classify(status int, payload any) async.RunResult {
	resp, hasResp := payload.(StartExecutionResponse)
	out := async.RunResult{
		Error:      errorText(payload),
		ExitCode:   resp.ExitCode,
		Output:     resp.Output,
		DurationMs: resp.DurationMs,
		ColdStart:  resp.ColdStart,
		InitMs:     resp.InitMs,
		QueuedMs:   resp.QueuedMs,
	}
	switch {
	case status == http.StatusBadRequest:
		out.Kind = async.KindValidation
	case status == http.StatusConflict:
		out.Kind = async.KindFatal
	case hasResp && resp.Success:
		out.Kind = async.KindOK
		out.Error = ""
	default:
		out.Kind = async.KindRetry
		if out.ExitCode == 0 {
			out.ExitCode = status
		}
		if out.Error == "" {
			out.Error = "la ejecución falló"
		}
	}
	return out
}

func errorText(payload any) string {
	switch body := payload.(type) {
	case StartExecutionResponse:
		return body.Error
	case map[string]string:
		return body["error"]
	case map[string]interface{}:
		if text, ok := body["error"].(string); ok {
			return text
		}
	}
	return ""
}
