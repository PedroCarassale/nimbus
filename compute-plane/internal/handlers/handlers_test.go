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
	"time"

	"github.com/PedroCarassale/nimbus/compute-plane/internal/cancel"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/concurrency"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/executor"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/metrics"
	"github.com/alicebob/miniredis/v2"
	"github.com/go-chi/chi/v5"
	"github.com/redis/go-redis/v9"
)

type fakeRunner struct {
	result *executor.ExecutionResult
	calls  int
	last   *executor.ExecutionRequest
}

func (f *fakeRunner) Execute(_ context.Context, req *executor.ExecutionRequest) *executor.ExecutionResult {
	f.calls++
	cp := *req
	f.last = &cp
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
	acquireCalls int
	queuedMs     int64
	lastMax      int
	releaseCalls int
	cancelErr    error
	cancelled    []string
}

func (f *fakeLocker) Acquire(_ context.Context, _, _ string, maxConcurrency int) (int64, error) {
	f.mu.Lock()
	f.lastMax = maxConcurrency
	f.acquireCalls++
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

// blockingRunner waits until ctx is cancelled, then returns success.
// A real cancel has to override that success: the handler already finished
// from the runner's point of view only because we killed its context.
type blockingRunner struct {
	started chan struct{}
	once    sync.Once
}

func (b *blockingRunner) Execute(ctx context.Context, req *executor.ExecutionRequest) *executor.ExecutionResult {
	if b.started != nil {
		b.once.Do(func() { close(b.started) })
	}
	<-ctx.Done()
	return &executor.ExecutionResult{
		ExecutionID: req.ExecutionID,
		Success:     true,
		Output:      map[string]interface{}{"slept": 2500},
		ExitCode:    0,
		DurationMs:  2500,
	}
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
	if body["coldStart"] != false || body["initMs"].(float64) != 0 {
		t.Fatalf("cold start fields %#v", body)
	}
}

func TestStartExecutionPropagatesWarmMeta(t *testing.T) {
	h := &Handler{
		executor: &fakeRunner{result: &executor.ExecutionResult{
			Success:   true,
			ExitCode:  0,
			ColdStart: true,
			InitMs:    180,
			Output:    map[string]interface{}{"ok": true},
		}},
		concurrency: &fakeLocker{acquire: true},
	}
	rr := postJSON(t, newRouter(h), "/executions", `{
		"executionId":"e1","functionId":"f1","version":"v1"
	}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d %s", rr.Code, rr.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["coldStart"] != true || body["initMs"].(float64) != 180 {
		t.Fatalf("body %#v", body)
	}
}

func TestStartExecutionRecordsMetrics(t *testing.T) {
	const fn = "fn-handler-metrics"
	beforeOK := metrics.Sample("nimbus_invocations_total", map[string]string{
		"function_id": fn, "status": "success", "mode": "sync",
	})
	beforeCold := metrics.Sample("nimbus_cold_starts_total", map[string]string{"function_id": fn})
	beforeRej := metrics.Sample("nimbus_rejections_total", map[string]string{
		"function_id": fn, "reason": "queue_full",
	})

	okHandler := &Handler{
		executor: &fakeRunner{result: &executor.ExecutionResult{
			Success: true, ExitCode: 0, ColdStart: true, InitMs: 20, DurationMs: 30,
		}},
		concurrency: &fakeLocker{acquire: true, queuedMs: 5},
	}
	rr := postJSON(t, newRouter(okHandler), "/executions", `{
		"executionId":"e-metrics","functionId":"`+fn+`","version":"v1"
	}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d %s", rr.Code, rr.Body.String())
	}
	if got := metrics.Sample("nimbus_invocations_total", map[string]string{
		"function_id": fn, "status": "success", "mode": "sync",
	}); got-beforeOK != 1 {
		t.Fatalf("invocations %v", got-beforeOK)
	}
	if got := metrics.Sample("nimbus_cold_starts_total", map[string]string{"function_id": fn}); got-beforeCold != 1 {
		t.Fatalf("cold %v", got-beforeCold)
	}

	rej := &Handler{executor: &fakeRunner{}, concurrency: &fakeLocker{acquire: false}}
	rr = postJSON(t, newRouter(rej), "/executions", `{
		"executionId":"e-rej","functionId":"`+fn+`","version":"v1"
	}`)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d %s", rr.Code, rr.Body.String())
	}
	if got := metrics.Sample("nimbus_rejections_total", map[string]string{
		"function_id": fn, "reason": "queue_full",
	}); got-beforeRej != 1 {
		t.Fatalf("rejections %v", got-beforeRej)
	}
	if got := metrics.Sample("nimbus_active_executions", map[string]string{"function_id": fn}); got != 0 {
		t.Fatalf("active leaked: %v", got)
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

func TestStartExecutionRejectsInvalidConfig(t *testing.T) {
	runner := &fakeRunner{}
	locker := &fakeLocker{acquire: true}
	h := &Handler{executor: runner, concurrency: locker}
	router := newRouter(h)

	cases := []struct {
		body  string
		field string
	}{
		{`{"executionId":"e1","functionId":"f1","version":"v1","timeoutSec":301}`, "timeoutSec"},
		{`{"executionId":"e1","functionId":"f1","version":"v1","memoryMb":8}`, "memoryMb"},
		{`{"executionId":"e1","functionId":"f1","version":"v1","cpus":9}`, "cpus"},
		{`{"executionId":"e1","functionId":"f1","version":"v1","handler":"../x.main"}`, "handler"},
		{`{"executionId":"e1","functionId":"f1","version":"v1","envVars":{"NIMBUS_SECRET":"super-secret-value"}}`, "envVars"},
		{`{"executionId":"e1","functionId":"f1","version":"v1","envVars":{"BAD-NAME":"x"}}`, "envVars"},
		{`{"executionId":"e1","functionId":"f1","version":"v1","runtime":"ruby"}`, "runtime"},
	}
	for _, tc := range cases {
		rr := postJSON(t, router, "/executions", tc.body)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status %d for %s body %s", rr.Code, tc.body, rr.Body.String())
		}
		var body map[string]string
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["field"] != tc.field || body["error"] == "" {
			t.Fatalf("body %#v", body)
		}
		if strings.Contains(rr.Body.String(), "super-secret-value") {
			t.Fatalf("response leaked env value: %s", rr.Body.String())
		}
	}
	if runner.calls != 0 || locker.acquireCalls != 0 || locker.releaseCalls != 0 {
		t.Fatalf("invalid config ran: calls=%d acquire=%d release=%d", runner.calls, locker.acquireCalls, locker.releaseCalls)
	}
}

func TestStartExecutionForwardsRuntimeConfig(t *testing.T) {
	runner := &fakeRunner{}
	locker := &fakeLocker{acquire: true}
	h := &Handler{executor: runner, concurrency: locker}
	rr := postJSON(t, newRouter(h), "/executions", `{
		"executionId":"e1","functionId":"f1","version":"v1",
		"timeoutSec":10,"memoryMb":256,"cpus":0.25,
		"handler":"src/app.main",
		"envVars":{"GREETING":"super-secret-value"}
	}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d %s", rr.Code, rr.Body.String())
	}
	if runner.last == nil {
		t.Fatal("missing request")
	}
	cfg := runner.last.Config
	if cfg.TimeoutSec != 10 || cfg.MemoryMB != 256 || cfg.CPUsFlag != "0.25" {
		t.Fatalf("config %+v", cfg)
	}
	if cfg.Handler != "src/app.main" || cfg.HandlerFile != "src/app" || cfg.HandlerExport != "main" {
		t.Fatalf("handler %+v", cfg)
	}
	if cfg.Runtime != "nodejs20" {
		t.Fatalf("default runtime %+v", cfg.Runtime)
	}
	if len(cfg.Env) != 1 || cfg.Env[0].Name != "GREETING" || cfg.Env[0].Value != "super-secret-value" {
		t.Fatalf("env %+v", cfg.Env)
	}
}

func TestRuntimeFieldSelectsPythonAndGo(t *testing.T) {
	runner := &fakeRunner{}
	locker := &fakeLocker{acquire: true}
	h := &Handler{executor: runner, concurrency: locker}
	router := newRouter(h)

	rr := postJSON(t, router, "/executions", `{
		"executionId":"e1","functionId":"f1","version":"v1","runtime":"python3.12","handler":"index.handler"
	}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("python status %d %s", rr.Code, rr.Body.String())
	}
	if runner.last.Config.Runtime != "python3.12" || runner.last.Config.Handler != "index.handler" || runner.last.Config.HandlerFile != "index" {
		t.Fatalf("python cfg %+v", runner.last.Config)
	}

	rr = postJSON(t, router, "/executions", `{
		"executionId":"e2","functionId":"f1","version":"v1","runtime":"go1.x"
	}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("go status %d %s", rr.Code, rr.Body.String())
	}
	cfg := runner.last.Config
	if cfg.Runtime != "go1.x" || cfg.Handler != "bootstrap" {
		t.Fatalf("omitted go handler %+v", cfg)
	}

	rr = postJSON(t, router, "/executions", `{
		"executionId":"e3","functionId":"f1","version":"v1","runtime":"go1.x","handler":"bin/hello"
	}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("go path status %d %s", rr.Code, rr.Body.String())
	}
	if runner.last.Config.Handler != "bin/hello" {
		t.Fatalf("explicit go handler %+v", runner.last.Config)
	}
}

func TestCancelRunningDoesNotReportSuccess(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	mgr := concurrency.NewManager(rdb, concurrency.Config{
		MaxConcurrency: 2,
		QueueMax:       4,
		GlobalMax:      8,
		MaxWait:        time.Second,
		Lease:          time.Minute,
	})
	runner := &blockingRunner{started: make(chan struct{})}
	h := &Handler{executor: runner, concurrency: mgr, runs: newRunStops()}
	router := newRouter(h)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/executions", strings.NewReader(`{
			"executionId":"run-1","functionId":"fn","version":"v1","timeoutSec":15,"event":{"sleepMs":2500}
		}`))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		done <- rr
	}()
	select {
	case <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("execution did not start")
	}
	cancelRR := postJSON(t, router, "/executions/run-1/cancel", "")
	if cancelRR.Code != http.StatusOK || !strings.Contains(cancelRR.Body.String(), `"cancelled":true`) {
		t.Fatalf("cancel %d %s", cancelRR.Code, cancelRR.Body.String())
	}
	var rr *httptest.ResponseRecorder
	select {
	case rr = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled execution did not return")
	}
	if rr.Code != http.StatusConflict {
		t.Fatalf("status %d %s", rr.Code, rr.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["success"] != false || body["error"] != cancel.Message {
		t.Fatalf("body %s", rr.Body.String())
	}
	if body["exitCode"] != float64(cancel.ExitCode) {
		t.Fatalf("exit %v", body["exitCode"])
	}
	if strings.Contains(rr.Body.String(), `"slept"`) {
		t.Fatalf("handler output survived the cancel: %s", rr.Body.String())
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
