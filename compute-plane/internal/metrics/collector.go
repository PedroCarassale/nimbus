package metrics

import (
	"context"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// WarmSample is one warm-pool series: how many containers of a function are
// idle or busy.
type WarmSample struct {
	FunctionID string
	State      string
	Count      int
}

// Sources are read on each scrape. Nil funcs report zero.
type Sources struct {
	QueueDepth      func(ctx context.Context) map[string]float64
	DLQEntries      func(ctx context.Context) float64
	ContainersInUse func(ctx context.Context) float64
	ContainersLimit float64
	WarmPool        func() []WarmSample
}

var (
	srcMu sync.RWMutex
	src   Sources

	queueDepthDesc = prometheus.NewDesc(
		"nimbus_queue_depth",
		"Invocations waiting in the per-function FIFO.",
		[]string{"function_id"},
		nil,
	)
	dlqDesc = prometheus.NewDesc(
		"nimbus_dlq_entries",
		"Invocations currently stored in the async dead-letter queue.",
		nil,
		nil,
	)
	containersDesc = prometheus.NewDesc(
		"nimbus_containers_in_use",
		"Execution slots held on this machine. Idle warm containers are counted in nimbus_warm_pool_size.",
		nil,
		nil,
	)
	containersLimitDesc = prometheus.NewDesc(
		"nimbus_containers_limit",
		"Configured global execution cap (NIMBUS_MAX_CONTAINERS).",
		nil,
		nil,
	)
	warmPoolDesc = prometheus.NewDesc(
		"nimbus_warm_pool_size",
		"Warm containers in this process, by function and state (idle or busy).",
		[]string{"function_id", "state"},
		nil,
	)
)

func init() {
	prometheus.MustRegister(liveCollector{})
}

// SetSources replaces the scrape callbacks. main calls it once the Redis
// client and the warm pool exist.
func SetSources(s Sources) {
	srcMu.Lock()
	src = s
	srcMu.Unlock()
}

type liveCollector struct{}

func (liveCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- queueDepthDesc
	ch <- dlqDesc
	ch <- containersDesc
	ch <- containersLimitDesc
	ch <- warmPoolDesc
}

func (liveCollector) Collect(ch chan<- prometheus.Metric) {
	srcMu.RLock()
	current := src
	srcMu.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	depths := map[string]float64{}
	for _, id := range rememberedIDs() {
		depths[id] = 0
	}
	if current.QueueDepth != nil {
		for id, n := range current.QueueDepth(ctx) {
			if n < 0 {
				n = 0
			}
			depths[label(id)] = n
		}
	}
	for id, n := range depths {
		ch <- prometheus.MustNewConstMetric(queueDepthDesc, prometheus.GaugeValue, n, id)
	}

	dlq := 0.0
	if current.DLQEntries != nil {
		dlq = current.DLQEntries(ctx)
		if dlq < 0 {
			dlq = 0
		}
	}
	ch <- prometheus.MustNewConstMetric(dlqDesc, prometheus.GaugeValue, dlq)

	inUse := 0.0
	if current.ContainersInUse != nil {
		inUse = current.ContainersInUse(ctx)
		if inUse < 0 {
			inUse = 0
		}
	}
	ch <- prometheus.MustNewConstMetric(containersDesc, prometheus.GaugeValue, inUse)

	limit := current.ContainersLimit
	if limit < 0 {
		limit = 0
	}
	ch <- prometheus.MustNewConstMetric(containersLimitDesc, prometheus.GaugeValue, limit)

	if current.WarmPool != nil {
		for _, sample := range current.WarmPool() {
			state := sample.State
			if state != "busy" {
				state = "idle"
			}
			n := sample.Count
			if n < 0 {
				n = 0
			}
			ch <- prometheus.MustNewConstMetric(warmPoolDesc, prometheus.GaugeValue, float64(n), label(sample.FunctionID), state)
		}
	}
}
