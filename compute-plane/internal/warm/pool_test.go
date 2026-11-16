package warm

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PedroCarassale/nimbus/compute-plane/internal/cancel"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/fnconfig"
)

type fakeSession struct {
	mu        sync.Mutex
	id        string
	initMs    int64
	invokes   int
	destroyed bool
	tainted   bool
	hold      chan struct{}
	entered   chan struct{}
	onInvoke  func(Invocation)
}

func (f *fakeSession) ID() string { return f.id }

func (f *fakeSession) InitMs() int64 { return f.initMs }

func (f *fakeSession) Invoke(ctx context.Context, inv Invocation) Outcome {
	f.mu.Lock()
	f.invokes++
	tainted := f.tainted
	f.mu.Unlock()

	if f.entered != nil {
		select {
		case f.entered <- struct{}{}:
		default:
		}
	}
	if f.onInvoke != nil {
		f.onInvoke(inv)
	}
	if f.hold != nil {
		select {
		case <-f.hold:
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				return Outcome{ExitCode: 124, Tainted: true}
			}
			return Outcome{ExitCode: 1, Tainted: true, Error: "cancelada"}
		}
	}
	if inv.Log != nil {
		inv.Log("stdout", "hola "+inv.ExecutionID)
	}
	if tainted {
		return Outcome{ExitCode: 137, OOM: true, Tainted: true, Error: "oom"}
	}
	return Outcome{Success: true, ExitCode: 0, Output: map[string]interface{}{"ok": true}}
}

func (f *fakeSession) Destroy(context.Context) error {
	f.mu.Lock()
	f.destroyed = true
	f.mu.Unlock()
	return nil
}

func (f *fakeSession) snapshot() (invokes int, destroyed bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.invokes, f.destroyed
}

type fakeStarter struct {
	mu       sync.Mutex
	sessions []*fakeSession
	failNext bool
	initMs   int64
}

func (f *fakeStarter) Start(_ context.Context, spec Spec) (Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext {
		f.failNext = false
		return nil, fmt.Errorf("docker no arrancó")
	}
	s := &fakeSession{
		id:     fmt.Sprintf("s-%d-%s", len(f.sessions)+1, spec.FunctionID),
		initMs: f.initMs,
	}
	f.sessions = append(f.sessions, s)
	return s, nil
}

func (f *fakeStarter) list() []*fakeSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*fakeSession, len(f.sessions))
	copy(out, f.sessions)
	return out
}

func testPool(t *testing.T, cfg Config) (*Pool, *fakeStarter) {
	t.Helper()
	starter := &fakeStarter{initMs: 80}
	p := New(cfg, starter)
	p.now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	return p, starter
}

func specOf(functionID, version, handler string, memory, cpus int, env ...fnconfig.EnvVar) Spec {
	cfg := fnconfig.Resolved{
		TimeoutSec: 5,
		MemoryMB:   memory,
		CPUsMilli:  cpus,
		Handler:    handler,
		Env:        env,
	}
	return Spec{
		Key:        PoolKey(functionID, version, cfg),
		FunctionID: functionID,
		Version:    version,
		WorkDir:    "/tmp/work",
		Image:      "nimbus-node",
		Config:     cfg,
	}
}

func TestPoolReusesMatchingContainer(t *testing.T) {
	p, starter := testPool(t, Config{IdleTTL: time.Minute, MaxPerFunction: 2, GlobalMax: 4})
	spec := specOf("fn", "v1", "handler", 128, 0)

	out, meta := p.Run(context.Background(), spec, Invocation{ExecutionID: "a"})
	if !out.Success || !meta.ColdStart || meta.InitMs != 80 {
		t.Fatalf("first out=%+v meta=%+v", out, meta)
	}
	out, meta = p.Run(context.Background(), spec, Invocation{ExecutionID: "b"})
	if !out.Success || meta.ColdStart || meta.InitMs != 0 {
		t.Fatalf("second out=%+v meta=%+v", out, meta)
	}
	if got := len(starter.list()); got != 1 {
		t.Fatalf("starts = %d", got)
	}
	if invokes, _ := starter.list()[0].snapshot(); invokes != 2 {
		t.Fatalf("invokes = %d", invokes)
	}
}

