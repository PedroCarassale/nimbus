package warm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/PedroCarassale/nimbus/compute-plane/internal/fnconfig"
)

// Spec is everything the runtime needs to start one container.
type Spec struct {
	Key        string
	FunctionID string
	Version    string
	WorkDir    string
	Image      string
	Config     fnconfig.Resolved
}

// Invocation is one call against a container that is already running.
type Invocation struct {
	ExecutionID string
	Event       map[string]interface{}
	// Log receives one user line. stream is "stdout" or "stderr".
	Log func(stream, line string)
	// Begin runs once the pool knows whether this call reused a container,
	// and before the handler runs.
	Begin func(Meta)
}

// Outcome is the handler result. Tainted means the container must not
// be reused: timeout, crash, OOM, or a broken protocol stream.
type Outcome struct {
	Success  bool
	Output   interface{}
	Error    string
	ExitCode int
	Stdout   string
	Stderr   string
	OOM      bool
	Tainted  bool
}

// Meta describes how the invocation was scheduled.
type Meta struct {
	ColdStart bool
	InitMs    int64
}

// Session is one running user container.
type Session interface {
	ID() string
	InitMs() int64
	Invoke(ctx context.Context, inv Invocation) Outcome
	Destroy(ctx context.Context) error
}

// Starter creates a session. Start must not return a session it has not
// already destroyed when it returns an error.
type Starter interface {
	Start(ctx context.Context, spec Spec) (Session, error)
}

type slot struct {
	id         string
	key        string
	functionID string
	session    Session
	busy       bool
	lastUsed   time.Time
}

// Pool is an in-process cache of warm sessions.
type Pool struct {
	cfg     Config
	starter Starter
	now     func() time.Time

	mu       sync.Mutex
	slots    map[string]*slot
	stopped  bool
	stop     chan struct{}
	stopOnce sync.Once
}

func New(cfg Config, starter Starter) *Pool {
	cfg = cfg.normalized()
	return &Pool{
		cfg:     cfg,
		starter: starter,
		now:     time.Now,
		slots:   make(map[string]*slot),
		stop:    make(chan struct{}),
	}
}

// StartReaper removes idle sessions after IdleTTL. Tests call Reap directly.
func (p *Pool) StartReaper() {
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-p.stop:
				return
			case <-ticker.C:
				p.Reap()
			}
		}
	}()
}

// Run serves one invocation. A matching idle session is reused. Otherwise
// a new container is started, unless this function or the process is
// already at its cap and nothing idle can be evicted. In that case the
// invocation still runs, in a container that is destroyed afterwards.
func (p *Pool) Run(ctx context.Context, spec Spec, inv Invocation) (Outcome, Meta) {
	slot, ephemeral, err := p.reserve(spec)
	if err != nil {
		return Outcome{ExitCode: 1, Error: err.Error()}, Meta{ColdStart: true}
	}

	if ephemeral {
		return p.runEphemeral(ctx, spec, inv)
	}

	cold := slot.session == nil
	if cold {
		sess, startErr := p.starter.Start(ctx, spec)
		if startErr != nil {
			p.drop(slot)
			return startFailure(ctx, startErr), Meta{ColdStart: true}
		}
		if !p.attachIfPresent(slot, sess) {
			_ = sess.Destroy(context.Background())
			return Outcome{ExitCode: 1, Error: "el pool warm está detenido"}, Meta{ColdStart: true}
		}
	}

	meta := Meta{ColdStart: cold}
	if cold && slot.session != nil {
		meta.InitMs = slot.session.InitMs()
	}
	if inv.Begin != nil {
		inv.Begin(meta)
	}

	out := slot.session.Invoke(ctx, inv)
	p.finish(slot, out)
	return out, meta
}

func (p *Pool) runEphemeral(ctx context.Context, spec Spec, inv Invocation) (Outcome, Meta) {
	sess, err := p.starter.Start(ctx, spec)
	if err != nil {
		return startFailure(ctx, err), Meta{ColdStart: true}
	}
	meta := Meta{ColdStart: true, InitMs: sess.InitMs()}
	if inv.Begin != nil {
		inv.Begin(meta)
	}
	out := sess.Invoke(ctx, inv)
	_ = sess.Destroy(context.Background())
	return out, meta
}

func startFailure(ctx context.Context, err error) Outcome {
	if ctx.Err() == context.DeadlineExceeded {
		return Outcome{ExitCode: 124, Tainted: true}
	}
	return Outcome{ExitCode: 1, Error: err.Error(), Tainted: true}
}

