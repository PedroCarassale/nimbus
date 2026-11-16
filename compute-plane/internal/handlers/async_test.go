package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PedroCarassale/nimbus/compute-plane/internal/async"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/executor"
	"github.com/alicebob/miniredis/v2"
	"github.com/go-chi/chi/v5"
	"github.com/redis/go-redis/v9"
)

func asyncRouter(h *Handler) http.Handler {
	r := chi.NewRouter()
	r.Post("/executions", h.StartExecution)
	r.Post("/async/jobs", h.EnqueueAsync)
	r.Get("/dlq", h.ListDLQ)
	r.Post("/dlq/{id}/redrive", h.RedriveDLQ)
	return r
}

func testQueue(t *testing.T) *async.Queue {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	q := async.New(rdb, async.Config{
		Enabled:     true,
		MaxAttempts: 3,
		LockTTL:     1e9,
		ClaimIdle:   1e9,
	})
	if err := q.EnsureGroup(context.Background()); err != nil {
		t.Fatal(err)
	}
	return q
}

func TestEnqueueRejectsInvalidConfigWithoutQueueing(t *testing.T) {
	runner := &fakeRunner{}
	h := &Handler{executor: runner, concurrency: &fakeLocker{acquire: true}, queue: testQueue(t)}
	router := asyncRouter(h)
	rr := postJSON(t, router, "/async/jobs", `{
		"invocationId":"inv-1","executionId":"exec-1","functionId":"fn","version":"v1","timeoutSec":9999
	}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d %s", rr.Code, rr.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["field"] != "timeoutSec" {
		t.Fatalf("body %#v", body)
	}
	if runner.calls != 0 {
		t.Fatalf("runner calls=%d", runner.calls)
	}
	list := httptest.NewRecorder()
	router.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/dlq", nil))
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"entries":[]`) {
		t.Fatalf("dlq %d %s", list.Code, list.Body.String())
	}
}

func TestEnqueueReturns202AndSyncStillRuns(t *testing.T) {
	runner := &fakeRunner{result: &executor.ExecutionResult{Success: true, ExitCode: 0, Output: map[string]any{"ok": true}}}
	h := &Handler{executor: runner, concurrency: &fakeLocker{acquire: true}, queue: testQueue(t)}
	router := asyncRouter(h)

	rr := postJSON(t, router, "/async/jobs", `{
		"invocationId":"inv-1","executionId":"exec-1","functionId":"fn","version":"v1","event":{"name":"Nimbus"}
	}`)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status %d %s", rr.Code, rr.Body.String())
	}
	var accepted map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	if accepted["status"] != "queued" || accepted["executionId"] != "exec-1" || accepted["duplicate"] != false {
		t.Fatalf("body %#v", accepted)
	}
	if runner.calls != 0 {
		t.Fatalf("enqueue ran the function: %d", runner.calls)
	}

	again := postJSON(t, router, "/async/jobs", `{
		"invocationId":"inv-1","executionId":"exec-2","functionId":"fn","version":"v1"
	}`)
	if again.Code != http.StatusAccepted {
		t.Fatalf("dup status %d %s", again.Code, again.Body.String())
	}
	if !strings.Contains(again.Body.String(), `"duplicate":true`) || !strings.Contains(again.Body.String(), `"executionId":"exec-1"`) {
		t.Fatalf("dup body %s", again.Body.String())
	}

	syncResp := postJSON(t, router, "/executions", `{
		"executionId":"exec-sync","functionId":"fn","version":"v1","event":{"name":"Sync"}
	}`)
	if syncResp.Code != http.StatusOK {
		t.Fatalf("sync %d %s", syncResp.Code, syncResp.Body.String())
	}
	if runner.calls != 1 {
		t.Fatalf("sync calls=%d", runner.calls)
	}
}

func TestRedriveUnknownIs404(t *testing.T) {
	h := &Handler{executor: &fakeRunner{}, concurrency: &fakeLocker{acquire: true}, queue: testQueue(t)}
	req := httptest.NewRequest(http.MethodPost, "/dlq/missing/redrive", nil)
	rr := httptest.NewRecorder()
	asyncRouter(h).ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		body, _ := io.ReadAll(rr.Body)
		t.Fatalf("status %d %s", rr.Code, body)
	}
}
