package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/PedroCarassale/nimbus/compute-plane/internal/concurrency"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/executor"
	"github.com/go-chi/chi/v5"
)

type fakeRunner struct {
	result *executor.ExecutionResult
	calls  int
}

func (f *fakeRunner) Execute(_ context.Context, req *executor.ExecutionRequest) *executor.ExecutionResult {
	f.calls++
	if f.result != nil {
		out := *f.result
		if out.ExecutionID == "" {
			out.ExecutionID = req.ExecutionID
		}
		return &out
	}
	return &executor.ExecutionResult{
		ExecutionID: req.ExecutionID,
		Success:     true,
		Output:      map[string]interface{}{"ok": true},
		ExitCode:    0,
		DurationMs:  12,
	}
}

type fakeLocker struct {
	mu           sync.Mutex
	acquire      bool
	acquireErr   error
	queuedMs     int64
	lastMax      int
	releaseCalls int
	cancelErr    error
	cancelled    []string
}

func (f *fakeLocker) Acquire(_ context.Context, _, _ string, maxConcurrency int) (int64, error) {
	f.mu.Lock()
	f.lastMax = maxConcurrency
	f.mu.Unlock()
	if f.acquireErr != nil {
		return f.queuedMs, f.acquireErr
	}
	if !f.acquire {
		return f.queuedMs, concurrency.ErrQueueFull
	}
	return f.queuedMs, nil
}

func (f *fakeLocker) Release(context.Context, string, string) error {
	f.mu.Lock()
	f.releaseCalls++
	f.mu.Unlock()
	return nil
}

func (f *fakeLocker) MarkForCancellation(_ context.Context, executionID string) error {
	if f.cancelErr != nil {
		return f.cancelErr
	}
	f.mu.Lock()
	f.cancelled = append(f.cancelled, executionID)
	f.mu.Unlock()
	return nil
}

func newRouter(h *Handler) http.Handler {
	r := chi.NewRouter()
	r.Get("/health", h.Health)
	r.Post("/executions", h.StartExecution)
	r.Post("/executions/{id}/cancel", h.CancelExecution)
	return r
}

