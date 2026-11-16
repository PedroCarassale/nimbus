package async

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWorkerRetriesThenDLQ(t *testing.T) {
	base := 100 * time.Millisecond
	cq := newClockQueue(t, Config{
		MaxAttempts:   3,
		BackoffBase:   base,
		BackoffMax:    time.Second,
		BackoffJitter: 0,
	})
	var calls atomic.Int32
	worker := NewWorker(cq.q, func(context.Context, json.RawMessage) RunResult {
		calls.Add(1)
		return RunResult{Kind: KindRetry, Error: "boom", ExitCode: 1, DurationMs: 42}
	})
	ctx := context.Background()
	if _, _, err := cq.q.Enqueue(ctx, "inv-1", "exec-1", jobBody(t, "exec-1")); err != nil {
		t.Fatal(err)
	}

	var starts []int64
	for attempt := 1; attempt <= 3; attempt++ {
		msgs, err := cq.q.Read(ctx, 5, -1)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("attempt %d deliveries=%d", attempt, len(msgs))
		}
		worker.Handle(ctx, msgs[0])
		st, err := cq.q.LoadState(ctx, "inv-1")
		if err != nil {
			t.Fatal(err)
		}
		starts = append([]int64{}, st.AttemptTimes...)
		if attempt < 3 {
			if st.Phase != PhaseRetrying || st.Attempt != attempt+1 {
				t.Fatalf("after %d: %+v", attempt, st)
			}
			if n, err := cq.q.PromoteDue(ctx, 10); err != nil || n != 0 {
				t.Fatalf("not due yet n=%d err=%v", n, err)
			}
			cq.advance(Backoff(attempt, base, 0, time.Second, 0))
			if n, err := cq.q.PromoteDue(ctx, 10); err != nil || n != 1 {
				t.Fatalf("promote attempt %d n=%d err=%v", attempt, n, err)
			}
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("calls=%d", calls.Load())
	}
	list, err := cq.q.ListDLQ(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("dlq %+v", list)
	}
	entry := list[0]
	if entry.Attempts != 3 || entry.LastError != "boom" || entry.ExitCode != 1 {
		t.Fatalf("entry %+v", entry)
	}
	if entry.ExecutionID != "exec-1" || entry.FunctionID != "fn-1" || len(entry.Payload) == 0 {
		t.Fatalf("payload %+v", entry)
	}
	if len(entry.AttemptTimes) != 3 {
		t.Fatalf("times %+v", entry.AttemptTimes)
	}
	if entry.AttemptTimes[1]-entry.AttemptTimes[0] != base.Milliseconds() {
		t.Fatalf("first gap %d, starts %v", entry.AttemptTimes[1]-entry.AttemptTimes[0], entry.AttemptTimes)
	}
	if entry.AttemptTimes[2]-entry.AttemptTimes[1] != (2 * base).Milliseconds() {
		t.Fatalf("second gap %d, starts %v", entry.AttemptTimes[2]-entry.AttemptTimes[1], entry.AttemptTimes)
	}
	if len(starts) != 3 {
		t.Fatalf("starts %v", starts)
	}
	st, err := cq.q.LoadState(ctx, "inv-1")
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != PhaseDead || st.Result == nil || st.Result.Status != "ERROR" || st.Result.Attempts != 3 {
		t.Fatalf("state %+v", st)
	}
	if st.Result.DurationMs != 42 {
		t.Fatalf("DLQ result dropped durationMs: %+v", st.Result)
	}
}

func TestWorkerValidationDoesNotRetryOrDLQ(t *testing.T) {
	cq := newClockQueue(t, Config{MaxAttempts: 3, BackoffBase: time.Millisecond})
	var calls atomic.Int32
	worker := NewWorker(cq.q, func(context.Context, json.RawMessage) RunResult {
		calls.Add(1)
		return RunResult{Kind: KindValidation, Error: "timeoutSec debe ser un entero entre 1 y 300", ExitCode: 400}
	})
	ctx := context.Background()
	if _, _, err := cq.q.Enqueue(ctx, "inv-1", "exec-1", jobBody(t, "exec-1")); err != nil {
		t.Fatal(err)
	}
	msgs, err := cq.q.Read(ctx, 5, -1)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("read %+v err=%v", msgs, err)
	}
	worker.Handle(ctx, msgs[0])
	worker.Handle(ctx, msgs[0])
	if calls.Load() != 1 {
		t.Fatalf("calls=%d", calls.Load())
	}
	list, err := cq.q.ListDLQ(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("validation landed in DLQ: %+v", list)
	}
	st, err := cq.q.LoadState(ctx, "inv-1")
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != PhaseFailed || st.Result == nil || st.Result.Status != "ERROR" {
		t.Fatalf("state %+v", st)
	}
	if n, err := cq.q.PromoteDue(ctx, 10); err != nil || n != 0 {
		t.Fatalf("promote n=%d err=%v", n, err)
	}
}