// reserve returns a busy slot. The session is nil when the caller must
// start one. ephemeral is true when every warm slot is busy and the
// invocation has to run outside the pool.
func (p *Pool) reserve(spec Spec) (*slot, bool, error) {
	for {
		p.mu.Lock()
		if p.stopped {
			p.mu.Unlock()
			return nil, false, fmt.Errorf("el pool warm está detenido")
		}
		if existing := p.checkoutLocked(spec.Key); existing != nil {
			p.mu.Unlock()
			return existing, false, nil
		}
		total, fn := p.countsLocked(spec.FunctionID)
		if total < p.cfg.GlobalMax && fn < p.cfg.MaxPerFunction {
			created := &slot{
				id:         newID(),
				key:        spec.Key,
				functionID: spec.FunctionID,
				busy:       true,
			}
			p.slots[created.id] = created
			p.mu.Unlock()
			return created, false, nil
		}

		sameFunction := fn >= p.cfg.MaxPerFunction
		victim := p.detachOldestIdleLocked(func(s *slot) bool {
			if sameFunction {
				return s.functionID == spec.FunctionID
			}
			return true
		})
		if victim == nil && total >= p.cfg.GlobalMax {
			victim = p.detachOldestIdleLocked(func(*slot) bool { return true })
		}
		p.mu.Unlock()

		if victim == nil {
			return nil, true, nil
		}
		if victim.session != nil {
			_ = victim.session.Destroy(context.Background())
		}
	}
}

func (p *Pool) checkoutLocked(key string) *slot {
	var found *slot
	for _, s := range p.slots {
		if s.busy || s.session == nil || s.key != key {
			continue
		}
		if found == nil || s.lastUsed.Before(found.lastUsed) {
			found = s
		}
	}
	if found == nil {
		return nil
	}
	found.busy = true
	return found
}

func (p *Pool) countsLocked(functionID string) (total, fn int) {
	for _, s := range p.slots {
		total++
		if s.functionID == functionID {
			fn++
		}
	}
	return total, fn
}

func (p *Pool) detachOldestIdleLocked(pred func(*slot) bool) *slot {
	var oldest *slot
	for _, s := range p.slots {
		if s.busy || s.session == nil || !pred(s) {
			continue
		}
		if oldest == nil || s.lastUsed.Before(oldest.lastUsed) {
			oldest = s
		}
	}
	if oldest == nil {
		return nil
	}
	delete(p.slots, oldest.id)
	return oldest
}

func (p *Pool) attachIfPresent(s *slot, sess Session) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	cur, ok := p.slots[s.id]
	if !ok || cur != s || p.stopped {
		return false
	}
	s.session = sess
	return true
}

func (p *Pool) drop(s *slot) {
	p.mu.Lock()
	delete(p.slots, s.id)
	p.mu.Unlock()
}

func (p *Pool) finish(s *slot, out Outcome) {
	p.mu.Lock()
	cur, ok := p.slots[s.id]
	remove := !ok || cur != s || p.stopped || out.Tainted
	if remove && ok && cur == s {
		delete(p.slots, s.id)
	}
	sess := s.session
	if !remove {
		s.busy = false
		s.lastUsed = p.now()
	}
	p.mu.Unlock()

	if remove && sess != nil {
		_ = sess.Destroy(context.Background())
	}
}

// Reap destroys idle sessions that have been unused for IdleTTL.
func (p *Pool) Reap() int {
	now := p.now()
	var victims []Session
	p.mu.Lock()
	for id, s := range p.slots {
		if s.busy || s.session == nil {
			continue
		}
		if now.Sub(s.lastUsed) >= p.cfg.IdleTTL {
			delete(p.slots, id)
			victims = append(victims, s.session)
		}
	}
	p.mu.Unlock()
	for _, sess := range victims {
		_ = sess.Destroy(context.Background())
	}
	return len(victims)
}

// Shutdown destroys every session and rejects later Run calls.
func (p *Pool) Shutdown(ctx context.Context) {
	p.stopOnce.Do(func() { close(p.stop) })

	p.mu.Lock()
	p.stopped = true
	victims := make([]Session, 0, len(p.slots))
	for id, s := range p.slots {
		delete(p.slots, id)
		if s.session != nil {
			victims = append(victims, s.session)
		}
	}
	p.mu.Unlock()

	for _, sess := range victims {
		_ = sess.Destroy(ctx)
	}
}

// Idle reports how many sessions are waiting for work.
func (p *Pool) Idle() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, s := range p.slots {
		if !s.busy && s.session != nil {
			n++
		}
	}
	return n
}

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("slot-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