func TestPoolKeySeparatesConfig(t *testing.T) {
	base := specOf("fn", "v1", "handler", 128, 0, fnconfig.EnvVar{Name: "A", Value: "1"})
	sameOrder := specOf("fn", "v1", "handler", 128, 0, fnconfig.EnvVar{Name: "A", Value: "1"})
	if base.Key != sameOrder.Key {
		t.Fatal("same env must hash the same")
	}
	swapped := specOf("fn", "v1", "handler", 128, 0,
		fnconfig.EnvVar{Name: "B", Value: "2"},
		fnconfig.EnvVar{Name: "A", Value: "1"},
	)
	reordered := specOf("fn", "v1", "handler", 128, 0,
		fnconfig.EnvVar{Name: "A", Value: "1"},
		fnconfig.EnvVar{Name: "B", Value: "2"},
	)
	if swapped.Key != reordered.Key {
		t.Fatal("env order must not change the key")
	}
	if base.Key == swapped.Key {
		t.Fatal("different env must not share a key")
	}

	cases := []Spec{
		specOf("other", "v1", "handler", 128, 0),
		specOf("fn", "v2", "handler", 128, 0),
		specOf("fn", "v1", "src/app.main", 128, 0),
		specOf("fn", "v1", "handler", 256, 0),
		specOf("fn", "v1", "handler", 128, 500),
		specOf("fn", "v1", "handler", 128, 0, fnconfig.EnvVar{Name: "A", Value: "x"}),
	}
	seen := map[string]bool{base.Key: true}
	for _, spec := range cases {
		if seen[spec.Key] {
			t.Fatalf("key collision for %#v", spec.Config)
		}
		seen[spec.Key] = true
	}

	p, starter := testPool(t, Config{IdleTTL: time.Minute, MaxPerFunction: 4, GlobalMax: 8})
	p.Run(context.Background(), base, Invocation{ExecutionID: "1"})
	p.Run(context.Background(), cases[2], Invocation{ExecutionID: "2"})
	if got := len(starter.list()); got != 2 {
		t.Fatalf("different handlers started %d sessions", got)
	}
}

func TestPoolCancelDiscardsBusySession(t *testing.T) {
	p, _ := testPool(t, Config{IdleTTL: time.Minute, MaxPerFunction: 2, GlobalMax: 4})
	busy := &fakeSession{id: "busy", initMs: 5, hold: make(chan struct{}), entered: make(chan struct{}, 1)}
	next := &fakeSession{id: "next", initMs: 6}
	p.starter = &scriptedStarter{sessions: []*fakeSession{busy, next}}
	spec := specOf("fn", "v1", "handler", 128, 0)

	ctx, stop := context.WithCancelCause(context.Background())
	done := make(chan Outcome, 1)
	go func() {
		out, _ := p.Run(ctx, spec, Invocation{ExecutionID: "run-1"})
		done <- out
	}()
	select {
	case <-busy.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("invoke did not start")
	}
	stop(cancel.ErrRun)

	var out Outcome
	select {
	case out = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled invoke did not return")
	}
	if out.Success || !out.Tainted || out.Error != cancel.Message || out.ExitCode != cancel.ExitCode {
		t.Fatalf("cancel out=%+v", out)
	}
	if _, destroyed := busy.snapshot(); !destroyed {
		t.Fatal("cancelled warm session was kept")
	}
	if p.Idle() != 0 {
		t.Fatalf("idle after cancel = %d", p.Idle())
	}

	out, meta := p.Run(context.Background(), spec, Invocation{ExecutionID: "run-2"})
	if !out.Success || !meta.ColdStart {
		t.Fatalf("next invoke reused the cancelled container out=%+v meta=%+v", out, meta)
	}
}

