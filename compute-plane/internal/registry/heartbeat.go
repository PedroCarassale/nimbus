package registry

import (
	"context"
	"log"
	"sync"
	"time"
)

// Source fills the counters and idle keys of one heartbeat. WorkerID, URL and
// State are set by the beater.
type Source func() Snapshot

// Beater refreshes a registration until Drain or Stop.
type Beater struct {
	reg      *Registry
	source   Source
	interval time.Duration
	id       string
	url      string

	mu    sync.Mutex
	state string
	stop  chan struct{}
	done  chan struct{}
}

// Start begins the heartbeat loop. The first beat happens before Start returns
// so a scheduler can see the worker as soon as the process is up.
func Start(reg *Registry, source Source, interval time.Duration, id, url string) *Beater {
	if interval <= 0 {
		interval = DefaultInterval
	}
	if source == nil {
		source = func() Snapshot { return Snapshot{} }
	}
	b := &Beater{
		reg:      reg,
		source:   source,
		interval: interval,
		id:       id,
		url:      url,
		state:    StateHealthy,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	b.publish()
	go b.loop()
	return b
}

func (b *Beater) loop() {
	defer close(b.done)
	ticker := time.NewTicker(b.interval)
	defer ticker.Stop()
	for {
		select {
		case <-b.stop:
			return
		case <-ticker.C:
			b.publish()
		}
	}
}

func (b *Beater) publish() {
	if b == nil || b.reg == nil {
		return
	}
	snap := b.source()
	snap.WorkerID = b.id
	snap.URL = b.url
	b.mu.Lock()
	state := b.state
	b.mu.Unlock()
	snap.State = state
	b.write(snap)
	// A shutdown can flip the state while this beat was in flight. Never
	// leave a healthy registration on top of draining.
	b.mu.Lock()
	now := b.state
	b.mu.Unlock()
	if now != state {
		snap.State = now
		b.write(snap)
	}
}

func (b *Beater) write(snap Snapshot) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := b.reg.Heartbeat(ctx, snap); err != nil {
		log.Printf("[registry] heartbeat %s: %v", b.id, err)
	}
}

// Drain marks the worker draining and keeps refreshing that state so the TTL
// does not hide it while in-flight executions finish. New routes stop. Cancel
// can still resolve the URL.
func (b *Beater) Drain() {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.state = StateDraining
	b.mu.Unlock()
	b.publish()
}

// Stop ends the loop and deletes the registration.
func (b *Beater) Stop() {
	if b == nil {
		return
	}
	select {
	case <-b.stop:
	default:
		close(b.stop)
	}
	<-b.done
	if b.reg == nil || b.id == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := b.reg.Remove(ctx, b.id); err != nil {
		log.Printf("[registry] remove %s: %v", b.id, err)
	}
}
