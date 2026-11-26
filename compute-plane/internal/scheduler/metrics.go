package scheduler

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	dto "github.com/prometheus/client_model/go"
)

var (
	routes = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "nimbus_scheduler_routes_total",
		Help: "Requests the scheduler forwarded, by worker and reason (warm, least_loaded, failover, cancel, broadcast, any).",
	}, []string{"worker", "reason"})

	workersHealthy = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "nimbus_scheduler_workers_healthy",
		Help: "Workers with a live heartbeat and state healthy.",
	})
)

func recordRoute(workerID, reason string) {
	if workerID == "" {
		workerID = "none"
	}
	if reason == "" {
		reason = "unknown"
	}
	routes.WithLabelValues(workerID, reason).Inc()
}

func setHealthy(n int) {
	if n < 0 {
		n = 0
	}
	workersHealthy.Set(float64(n))
}

// sample reads one counter or gauge. Extra labels on a series are ignored.
// Tests use it to assert a delta.
func sample(name string, match map[string]string) float64 {
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