func TestPoolDiscardsTaintedSession(t *testing.T) {
	p, _ := testPool(t, Config{IdleTTL: time.Minute, MaxPerFunction: 2, GlobalMax: 4})
	spec := specOf("fn", "v1", "handler", 128, 0)
	p.starter = &scriptedStarter{sessions: []*fakeSession{
		{id: "bad", tainted: true, initMs: 10},
		{id: "good", initMs: 11},
	}}

	out, meta := p.Run(context.Background(), spec, Invocation{ExecutionID: "1"})
	if out.Success || !out.Tainted || !out.OOM || !meta.ColdStart {
		t.Fatalf("tainted out=%+v meta=%+v", out, meta)
	}
	out, meta = p.Run(context.Background(), spec, Invocation{ExecutionID: "2"})
	if !out.Success || !meta.ColdStart {
		t.Fatalf("reuse after taint out=%+v meta=%+v", out, meta)
	}
	bad := p.starter.(*scriptedStarter).sessions[0]
	if _, destroyed := bad.snapshot(); !destroyed {
		t.Fatal("tainted session was kept")
	}
	if p.Idle() != 1 {
		t.Fatalf("idle = %d", p.Idle())
	}
}

type scriptedStarter struct {
	mu       sync.Mutex
	sessions []*fakeSession
	n        int
}

func (s *scriptedStarter) Start(context.Context, Spec) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.n >= len(s.sessions) {
		return nil, fmt.Errorf("no more sessions")
	}
	sess := s.sessions[s.n]
	s.n++
	return sess, nil
}

func TestPoolIdleTTL(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	p, starter := testPool(t, Config{IdleTTL: 10 * time.Second, MaxPerFunction: 2, GlobalMax: 4})
	p.now = func() time.Time { return now }
	spec := specOf("fn", "v1", "handler", 128, 0)

	if _, meta := p.Run(context.Background(), spec, Invocation{ExecutionID: "1"}); !meta.ColdStart {
		t.Fatal("expected cold")
	}
	now = now.Add(9 * time.Second)
	if n := p.Reap(); n != 0 {
		t.Fatalf("reaped %d before the TTL", n)
	}
	if _, meta := p.Run(context.Background(), spec, Invocation{ExecutionID: "2"}); meta.ColdStart {
		t.Fatal("still inside the TTL, expected warm")
	}
	now = now.Add(10 * time.Second)
	if n := p.Reap(); n != 1 {
		t.Fatalf("reaped %d, want 1", n)
	}
	if _, destroyed := starter.list()[0].snapshot(); !destroyed {
		t.Fatal("expired session was not destroyed")
	}
	if _, meta := p.Run(context.Background(), spec, Invocation{ExecutionID: "3"}); !meta.ColdStart {
		t.Fatal("expected a new container after the TTL")
	}
	if got := len(starter.list()); got != 2 {
		t.Fatalf("starts = %d", got)
	}
}

func TestPoolReapSkipsBusy(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	p, _ := testPool(t, Config{IdleTTL: time.Second, MaxPerFunction: 2, GlobalMax: 4})
	p.now = func() time.Time { return now }
	hold := make(chan struct{})
	entered := make(chan struct{}, 1)
	p.starter = &scriptedStarter{sessions: []*fakeSession{{
		id: "busy", initMs: 1, hold: hold, entered: entered,
	}}}
	spec := specOf("fn", "v1", "handler", 128, 0)

	done := make(chan Outcome, 1)
	go func() {
		out, _ := p.Run(context.Background(), spec, Invocation{ExecutionID: "1"})
		done <- out
	}()
	<-entered
	now = now.Add(time.Minute)
	if n := p.Reap(); n != 0 {
		t.Fatalf("reaped a busy session: %d", n)
	}
	close(hold)
	out := <-done
	if !out.Success {
		t.Fatalf("out %+v", out)
	}
	now = now.Add(time.Minute)
	if n := p.Reap(); n != 1 {
		t.Fatalf("idle reap = %d", n)
	}
}

