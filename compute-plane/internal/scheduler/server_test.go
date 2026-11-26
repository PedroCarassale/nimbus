package scheduler

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PedroCarassale/nimbus/compute-plane/internal/fnconfig"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/registry"
	ntrace "github.com/PedroCarassale/nimbus/compute-plane/internal/trace"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/warm"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func testRig(t *testing.T) (*registry.Registry, *redis.Client, *Server) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	reg := registry.New(rdb, 6*time.Second)
	client := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{Timeout: 200 * time.Millisecond}).DialContext,
		},
	}
	return reg, rdb, New(reg, fnconfig.Builtin(), client)
}

type fakeWorker struct {
	id      string
	srv     *httptest.Server
	mu      sync.Mutex
	paths   []string
	parents []string
	bodies  []string
}

func newFake(t *testing.T, id string, status int, payload string) *fakeWorker {
	t.Helper()
	f := &fakeWorker{id: id}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		f.parents = append(f.parents, r.Header.Get("traceparent"))
		f.bodies = append(f.bodies, string(raw))
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Nimbus-Worker", id)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeWorker) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.paths)
}

func (f *fakeWorker) path(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i < 0 || i >= len(f.paths) {
		return ""
	}
	return f.paths[i]
}

func (f *fakeWorker) beat(t *testing.T, reg *registry.Registry, active int, keys ...string) {
	t.Helper()
	err := reg.Heartbeat(context.Background(), registry.Snapshot{
		WorkerID:         f.id,
		URL:              f.srv.URL,
		ActiveExecutions: active,
		MaxContainers:    8,
		IdleWarmKeys:     keys,
		State:            registry.StateHealthy,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func execBody() string {
	return `{"executionId":"e1","functionId":"fn-1","version":"v1","memoryMb":128,"handler":"index.handler","event":{"n":1}}`
}

func affinityOfTest() string {
	rawHandler, _ := json.Marshal("index.handler")
	resolved, err := fnconfig.Resolve(fnconfig.Raw{
		MemoryMB: json.RawMessage("128"),
		Handler:  rawHandler,
	}, fnconfig.Builtin())
	if err != nil {
		panic(err)
	}
	resolved.Runtime = "nodejs20"
	return warm.AffinityKey("fn-1", "v1", resolved)
}

func post(t *testing.T, srv *Server, path, body, traceparent string) (int, http.Header, string) {
	t.Helper()
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	req, err := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if traceparent != "" {
		req.Header.Set("traceparent", traceparent)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(raw)
}

func TestRouteWarmAffinityAndHeader(t *testing.T) {
	reg, _, srv := testRig(t)
	cold := newFake(t, "cold", 200, `{"ok":true}`)
	warmW := newFake(t, "warm-worker", 200, `{"executionId":"e1","success":true,"coldStart":false}`)
	cold.beat(t, reg, 0)
	warmW.beat(t, reg, 4, affinityOfTest())

	before := sample("nimbus_scheduler_routes_total", map[string]string{"worker": "warm-worker", "reason": "warm"})
	code, hdr, body := post(t, srv, "/executions", execBody(), "")
	if code != 200 || !strings.Contains(body, `"coldStart":false`) {
		t.Fatalf("code %d body %s", code, body)
	}
	if hdr.Get("X-Nimbus-Worker") != "warm-worker" {
		t.Fatalf("header %q", hdr.Get("X-Nimbus-Worker"))
	}
	if cold.calls() != 0 || warmW.calls() != 1 {
		t.Fatalf("cold %d warm %d", cold.calls(), warmW.calls())
	}
	after := sample("nimbus_scheduler_routes_total", map[string]string{"worker": "warm-worker", "reason": "warm"})
	if after-before != 1 {
		t.Fatalf("warm routes delta %v", after-before)
	}
}

func TestRouteLeastLoaded(t *testing.T) {
	reg, _, srv := testRig(t)
	busy := newFake(t, "busy", 200, `{"from":"busy"}`)
	idle := newFake(t, "idle", 200, `{"from":"idle"}`)
	// 6/8 is busier than 1/8.
	busy.beat(t, reg, 6)
	idle.beat(t, reg, 1)
	code, hdr, body := post(t, srv, "/executions", execBody(), "")
	if code != 200 || !strings.Contains(body, "idle") {
		t.Fatalf("code %d body %s", code, body)
	}
	if hdr.Get("X-Nimbus-Worker") != "idle" || busy.calls() != 0 {
		t.Fatalf("header %s busy calls %d", hdr.Get("X-Nimbus-Worker"), busy.calls())
	}
}

func TestRouteFailoverOnce(t *testing.T) {
	reg, _, srv := testRig(t)
	deadSrv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := deadSrv.URL
	deadSrv.Close()
	second := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	secondURL := second.URL
	second.Close()
	live := newFake(t, "survivor", 200, `{"from":"survivor"}`)

	ctx := context.Background()
	for _, snap := range []registry.Snapshot{
		{WorkerID: "dead-a", URL: deadURL, ActiveExecutions: 0, MaxContainers: 8, State: registry.StateHealthy},
		{WorkerID: "dead-b", URL: secondURL, ActiveExecutions: 1, MaxContainers: 8, State: registry.StateHealthy},
		{WorkerID: "survivor", URL: live.srv.URL, ActiveExecutions: 2, MaxContainers: 8, State: registry.StateHealthy},
	} {
		if err := reg.Heartbeat(ctx, snap); err != nil {
			t.Fatal(err)
		}
	}
	before := sample("nimbus_scheduler_routes_total", map[string]string{"worker": "survivor", "reason": "failover"})
	code, _, body := post(t, srv, "/executions", execBody(), "")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("retry stops after one failover, code %d body %s calls %d", code, body, live.calls())
	}
	if live.calls() != 0 {
		t.Fatalf("third worker was contacted %d", live.calls())
	}
	if sample("nimbus_scheduler_routes_total", map[string]string{"worker": "survivor", "reason": "failover"}) != before {
		t.Fatal("failover metric should not count a worker that was not dialed successfully")
	}

	// One dead peer and one live peer: the single retry lands on the survivor.
	if err := reg.Remove(ctx, "dead-b"); err != nil {
		t.Fatal(err)
	}
	code, hdr, body := post(t, srv, "/executions", execBody(), "")
	if code != 200 || !strings.Contains(body, "survivor") || hdr.Get("X-Nimbus-Worker") != "survivor" {
		t.Fatalf("code %d header %s body %s", code, hdr.Get("X-Nimbus-Worker"), body)
	}
	delta := sample("nimbus_scheduler_routes_total", map[string]string{"worker": "survivor", "reason": "failover"}) - before
	if delta != 1 {
		t.Fatalf("failover delta %v", delta)
	}
}

func TestCancelRoutesToRecordedWorker(t *testing.T) {
	reg, rdb, srv := testRig(t)
	holder := newFake(t, "holder", 200, `{"executionId":"e1","cancelled":true,"message":"ok"}`)
	other := newFake(t, "other", 200, `{"executionId":"e1","cancelled":true,"message":"other"}`)
	holder.beat(t, reg, 1)
	other.beat(t, reg, 0)
	if err := reg.Heartbeat(context.Background(), registry.Snapshot{
		WorkerID: "holder", URL: holder.srv.URL, ActiveExecutions: 1, MaxContainers: 8, State: registry.StateHealthy,
	}); err != nil {
		t.Fatal(err)
	}
	err := rdb.HSet(context.Background(), "nimbus:exec:e1", "workerId", "holder", "status", "running", "functionId", "fn").Err()
	if err != nil {
		t.Fatal(err)
	}
	code, _, body := post(t, srv, "/executions/e1/cancel", "", "")
	if code != 200 || !strings.Contains(body, `"cancelled":true`) {
		t.Fatalf("code %d body %s", code, body)
	}
	if holder.calls() != 1 || other.calls() != 0 {
		t.Fatalf("holder %d other %d", holder.calls(), other.calls())
	}
	if holder.path(0) != "/executions/e1/cancel" {
		t.Fatalf("path %s", holder.path(0))
	}
}

func TestCancelBroadcastWhenWorkerUnknown(t *testing.T) {
	reg, _, srv := testRig(t)
	miss := newFake(t, "miss", 404, `{"executionId":"e1","cancelled":false}`)
	hit := newFake(t, "hit", 200, `{"executionId":"e1","cancelled":true}`)
	miss.beat(t, reg, 0)
	hit.beat(t, reg, 0)
	code, _, body := post(t, srv, "/executions/e1/cancel", "", "")
	if code != 200 || !strings.Contains(body, `"cancelled":true`) {
		t.Fatalf("code %d body %s", code, body)
	}
	if miss.calls() != 1 || hit.calls() != 1 {
		t.Fatalf("miss %d hit %d", miss.calls(), hit.calls())
	}
}

func TestForwardAsyncToAnyHealthy(t *testing.T) {
	reg, _, srv := testRig(t)
	a := newFake(t, "a", 202, `{"status":"queued"}`)
	b := newFake(t, "b", 202, `{"status":"queued"}`)
	a.beat(t, reg, 3)
	b.beat(t, reg, 0)
	code, hdr, body := post(t, srv, "/async/jobs", `{"invocationId":"i1","executionId":"e1","functionId":"fn","version":"v1"}`, "")
	if code != 202 || !strings.Contains(body, "queued") {
		t.Fatalf("code %d body %s", code, body)
	}
	if hdr.Get("X-Nimbus-Worker") != "b" || a.calls() != 0 {
		t.Fatalf("header %s a %d", hdr.Get("X-Nimbus-Worker"), a.calls())
	}
}

func TestWorkersAndHealth(t *testing.T) {
	reg, _, srv := testRig(t)
	if err := reg.Heartbeat(context.Background(), registry.Snapshot{
		WorkerID: "a", URL: "http://a:8080", MaxContainers: 8, State: registry.StateHealthy,
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Heartbeat(context.Background(), registry.Snapshot{
		WorkerID: "b", URL: "http://b:8080", MaxContainers: 8, State: registry.StateDraining,
	}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	resp, err := http.Get(ts.URL + "/workers")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var listed struct {
		Workers []registry.Snapshot `json:"workers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Workers) != 2 {
		t.Fatalf("workers %+v", listed.Workers)
	}
	health, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer health.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(health.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["service"] != "scheduler" || body["workersHealthy"] != float64(1) {
		t.Fatalf("health %+v", body)
	}
	if sample("nimbus_scheduler_workers_healthy", nil) != 1 {
		t.Fatalf("gauge %v", sample("nimbus_scheduler_workers_healthy", nil))
	}
}

func TestNoWorkers503(t *testing.T) {
	_, _, srv := testRig(t)
	code, _, body := post(t, srv, "/executions", execBody(), "")
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "No hay workers") {
		t.Fatalf("code %d body %s", code, body)
	}
}

func TestTraceparentPropagates(t *testing.T) {
	tp := sdktrace.NewTracerProvider()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	ctx, span := tp.Tracer("test").Start(context.Background(), "nest")
	parent := ntrace.Header(ctx)
	span.End()
	if parent == "" {
		t.Fatal("empty traceparent")
	}

	reg, _, srv := testRig(t)
	var got string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("traceparent")
		w.Header().Set("X-Nimbus-Worker", "traced")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(backend.Close)
	if err := reg.Heartbeat(context.Background(), registry.Snapshot{
		WorkerID: "traced", URL: backend.URL, MaxContainers: 8, State: registry.StateHealthy,
	}); err != nil {
		t.Fatal(err)
	}
	code, _, _ := post(t, srv, "/executions", execBody(), parent)
	if code != 200 {
		t.Fatalf("code %d", code)
	}
	if got == "" || !strings.Contains(got, ntrace.ID(ctx)) {
		t.Fatalf("forwarded traceparent %q want trace %s", got, ntrace.ID(ctx))
	}
	if got == parent {
		t.Fatal("scheduler should start its own span instead of copying the parent header unchanged")
	}
}
