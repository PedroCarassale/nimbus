// Package metrics exposes Prometheus metrics for the compute plane.
//
// Labels stay low-cardinality: function_id is allowed, execution_id is not.
// Gauges that describe Redis or the warm pool are filled by the collector in
// collector.go on each scrape, so a restart does not have to remember them.
package metrics

import (
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	dto "github.com/prometheus/client_model/go"
)

var (
	invocations = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "nimbus_invocations_total",
		Help: "Invocations that ran, by function, status (success, error, timeout, oom, cancelled) and mode (sync, async).",
	}, []string{"function_id", "status", "mode"})

	errorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "nimbus_errors_total",
		Help: "Invocations whose handler failed (not a timeout and not an OOM).",
	}, []string{"function_id", "mode"})

	timeoutsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "nimbus_timeouts_total",
		Help: "Invocations stopped because they exceeded timeoutSec (exit 124).",
	}, []string{"function_id", "mode"})

	oomsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "nimbus_ooms_total",
		Help: "Invocations killed for exceeding the memory limit.",
	}, []string{"function_id", "mode"})

	coldStarts = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "nimbus_cold_starts_total",
		Help: "Invocations that started a new container.",
	}, []string{"function_id"})

	warmStarts = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "nimbus_warm_starts_total",
		Help: "Invocations that reused a warm container.",
	}, []string{"function_id"})

	rejections = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "nimbus_rejections_total",
		Help: "Invocations rejected with HTTP 429 before they ran.",
	}, []string{"function_id", "reason"})

	asyncRetries = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "nimbus_async_retries_total",
		Help: "Async attempts scheduled after a retriable failure. The first attempt is not a retry.",
	}, []string{"function_id"})

	duration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "nimbus_invocation_duration_seconds",
		Help:    "Time spent after a concurrency slot was granted, including artifact download and the handler.",
		Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300},
	}, []string{"function_id", "mode"})

	initDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "nimbus_init_duration_seconds",
		Help:    "Container init time on a cold start. Warm hits and the one-shot cold path (initMs 0) are omitted.",
		Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10},
	}, []string{"function_id"})

	queueWait = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "nimbus_queue_wait_seconds",
		Help:    "Time an invocation waited in the per-function FIFO before it was admitted or rejected.",
		Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60},
	}, []string{"function_id"})

	active = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "nimbus_active_executions",
		Help: "Executions that currently hold a concurrency slot in this process.",
	}, []string{"function_id"})
)

var remembered sync.Map

// Remember keeps a function id so a later scrape can report queue depth 0
// after the Redis key is gone.
func Remember(functionID string) {
	id := label(functionID)
	remembered.Store(id, struct{}{})
}

func rememberedIDs() []string {
	var ids []string
	remembered.Range(func(key, _ any) bool {
		if id, ok := key.(string); ok {
			ids = append(ids, id)
		}
		return true
	})
	return ids
}

// Classify maps an execution result to a status label.
func Classify(success bool, exitCode int, errText string) string {
	if exitCode == 124 {
		return "timeout"
	}
	if exitCode == 137 || exitCode == 134 || strings.Contains(errText, "límite de memoria") {
		return "oom"
	}
	if success {
		return "success"
	}
	return "error"
}

// RecordInvocation updates counters and histograms for one execution that
// got past admission. mode is "sync" or "async".
func RecordInvocation(functionID, mode, status string, durationMs int64, coldStart bool, initMs int64) {
	id := label(functionID)
	mode = modeLabel(mode)
	status = statusLabel(status)
	Remember(id)
	invocations.WithLabelValues(id, status, mode).Inc()
	switch status {
	case "error":
		errorsTotal.WithLabelValues(id, mode).Inc()
	case "timeout":
		timeoutsTotal.WithLabelValues(id, mode).Inc()
	case "oom":
		oomsTotal.WithLabelValues(id, mode).Inc()
	}
	if durationMs < 0 {
		durationMs = 0
	}
	duration.WithLabelValues(id, mode).Observe(float64(durationMs) / 1000)
	if coldStart {
		coldStarts.WithLabelValues(id).Inc()
		if initMs > 0 {
			initDuration.WithLabelValues(id).Observe(float64(initMs) / 1000)
		}
	} else {
		warmStarts.WithLabelValues(id).Inc()
	}
}

// ObserveQueueWait records how long an invocation sat in the FIFO.
func ObserveQueueWait(functionID string, queuedMs int64) {
	if queuedMs < 0 {
		queuedMs = 0
	}
	id := label(functionID)
	Remember(id)
	queueWait.WithLabelValues(id).Observe(float64(queuedMs) / 1000)
}

// Rejection counts an HTTP 429. reason is queue_full or queue_timeout.
func Rejection(functionID, reason string) {
	if reason != "queue_timeout" {
		reason = "queue_full"
	}
	id := label(functionID)
	Remember(id)
	rejections.WithLabelValues(id, reason).Inc()
}

// AsyncRetry counts one scheduled retry. The first attempt is not a retry.
func AsyncRetry(functionID string) {
	id := label(functionID)
	Remember(id)
	asyncRetries.WithLabelValues(id).Inc()
}

// ActiveInc marks one execution that holds a slot.
func ActiveInc(functionID string) {
	active.WithLabelValues(label(functionID)).Inc()
}

// ActiveDec releases the gauge incremented by ActiveInc.
func ActiveDec(functionID string) {
	active.WithLabelValues(label(functionID)).Dec()
}

func label(id string) string {
	if id == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
		if b.Len() >= 128 {
			break
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

func modeLabel(mode string) string {
	if mode == "async" {
		return "async"
	}
	return "sync"
}

func statusLabel(status string) string {
	switch status {
	case "success", "error", "timeout", "oom", "cancelled":
		return status
	default:
		return "error"
	}
}

// Sample reads one counter or gauge from the default gatherer.
// match labels must all be present; extra labels on the series are ignored
// and matching series are summed. It is used by tests.
func Sample(name string, match map[string]string) float64 {
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		return 0
	}
	var sum float64
	for _, fam := range families {
		if fam.GetName() != name {
			continue
		}
		for _, m := range fam.GetMetric() {
			if !labelsMatch(m, match) {
				continue
			}
			switch {
			case m.GetCounter() != nil:
				sum += m.GetCounter().GetValue()
			case m.GetGauge() != nil:
				sum += m.GetGauge().GetValue()
			case m.GetHistogram() != nil:
				sum += float64(m.GetHistogram().GetSampleCount())
			}
		}
	}
	return sum
}

func labelsMatch(m *dto.Metric, match map[string]string) bool {
	got := map[string]string{}
	for _, lp := range m.GetLabel() {
		got[lp.GetName()] = lp.GetValue()
	}
	for k, v := range match {
		if got[k] != v {
			return false
		}
	}
	return true
}