func TestPoolEvictsLRUIdleAtGlobalCap(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	p, _ := testPool(t, Config{IdleTTL: time.Hour, MaxPerFunction: 2, GlobalMax: 2})
	p.now = func() time.Time { return now }
	sessions := []*fakeSession{
		{id: "old", initMs: 1},
		{id: "new", initMs: 1},
		{id: "third", initMs: 1},
	}
	p.starter = &scriptedStarter{sessions: sessions}

	p.Run(context.Background(), specOf("fn-a", "v", "handler", 128, 0), Invocation{ExecutionID: "a"})
	now = now.Add(time.Second)
	p.Run(context.Background(), specOf("fn-b", "v", "handler", 128, 0), Invocation{ExecutionID: "b"})
	now = now.Add(time.Second)
	out, meta := p.Run(context.Background(), specOf("fn-c", "v", "handler", 128, 0), Invocation{ExecutionID: "c"})
	if !out.Success || !meta.ColdStart {
		t.Fatalf("out=%+v meta=%+v", out, meta)
	}
	if _, destroyed := sessions[0].snapshot(); !destroyed {
		t.Fatal("oldest idle session should have been evicted")
	}
	if _, destroyed := sessions[1].snapshot(); destroyed {
		t.Fatal("newer idle session was evicted")
	}
	if p.Idle() != 2 {
		t.Fatalf("idle = %d", p.Idle())
	}
}

func TestPoolEvictsIdleOfSameFunctionAtCap(t *testing.T) {
	p, _ := testPool(t, Config{IdleTTL: time.Hour, MaxPerFunction: 1, GlobalMax: 4})
	sessions := []*fakeSession{
		{id: "handler-a", initMs: 1},
		{id: "handler-b", initMs: 1},
	}
	p.starter = &scriptedStarter{sessions: sessions}

	p.Run(context.Background(), specOf("fn", "v", "handler", 128, 0), Invocation{ExecutionID: "1"})
	out, meta := p.Run(context.Background(), specOf("fn", "v", "other.main", 128, 0), Invocation{ExecutionID: "2"})
	if !out.Success || !meta.ColdStart {
		t.Fatalf("out=%+v meta=%+v", out, meta)
	}
	if _, destroyed := sessions[0].snapshot(); !destroyed {
		t.Fatal("the idle container of this function should have been replaced")
	}
	if _, destroyed := sessions[1].snapshot(); destroyed {
		t.Fatal("the replacement was destroyed")
	}
}

func TestPoolEphemeralWhenEverySlotIsBusy(t *testing.T) {
	p, _ := testPool(t, Config{IdleTTL: time.Hour, MaxPerFunction: 1, GlobalMax: 2})
	hold := make(chan struct{})
	entered := make(chan struct{}, 1)
	first := &fakeSession{id: "busy", hold: hold, entered: entered, initMs: 1}
	extra := &fakeSession{id: "extra", initMs: 1}
	p.starter = &scriptedStarter{sessions: []*fakeSession{first, extra}}
	spec := specOf("fn", "v", "handler", 128, 0)

	done := make(chan struct{})
	go func() {
		p.Run(context.Background(), spec, Invocation{ExecutionID: "1"})
		close(done)
	}()
	<-entered

	out, meta := p.Run(context.Background(), spec, Invocation{ExecutionID: "2"})
	if !out.Success || !meta.ColdStart {
		t.Fatalf("ephemeral out=%+v meta=%+v", out, meta)
	}
	if _, destroyed := extra.snapshot(); !destroyed {
		t.Fatal("ephemeral session must be destroyed after the call")
	}
	if _, destroyed := first.snapshot(); destroyed {
		t.Fatal("busy session must stay")
	}
	close(hold)
	<-done
}

