package scheduler

import "github.com/PedroCarassale/nimbus/compute-plane/internal/registry"

// Worker is one live compute process the scheduler can route to.
// Live is false when the caller wants Select to ignore a snapshot that
// has already expired. List() only returns live heartbeats, so those
// conversions set Live to true.
type Worker struct {
	ID           string
	URL          string
	Active       int
	Max          int
	IdleWarmKeys []string
	State        string
	Live         bool
}

// Select picks a worker for a new execution.
//
// Healthy means live, state healthy, a URL, and effective load under the
// container cap. Effective load is the heartbeat's active count plus routes
// this process has already handed out and not finished: heartbeats are a
// couple of seconds behind, and a parallel burst would otherwise all land
// on the same worker.
//
// A non-empty warmKey prefers a healthy worker advertising that idle sandbox.
// Otherwise the lowest utilization wins (active/max). Equal utilization
// breaks toward the smaller effective count, then the smaller worker id.
// skip drops workers that already failed a dial in this attempt.
// The bool is false when nobody is eligible.
func Select(workers []Worker, warmKey string, inflight map[string]int, skip map[string]bool) (Worker, string, bool) {
	healthy := make([]Worker, 0, len(workers))
	for _, w := range workers {
		if skip[w.ID] || !eligible(w, inflight) {
			continue
		}
		healthy = append(healthy, w)
	}
	if len(healthy) == 0 {
		return Worker{}, "", false
	}
	if warmKey != "" {
		warm := make([]Worker, 0, len(healthy))
		for _, w := range healthy {
			if hasKey(w, warmKey) {
				warm = append(warm, w)
			}
		}
		if len(warm) > 0 {
			return least(warm, inflight), "warm", true
		}
	}
	return least(healthy, inflight), "least_loaded", true
}

func eligible(w Worker, inflight map[string]int) bool {
	if !w.Live || w.State != registry.StateHealthy || w.URL == "" || w.Max <= 0 {
		return false
	}
	return effective(w, inflight) < w.Max
}

func effective(w Worker, inflight map[string]int) int {
	n := w.Active
	if inflight != nil {
		n += inflight[w.ID]
	}
	if n < 0 {
		return 0
	}
	return n
}

func hasKey(w Worker, key string) bool {
	for _, k := range w.IdleWarmKeys {
		if k == key {
			return true
		}
	}
	return false
}

func least(ws []Worker, inflight map[string]int) Worker {
	best := ws[0]
	for _, w := range ws[1:] {
		if better(w, best, inflight) {
			best = w
		}
	}
	return best
}

func better(a, b Worker, inflight map[string]int) bool {
	ae := int64(effective(a, inflight))
	be := int64(effective(b, inflight))
	left := ae * int64(b.Max)
	right := be * int64(a.Max)
	if left != right {
		return left < right
	}
	if ae != be {
		return ae < be
	}
	return a.ID < b.ID
}