func postJSON(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestHealth(t *testing.T) {
	h := &Handler{executor: &fakeRunner{}, concurrency: &fakeLocker{acquire: true}}
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rr := httptest.NewRecorder()
	newRouter(h).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "ok" || body["service"] != "compute-plane" {
		t.Fatalf("body %#v", body)
	}
}

func TestStartExecutionSuccess(t *testing.T) {
	runner := &fakeRunner{}
	locker := &fakeLocker{acquire: true}
	h := &Handler{executor: runner, concurrency: locker}
	rr := postJSON(t, newRouter(h), "/executions", `{
		"executionId":"e1","functionId":"f1","version":"v1","event":{"name":"Nimbus"}
	}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["success"] != true || body["executionId"] != "e1" {
		t.Fatalf("body %#v", body)
	}
	if runner.calls != 1 || locker.releaseCalls != 1 {
		t.Fatalf("calls=%d release=%d", runner.calls, locker.releaseCalls)
	}
}

func TestStartExecutionBadRequest(t *testing.T) {
	h := &Handler{executor: &fakeRunner{}, concurrency: &fakeLocker{acquire: true}}
	router := newRouter(h)

	rr := postJSON(t, router, "/executions", `{`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid json status %d", rr.Code)
	}

	rr = postJSON(t, router, "/executions", `{"executionId":"e1"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("missing fields status %d", rr.Code)
	}
	body, _ := io.ReadAll(rr.Body)
	if !strings.Contains(string(body), "requeridos") {
		t.Fatalf("body %s", body)
	}
}

func TestStartExecutionBusy(t *testing.T) {
	runner := &fakeRunner{}
	locker := &fakeLocker{acquire: false}
	h := &Handler{executor: runner, concurrency: locker}
	rr := postJSON(t, newRouter(h), "/executions", `{
		"executionId":"e1","functionId":"f1","version":"v1"
	}`)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d", rr.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["retryAfter"].(float64) != 5 || body["reason"] != "queue_full" {
		t.Fatalf("body %#v", body)
	}
	if body["error"] != "Cola llena para esta función" {
		t.Fatalf("body %#v", body)
	}
	if runner.calls != 0 || locker.releaseCalls != 0 {
		t.Fatalf("should not run or release, calls=%d release=%d", runner.calls, locker.releaseCalls)
	}
}

func TestStartExecutionLockError(t *testing.T) {
	h := &Handler{
		executor:    &fakeRunner{},
		concurrency: &fakeLocker{acquireErr: context.DeadlineExceeded},
	}
	rr := postJSON(t, newRouter(h), "/executions", `{
		"executionId":"e1","functionId":"f1","version":"v1"
	}`)
	if rr.Code != http.StatusRequestTimeout {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}

	h = &Handler{
		executor:    &fakeRunner{},
		concurrency: &fakeLocker{acquireErr: context.Canceled},
	}
	rr = postJSON(t, newRouter(h), "/executions", `{
		"executionId":"e1","functionId":"f1","version":"v1"
	}`)
	if rr.Code != http.StatusRequestTimeout {
		t.Fatalf("cancel status %d", rr.Code)
	}

	h = &Handler{
		executor:    &fakeRunner{},
		concurrency: &fakeLocker{acquireErr: io.EOF},
	}
	rr = postJSON(t, newRouter(h), "/executions", `{
		"executionId":"e1","functionId":"f1","version":"v1"
	}`)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("internal status %d", rr.Code)
	}
}

func TestStartExecutionQueueTimeoutAndCancel(t *testing.T) {
	h := &Handler{
		executor:    &fakeRunner{},
		concurrency: &fakeLocker{acquireErr: concurrency.ErrWaitTimeout, queuedMs: 40},
	}
	rr := postJSON(t, newRouter(h), "/executions", `{
		"executionId":"e1","functionId":"f1","version":"v1"
	}`)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("timeout status %d", rr.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["reason"] != "queue_timeout" || body["queuedMs"].(float64) != 40 {
		t.Fatalf("body %#v", body)
	}

	h = &Handler{
		executor:    &fakeRunner{},
		concurrency: &fakeLocker{acquireErr: concurrency.ErrCancelled},
	}
	rr = postJSON(t, newRouter(h), "/executions", `{
		"executionId":"e1","functionId":"f1","version":"v1"
	}`)
	if rr.Code != http.StatusConflict {
		t.Fatalf("cancel status %d %s", rr.Code, rr.Body.String())
	}
}

func TestStartExecutionMaxConcurrencyOverride(t *testing.T) {
	locker := &fakeLocker{acquire: true, queuedMs: 12}
	h := &Handler{executor: &fakeRunner{}, concurrency: locker}
	rr := postJSON(t, newRouter(h), "/executions", `{
		"executionId":"e1","functionId":"f1","version":"v1","maxConcurrency":4
	}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d %s", rr.Code, rr.Body.String())
	}
	if locker.lastMax != 4 {
		t.Fatalf("maxConcurrency forwarded as %d", locker.lastMax)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["queuedMs"].(float64) != 12 {
		t.Fatalf("body %#v", body)
	}

	locker.lastMax = -1
	rr = postJSON(t, newRouter(h), "/executions", `{
		"executionId":"e2","functionId":"f1","version":"v1","maxConcurrency":-3
	}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	if locker.lastMax != 0 {
		t.Fatalf("negative override = %d, want default (0)", locker.lastMax)
	}
}

func TestStartExecutionTimeoutAndError(t *testing.T) {
	router := func(result *executor.ExecutionResult) *httptest.ResponseRecorder {
		h := &Handler{
			executor:    &fakeRunner{result: result},
			concurrency: &fakeLocker{acquire: true},
		}
		return postJSON(t, newRouter(h), "/executions", `{
			"executionId":"e1","functionId":"f1","version":"v1"
		}`)
	}

	rr := router(&executor.ExecutionResult{Success: false, ExitCode: 124, Error: "Timeout excedido (5s)"})
	if rr.Code != http.StatusGatewayTimeout {
		t.Fatalf("timeout status %d", rr.Code)
	}

	rr = router(&executor.ExecutionResult{Success: false, ExitCode: 1, Error: "fallo de prueba"})
	if rr.Code != http.StatusOK {
		t.Fatalf("handler error status %d", rr.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["success"] != false || body["exitCode"].(float64) != 1 {
		t.Fatalf("body %#v", body)
	}
}

func TestCancelExecution(t *testing.T) {
	locker := &fakeLocker{}
	h := &Handler{executor: &fakeRunner{}, concurrency: locker}
	router := newRouter(h)

	rr := postJSON(t, router, "/executions/exec-9/cancel", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d %s", rr.Code, rr.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["cancelled"] != true || body["executionId"] != "exec-9" {
		t.Fatalf("body %#v", body)
	}
	if len(locker.cancelled) != 1 || locker.cancelled[0] != "exec-9" {
		t.Fatalf("cancelled %#v", locker.cancelled)
	}

	locker.cancelErr = context.Canceled
	rr = postJSON(t, router, "/executions/missing/cancel", "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("missing status %d", rr.Code)
	}
}