func TestPoolConcurrentCallsDoNotShareASession(t *testing.T) {
	p, _ := testPool(t, Config{IdleTTL: time.Hour, MaxPerFunction: 2, GlobalMax: 4})
	var mu sync.Mutex
	var sessions []*fakeSession
	p.starter = starterFunc(func(Spec) (Session, error) {
		s := &fakeSession{
			initMs:  5,
			hold:    make(chan struct{}),
			entered: make(chan struct{}, 1),
		}
		mu.Lock()
		s.id = fmt.Sprintf("c-%d", len(sessions)+1)
		sessions = append(sessions, s)
		mu.Unlock()
		return s, nil
	})

	spec := specOf("fn", "v", "handler", 128, 0)
	errCh := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			out, _ := p.Run(context.Background(), spec, Invocation{ExecutionID: fmt.Sprintf("e%d", i)})
			if !out.Success {
				errCh <- fmt.Errorf("invoke %d failed: %+v", i, out)
				return
			}
			errCh <- nil
		}(i)
	}

	deadline := time.After(2 * time.Second)
	ready := 0
	for ready < 2 {
		mu.Lock()
		current := append([]*fakeSession(nil), sessions...)
		mu.Unlock()
		for _, s := range current {
			select {
			case <-s.entered:
				ready++
			default:
			}
		}
		if ready >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for both sessions")
		case <-time.After(5 * time.Millisecond):
		}
	}
	mu.Lock()
	if len(sessions) != 2 {
		t.Fatalf("starts = %d", len(sessions))
	}
	for _, s := range sessions {
		close(s.hold)
	}
	mu.Unlock()
	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range sessions {
		if n, _ := s.snapshot(); n != 1 {
			t.Fatalf("session %s invokes = %d", s.id, n)
		}
	}
}

type starterFunc func(Spec) (Session, error)

func (f starterFunc) Start(_ context.Context, spec Spec) (Session, error) {
	return f(spec)
}

func TestPoolLogsStayWithTheInvocation(t *testing.T) {
	p, _ := testPool(t, Config{IdleTTL: time.Minute, MaxPerFunction: 1, GlobalMax: 2})
	p.starter = &scriptedStarter{sessions: []*fakeSession{{id: "s", initMs: 1}}}
	spec := specOf("fn", "v", "handler", 128, 0)

	var mu sync.Mutex
	got := map[string][]string{}
	for _, id := range []string{"exec-a", "exec-b"} {
		id := id
		_, _ = p.Run(context.Background(), spec, Invocation{
			ExecutionID: id,
			Log: func(stream, line string) {
				mu.Lock()
				got[id] = append(got[id], stream+":"+line)
				mu.Unlock()
			},
		})
	}
	if len(got["exec-a"]) != 1 || got["exec-a"][0] != "stdout:hola exec-a" {
		t.Fatalf("a = %#v", got["exec-a"])
	}
	if len(got["exec-b"]) != 1 || got["exec-b"][0] != "stdout:hola exec-b" {
		t.Fatalf("b = %#v", got["exec-b"])
	}
}

func TestPoolShutdownDestroysSessionsAndRejects(t *testing.T) {
	p, starter := testPool(t, Config{IdleTTL: time.Hour, MaxPerFunction: 2, GlobalMax: 4})
	spec := specOf("fn", "v", "handler", 128, 0)
	p.Run(context.Background(), spec, Invocation{ExecutionID: "1"})
	p.Shutdown(context.Background())
	if _, destroyed := starter.list()[0].snapshot(); !destroyed {
		t.Fatal("shutdown left the session running")
	}
	out, meta := p.Run(context.Background(), spec, Invocation{ExecutionID: "2"})
	if out.Success || !meta.ColdStart || out.Error == "" {
		t.Fatalf("run after shutdown out=%+v meta=%+v", out, meta)
	}
	if got := len(starter.list()); got != 1 {
		t.Fatalf("shutdown started another session: %d", got)
	}
}

func TestPoolStartErrorDoesNotOccupyTheSlot(t *testing.T) {
	p, starter := testPool(t, Config{IdleTTL: time.Hour, MaxPerFunction: 1, GlobalMax: 2})
	starter.failNext = true
	spec := specOf("fn", "v", "handler", 128, 0)
	out, meta := p.Run(context.Background(), spec, Invocation{ExecutionID: "1"})
	if out.Success || !meta.ColdStart || out.Error == "" {
		t.Fatalf("out=%+v", out)
	}
	if p.Idle() != 0 {
		t.Fatalf("idle after failed start = %d", p.Idle())
	}
	out, meta = p.Run(context.Background(), spec, Invocation{ExecutionID: "2"})
	if !out.Success || !meta.ColdStart {
		t.Fatalf("retry out=%+v meta=%+v", out, meta)
	}
}