func TestWorkerSuccessAndRedrive(t *testing.T) {
	cq := newClockQueue(t, Config{MaxAttempts: 2, BackoffBase: time.Millisecond, BackoffMax: time.Second})
	var calls atomic.Int32
	worker := NewWorker(cq.q, func(context.Context, json.RawMessage) RunResult {
		n := calls.Add(1)
		if n == 1 {
			return RunResult{Kind: KindRetry, Error: "once", ExitCode: 1}
		}
		return RunResult{Kind: KindOK, Output: map[string]any{"ok": true}, ExitCode: 0, DurationMs: 12}
	})
	ctx := context.Background()
	if _, _, err := cq.q.Enqueue(ctx, "inv-1", "exec-1", jobBody(t, "exec-1")); err != nil {
		t.Fatal(err)
	}
	msgs, err := cq.q.Read(ctx, 5, -1)
	if err != nil || len(msgs) != 1 {
		t.Fatal(err)
	}
	worker.Handle(ctx, msgs[0])
	cq.advance(time.Millisecond)
	if _, err := cq.q.PromoteDue(ctx, 10); err != nil {
		t.Fatal(err)
	}
	msgs, err = cq.q.Read(ctx, 5, -1)
	if err != nil || len(msgs) != 1 || msgs[0].Job.Attempt != 2 {
		t.Fatalf("retry %+v err=%v", msgs, err)
	}
	worker.Handle(ctx, msgs[0])
	st, err := cq.q.LoadState(ctx, "inv-1")
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != PhaseSucceeded || st.Result == nil || st.Result.Status != "SUCCESS" || st.Result.Attempts != 2 {
		t.Fatalf("state %+v", st)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls=%d", calls.Load())
	}
}

func TestWorkerCrashCountsAttemptAndClaimDoesNotDoubleRun(t *testing.T) {
	cq := newClockQueue(t, Config{MaxAttempts: 3, LockTTL: time.Second})
	ctx := context.Background()
	if _, _, err := cq.q.Enqueue(ctx, "inv-1", "exec-1", jobBody(t, "exec-1")); err != nil {
		t.Fatal(err)
	}
	msgs, err := cq.q.Read(ctx, 5, -1)
	if err != nil || len(msgs) != 1 {
		t.Fatal(err)
	}
	if err := cq.q.SaveState(ctx, "inv-1", State{
		Phase: PhaseRunning, Attempt: 1, EnqueuedAt: msgs[0].Job.EnqueuedAt,
		AttemptTimes: []int64{cq.now().UnixMilli()},
	}); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	var once sync.Once
	worker := NewWorker(cq.q, func(context.Context, json.RawMessage) RunResult {
		calls.Add(1)
		once.Do(func() { close(started) })
		<-release
		return RunResult{Kind: KindOK, Output: map[string]any{"ok": true}}
	})

	done := make(chan struct{})
	go func() {
		worker.Handle(ctx, msgs[0])
		close(done)
	}()
	<-started

	other := NewWorker(cq.q.WithConsumer("other"), func(context.Context, json.RawMessage) RunResult {
		calls.Add(1)
		return RunResult{Kind: KindOK}
	})
	other.Handle(ctx, msgs[0])
	if calls.Load() != 1 {
		t.Fatalf("concurrent calls=%d", calls.Load())
	}
	close(release)
	<-done

	st, err := cq.q.LoadState(ctx, "inv-1")
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != PhaseSucceeded || st.Attempt != 2 {
		t.Fatalf("reclaimed attempt state %+v", st)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls=%d", calls.Load())
	}
}

func TestWorkerTimeoutReachesDLQ(t *testing.T) {
	cq := newClockQueue(t, Config{MaxAttempts: 1})
	worker := NewWorker(cq.q, func(context.Context, json.RawMessage) RunResult {
		return RunResult{Kind: KindRetry, Error: "Timeout excedido", ExitCode: 124}
	})
	ctx := context.Background()
	if _, _, err := cq.q.Enqueue(ctx, "inv-1", "exec-1", jobBody(t, "exec-1")); err != nil {
		t.Fatal(err)
	}
	msgs, err := cq.q.Read(ctx, 1, -1)
	if err != nil || len(msgs) != 1 {
		t.Fatal(err)
	}
	worker.Handle(ctx, msgs[0])
	list, err := cq.q.ListDLQ(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list %+v err=%v", list, err)
	}
	if list[0].ExitCode != 124 {
		t.Fatalf("entry %+v", list[0])
	}
	st, err := cq.q.LoadState(ctx, "inv-1")
	if err != nil {
		t.Fatal(err)
	}
	if st.Result == nil || st.Result.Status != "TIMEOUT" {
		t.Fatalf("state %+v", st)
	}
}
