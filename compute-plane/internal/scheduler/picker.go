package scheduler

import (
	"sync"
)

// Picker is Select plus the in-flight reservations of this process.
// Choose and Done are safe for concurrent requests.
type Picker struct {
	mu       sync.Mutex
	inflight map[string]int
}

func NewPicker() *Picker {
	return &Picker{inflight: map[string]int{}}
}

// Choose reserves one slot on the selected worker. The caller must Done it
// when the proxied request finishes, including a dial failure before the
// failover attempt.
func (p *Picker) Choose(workers []Worker, warmKey string, skip map[string]bool) (Worker, string, bool) {
	if p == nil {
		w, reason, ok := Select(workers, warmKey, nil, skip)
		return w, reason, ok
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	w, reason, ok := Select(workers, warmKey, p.inflight, skip)
	if ok {
		p.inflight[w.ID]++
	}
	return w, reason, ok
}

// Done releases the reservation from Choose.
func (p *Picker) Done(id string) {
	if p == nil || id == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.inflight[id] > 0 {
		p.inflight[id]--
		if p.inflight[id] == 0 {
			delete(p.inflight, id)
		}
	}
}