func TestPoolBeginFiresBeforeInvoke(t *testing.T) {
	p, _ := testPool(t, Config{IdleTTL: time.Hour, MaxPerFunction: 1, GlobalMax: 2})
	order := make(chan string, 4)
	sess := &fakeSession{id: "s", initMs: 15, onInvoke: func(Invocation) {
		order <- "invoke"
	}}
	p.starter = &scriptedStarter{sessions: []*fakeSession{sess}}
	spec := specOf("fn", "v", "handler", 128, 0)
	var cold bool
	p.Run(context.Background(), spec, Invocation{
		ExecutionID: "1",
		Begin: func(meta Meta) {
			cold = meta.ColdStart
			order <- "begin"
		},
	})
	if !cold {
		t.Fatal("expected cold begin")
	}
	if got := <-order; got != "begin" {
		t.Fatalf("first event %s", got)
	}
	if got := <-order; got != "invoke" {
		t.Fatalf("second event %s", got)
	}
}

func TestPoolStress(t *testing.T) {
	p, _ := testPool(t, Config{IdleTTL: time.Hour, MaxPerFunction: 2, GlobalMax: 4})
	var n atomic.Int64
	p.starter = starterFunc(func(spec Spec) (Session, error) {
		return &fakeSession{id: fmt.Sprintf("%s-%d", spec.FunctionID, n.Add(1)), initMs: 3}, nil
	})
	var wg sync.WaitGroup
	errCh := make(chan error, 30)
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			spec := specOf(fmt.Sprintf("fn-%d", i%3), "v1", "handler", 128, 0)
			out, _ := p.Run(context.Background(), spec, Invocation{ExecutionID: fmt.Sprintf("e-%d", i)})
			if !out.Success || out.ExitCode != 0 {
				errCh <- fmt.Errorf("i=%d out=%+v", i, out)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

func TestLoadConfig(t *testing.T) {
	t.Setenv("NIMBUS_WARM", "")
	t.Setenv("NIMBUS_WARM_IDLE_SEC", "")
	t.Setenv("NIMBUS_FN_CONCURRENCY", "")
	t.Setenv("NIMBUS_MAX_CONTAINERS", "")
	cfg := LoadConfig()
	if !cfg.Enabled || cfg.IdleTTL != time.Minute || cfg.MaxPerFunction != 2 || cfg.GlobalMax != 8 {
		t.Fatalf("defaults %+v", cfg)
	}

	t.Setenv("NIMBUS_WARM", "off")
	t.Setenv("NIMBUS_WARM_IDLE_SEC", "6")
	t.Setenv("NIMBUS_FN_CONCURRENCY", "3")
	t.Setenv("NIMBUS_MAX_CONTAINERS", "5")
	cfg = LoadConfig()
	if cfg.Enabled || cfg.IdleTTL != 6*time.Second || cfg.MaxPerFunction != 3 || cfg.GlobalMax != 5 {
		t.Fatalf("override %+v", cfg)
	}

	t.Setenv("NIMBUS_WARM", "0")
	if LoadConfig().Enabled {
		t.Fatal("0 should disable warm")
	}
	t.Setenv("NIMBUS_WARM", "true")
	if !LoadConfig().Enabled {
		t.Fatal("true should enable warm")
	}

	t.Setenv("NIMBUS_FN_CONCURRENCY", "9")
	t.Setenv("NIMBUS_MAX_CONTAINERS", "4")
	cfg = LoadConfig()
	if cfg.MaxPerFunction != 4 || cfg.GlobalMax != 4 {
		t.Fatalf("per-function cap should follow the global cap: %+v", cfg)
	}
}
