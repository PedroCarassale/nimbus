package async

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	ntrace "github.com/PedroCarassale/nimbus/compute-plane/internal/trace"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type clockQueue struct {
	q       *Queue
	mr      *miniredis.Miniredis
	advance func(time.Duration)
	now     func() time.Time
}

func newClockQueue(t *testing.T, cfg Config) clockQueue {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	if cfg.MaxAttempts == 0 {
		cfg.MaxAttempts = 3
	}
	if cfg.LockTTL == 0 {
		cfg.LockTTL = time.Second
	}
	if cfg.ClaimIdle == 0 {
		cfg.ClaimIdle = 50 * time.Millisecond
	}
	q := New(rdb, cfg)
	q.rnd = func() float64 { return 0 }
	current := time.Unix(1_700_000_000, 0)
	q.now = func() time.Time { return current }
	if err := q.EnsureGroup(context.Background()); err != nil {
		t.Fatal(err)
	}
	return clockQueue{
		q:       q,
		mr:      mr,
		advance: func(d time.Duration) { current = current.Add(d) },
		now:     func() time.Time { return current },
	}
}

func jobBody(t *testing.T, executionID string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"executionId": executionID,
		"functionId":  "fn-1",
		"version":     "v1",
		"event":       map[string]any{"name": "Nimbus"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestEnqueueCarriesTraceParent(t *testing.T) {
	tp := sdktrace.NewTracerProvider()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	ctx, span := tp.Tracer("test").Start(context.Background(), "nimbus.async.enqueue")
	defer span.End()

	cq := newClockQueue(t, Config{})
	if _, _, err := cq.q.Enqueue(ctx, "inv-trace", "exec-trace", jobBody(t, "exec-trace")); err != nil {
		t.Fatal(err)
	}
	got, err := cq.q.Read(context.Background(), 4, -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("deliveries=%d", len(got))
	}
	if !strings.Contains(got[0].Job.TraceParent, span.SpanContext().TraceID().String()) {
		t.Fatalf("traceparent %q", got[0].Job.TraceParent)
	}
	continued := ntrace.Continue(context.Background(), got[0].Job.TraceParent, got[0].Job.TraceState)
	if ntrace.ID(continued) != span.SpanContext().TraceID().String() {
		t.Fatalf("worker context %s", ntrace.ID(continued))
	}
}

func TestEnqueueReadAndIdempotent(t *testing.T) {
	cq := newClockQueue(t, Config{})
	ctx := context.Background()
	body := jobBody(t, "exec-1")

	id, dup, err := cq.q.Enqueue(ctx, "inv-1", "exec-1", body)
	if err != nil || dup || id != "exec-1" {
		t.Fatalf("enqueue id=%s dup=%v err=%v", id, dup, err)
	}
	id, dup, err = cq.q.Enqueue(ctx, "inv-1", "exec-other", body)
	if err != nil || !dup || id != "exec-1" {
		t.Fatalf("second enqueue id=%s dup=%v err=%v", id, dup, err)
	}
	pendingStatus, err := cq.q.rdb.HGet(ctx, "nimbus:exec:exec-1", "status").Result()
	if err != nil || pendingStatus != "pending" {
		t.Fatalf("exec hash status=%q err=%v", pendingStatus, err)
	}
	if fn, err := cq.q.rdb.HGet(ctx, "nimbus:exec:exec-1", "functionId").Result(); err != nil || fn != "fn-1" {
		t.Fatalf("functionId=%q err=%v", fn, err)
	}
	if n, err := cq.q.rdb.Exists(ctx, "nimbus:exec:exec-other").Result(); err != nil || n != 0 {
		t.Fatalf("duplicate wrote another exec hash n=%d err=%v", n, err)
	}

	got, err := cq.q.Read(ctx, 10, -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("deliveries=%d", len(got))
	}
	if got[0].Job.InvocationID != "inv-1" || got[0].Job.Attempt != 1 || got[0].Job.MaxAttempts != 3 {
		t.Fatalf("job %+v", got[0].Job)
	}
	again, err := cq.q.Read(ctx, 10, -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("duplicate read: %+v", again)
	}
	pending, err := cq.q.rdb.XPending(ctx, streamJobs, groupWorkers).Result()
	if err != nil {
		t.Fatal(err)
	}
	if pending.Count != 1 {
		t.Fatalf("pending=%d", pending.Count)
	}
}

func TestRetryDelayThenPromote(t *testing.T) {
	base := 100 * time.Millisecond
	cq := newClockQueue(t, Config{BackoffBase: base, BackoffMax: time.Second})
	ctx := context.Background()
	job := Job{
		InvocationID: "inv-1",
		Attempt:      2,
		MaxAttempts:  3,
		EnqueuedAt:   cq.now().UnixMilli(),
		Body:         jobBody(t, "exec-1"),
	}
	st := State{Phase: PhaseRetrying, Attempt: 2, EnqueuedAt: job.EnqueuedAt}
	if err := cq.q.ScheduleRetry(ctx, job, cq.q.RetryDelay(1), st); err != nil {
		t.Fatal(err)
	}
	n, err := cq.q.PromoteDue(ctx, 10)
	if err != nil || n != 0 {
		t.Fatalf("early promote n=%d err=%v", n, err)
	}
	cq.advance(base)
	n, err = cq.q.PromoteDue(ctx, 10)
	if err != nil || n != 1 {
		t.Fatalf("due promote n=%d err=%v", n, err)
	}
	got, err := cq.q.Read(ctx, 10, -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Job.Attempt != 2 {
		t.Fatalf("promoted %+v", got)
	}
	loaded, err := cq.q.LoadState(ctx, "inv-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Phase != PhaseRetrying || loaded.Attempt != 2 {
		t.Fatalf("state %+v", loaded)
	}
}

func TestLockSingleFlightAndRefresh(t *testing.T) {
	cq := newClockQueue(t, Config{LockTTL: 200 * time.Millisecond})
	ctx := context.Background()
	ok, err := cq.q.TryLock(ctx, "inv-1", "token-a")
	if err != nil || !ok {
		t.Fatalf("first lock ok=%v err=%v", ok, err)
	}
	ok, err = cq.q.TryLock(ctx, "inv-1", "token-b")
	if err != nil || ok {
		t.Fatalf("second lock ok=%v err=%v", ok, err)
	}
	cq.mr.FastForward(100 * time.Millisecond)
	if err := cq.q.RefreshLock(ctx, "inv-1", "token-a"); err != nil {
		t.Fatal(err)
	}
	cq.mr.FastForward(150 * time.Millisecond)
	ok, err = cq.q.TryLock(ctx, "inv-1", "token-b")
	if err != nil || ok {
		t.Fatalf("lock should still be held ok=%v err=%v", ok, err)
	}
	cq.mr.FastForward(200 * time.Millisecond)
	ok, err = cq.q.TryLock(ctx, "inv-1", "token-b")
	if err != nil || !ok {
		t.Fatalf("lock should expire ok=%v err=%v", ok, err)
	}
	if err := cq.q.Unlock(ctx, "inv-1", "token-b"); err != nil {
		t.Fatal(err)
	}
	ok, err = cq.q.TryLock(ctx, "inv-1", "token-c")
	if err != nil || !ok {
		t.Fatalf("after unlock ok=%v err=%v", ok, err)
	}
}

func TestAutoClaimReclaimsIdlePending(t *testing.T) {
	cq := newClockQueue(t, Config{ClaimIdle: 50 * time.Millisecond})
	ctx := context.Background()
	if _, _, err := cq.q.Enqueue(ctx, "inv-1", "exec-1", jobBody(t, "exec-1")); err != nil {
		t.Fatal(err)
	}
	// XAUTOCLAIM idle follows miniredis' clock, not FastForward (that only
	// shortens key TTLs). Set it before the message is delivered.
	start := time.Unix(1_700_000_000, 0)
	cq.mr.SetTime(start)
	owner := cq.q.WithConsumer("owner")
	other := cq.q.WithConsumer("other")
	got, err := owner.Read(ctx, 5, -1)
	if err != nil || len(got) != 1 {
		t.Fatalf("read %+v err=%v", got, err)
	}
	early, err := other.Claim(ctx, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(early) != 0 {
		t.Fatalf("claimed too soon: %+v", early)
	}
	cq.mr.SetTime(start.Add(60 * time.Millisecond))
	claimed, err := other.Claim(ctx, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0].Job.InvocationID != "inv-1" {
		t.Fatalf("claimed %+v", claimed)
	}
	if err := other.Ack(ctx, claimed[0]); err != nil {
		t.Fatal(err)
	}
	pending, err := cq.q.rdb.XPending(ctx, streamJobs, groupWorkers).Result()
	if err != nil {
		t.Fatal(err)
	}
	if pending.Count != 0 {
		t.Fatalf("pending=%d", pending.Count)
	}
}

func TestDLQListAndRedrive(t *testing.T) {
	cq := newClockQueue(t, Config{})
	ctx := context.Background()
	body := jobBody(t, "exec-1")
	job := Job{InvocationID: "inv-1", Attempt: 3, MaxAttempts: 3, EnqueuedAt: 10, Body: body}
	rawJob, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	entry := DLQEntry{
		ID: "inv-1", InvocationID: "inv-1", ExecutionID: "exec-1",
		FunctionID: "fn-1", Version: "v1", Event: json.RawMessage(`{"name":"Nimbus"}`),
		Payload: rawJob, LastError: "boom", ExitCode: 1, Attempts: 3, MaxAttempts: 3,
		EnqueuedAt: 10, AttemptTimes: []int64{10, 20, 30}, FailedAt: 30, DeadAt: 40,
	}
	st := State{Phase: PhaseDead, Attempt: 3, LastError: "boom", Result: &Result{Status: "ERROR", Attempts: 3}}
	if err := cq.q.CommitTerminal(ctx, "inv-1", st, &entry); err != nil {
		t.Fatal(err)
	}
	list, err := cq.q.ListDLQ(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].LastError != "boom" || list[0].Attempts != 3 {
		t.Fatalf("list %+v", list)
	}
	if string(list[0].Event) != `{"name":"Nimbus"}` || len(list[0].AttemptTimes) != 3 {
		t.Fatalf("payload %+v", list[0])
	}

	if _, err := cq.q.Redrive(ctx, "missing"); err != ErrNotFound {
		t.Fatalf("missing redrive: %v", err)
	}
	got, err := cq.q.Redrive(ctx, "inv-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ExecutionID != "exec-1" {
		t.Fatalf("redrive %+v", got)
	}
	list, err = cq.q.ListDLQ(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("dlq after redrive: %+v", list)
	}
	msgs, err := cq.q.Read(ctx, 5, -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Job.Attempt != 1 {
		t.Fatalf("requeued %+v", msgs)
	}
	loaded, err := cq.q.LoadState(ctx, "inv-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Phase != PhaseQueued {
		t.Fatalf("state %+v", loaded)
	}
	results, err := cq.q.rdb.XRange(ctx, streamResults, "-", "+").Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("results %+v", results)
	}
}
