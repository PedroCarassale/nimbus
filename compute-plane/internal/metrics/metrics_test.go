package metrics

import (
	"context"
	"testing"
)

func TestRecordInvocationAndRejection(t *testing.T) {
	const fn = "fn-metrics-unit"
	beforeInv := Sample("nimbus_invocations_total", map[string]string{
		"function_id": fn, "status": "success", "mode": "sync",
	})
	beforeErr := Sample("nimbus_errors_total", map[string]string{
		"function_id": fn, "mode": "async",
	})
	beforeTimeout := Sample("nimbus_timeouts_total", map[string]string{
		"function_id": fn, "mode": "sync",
	})
	beforeOOM := Sample("nimbus_ooms_total", map[string]string{
		"function_id": fn, "mode": "sync",
	})
	beforeCold := Sample("nimbus_cold_starts_total", map[string]string{"function_id": fn})
	beforeWarm := Sample("nimbus_warm_starts_total", map[string]string{"function_id": fn})
	beforeRej := Sample("nimbus_rejections_total", map[string]string{
		"function_id": fn, "reason": "queue_full",
	})
	beforeRetry := Sample("nimbus_async_retries_total", map[string]string{"function_id": fn})

	if got := Classify(true, 0, ""); got != "success" {
		t.Fatalf("success: %s", got)
	}
	if got := Classify(false, 124, "Timeout excedido"); got != "timeout" {
		t.Fatalf("timeout: %s", got)
	}
	if got := Classify(false, 137, "killed"); got != "oom" {
		t.Fatalf("oom: %s", got)
	}
	if got := Classify(false, 1, "La función superó el límite de memoria (128 MB) y fue terminada"); got != "oom" {
		t.Fatalf("oom text: %s", got)
	}
	if got := Classify(false, 1, "boom"); got != "error" {
		t.Fatalf("error: %s", got)
	}

	RecordInvocation(fn, "sync", "success", 40, true, 12)
	RecordInvocation(fn, "async", "error", 15, false, 0)
	RecordInvocation(fn, "sync", Classify(false, 124, ""), 1000, true, 0)
	RecordInvocation(fn, "sync", Classify(false, 137, ""), 20, true, 8)
	ObserveQueueWait(fn, 250)
	Rejection(fn, "queue_full")
	Rejection(fn, "nope")
	AsyncRetry(fn)
	ActiveInc(fn)
	ActiveDec(fn)

	if got := Sample("nimbus_invocations_total", map[string]string{
		"function_id": fn, "status": "success", "mode": "sync",
	}); got-beforeInv != 1 {
		t.Fatalf("invocations %v", got-beforeInv)
	}
	if got := Sample("nimbus_errors_total", map[string]string{
		"function_id": fn, "mode": "async",
	}); got-beforeErr != 1 {
		t.Fatalf("errors %v", got-beforeErr)
	}
	if got := Sample("nimbus_timeouts_total", map[string]string{
		"function_id": fn, "mode": "sync",
	}); got-beforeTimeout != 1 {
		t.Fatalf("timeouts %v", got-beforeTimeout)
	}
	if got := Sample("nimbus_ooms_total", map[string]string{
		"function_id": fn, "mode": "sync",
	}); got-beforeOOM != 1 {
		t.Fatalf("ooms %v", got-beforeOOM)
	}
	if got := Sample("nimbus_cold_starts_total", map[string]string{"function_id": fn}); got-beforeCold != 3 {
		t.Fatalf("cold %v", got-beforeCold)
	}
	if got := Sample("nimbus_warm_starts_total", map[string]string{"function_id": fn}); got-beforeWarm != 1 {
		t.Fatalf("warm %v", got-beforeWarm)
	}
	if got := Sample("nimbus_rejections_total", map[string]string{
		"function_id": fn, "reason": "queue_full",
	}); got-beforeRej != 2 {
		t.Fatalf("rejections %v (unknown reasons collapse to queue_full)", got-beforeRej)
	}
	if got := Sample("nimbus_async_retries_total", map[string]string{"function_id": fn}); got-beforeRetry != 1 {
		t.Fatalf("retries %v", got-beforeRetry)
	}
	if got := Sample("nimbus_invocation_duration_seconds", map[string]string{
		"function_id": fn, "mode": "sync",
	}); got < 3 {
		t.Fatalf("duration samples %v", got)
	}
	if got := Sample("nimbus_active_executions", map[string]string{"function_id": fn}); got != 0 {
		t.Fatalf("active %v", got)
	}
	if got := Sample("nimbus_init_duration_seconds", map[string]string{"function_id": fn}); got != 2 {
		t.Fatalf("init samples %v, want 2 (zero initMs is skipped)", got)
	}
}

func TestCollectorEmitsLiveGauges(t *testing.T) {
	Remember("fn-live")
	SetSources(Sources{
		QueueDepth: func(context.Context) map[string]float64 {
			return map[string]float64{"fn-live": 4, "fn-other": 1}
		},
		DLQEntries:      func(context.Context) float64 { return 2 },
		ContainersInUse: func(context.Context) float64 { return 3 },
		ContainersLimit: 8,
		WarmPool: func() []WarmSample {
			return []WarmSample{{FunctionID: "fn-live", State: "idle", Count: 1}}
		},
	})
	t.Cleanup(func() { SetSources(Sources{}) })

	if got := Sample("nimbus_queue_depth", map[string]string{"function_id": "fn-live"}); got != 4 {
		t.Fatalf("queue %v", got)
	}
	if got := Sample("nimbus_queue_depth", map[string]string{"function_id": "fn-other"}); got != 1 {
		t.Fatalf("other queue %v", got)
	}
	if got := Sample("nimbus_dlq_entries", nil); got != 2 {
		t.Fatalf("dlq %v", got)
	}
	if got := Sample("nimbus_containers_in_use", nil); got != 3 {
		t.Fatalf("containers %v", got)
	}
	if got := Sample("nimbus_containers_limit", nil); got != 8 {
		t.Fatalf("limit %v", got)
	}
	if got := Sample("nimbus_warm_pool_size", map[string]string{
		"function_id": "fn-live", "state": "idle",
	}); got != 1 {
		t.Fatalf("warm %v", got)
	}
}

func TestLabelStripsUnsafeCharacters(t *testing.T) {
	if got := label("fn/1 a"); got != "fn_1_a" {
		t.Fatalf("label %q", got)
	}
	if got := label(""); got != "unknown" {
		t.Fatalf("empty %q", got)
	}
}
