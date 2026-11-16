package concurrency

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	m := NewManager(rdb, Config{
		MaxConcurrency: 2,
		QueueMax:       3,
		GlobalMax:      8,
		MaxWait:        2 * time.Second,
		Lease:          time.Hour,
		PollInterval:   10 * time.Millisecond,
	})
	m.machineID = "test-machine"
	t.Cleanup(m.stopAll)
	return m
}

func waitStatus(t *testing.T, m *Manager, executionID, status string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := m.rdb.HGet(context.Background(), m.execKey(executionID), "status").Result()
		if err == nil && got == status {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	got, _ := m.rdb.HGet(context.Background(), m.execKey(executionID), "status").Result()
	t.Fatalf("exec %s status = %q, want %s", executionID, got, status)
}

func TestNConcurrentThenQueue(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	start := time.Now()
	for _, id := range []string{"a", "b"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_, err := m.Acquire(ctx, "fn", id, 2)
			errCh <- err
		}(id)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatalf("two slots should be granted immediately, took %s", time.Since(start))
	}

	got := make(chan error, 1)
	go func() {
		_, err := m.Acquire(ctx, "fn", "c", 2)
		got <- err
	}()
	waitStatus(t, m, "c", "queued")
	select {
	case err := <-got:
		t.Fatalf("third acquire returned early: %v", err)
	case <-time.After(40 * time.Millisecond):
	}

	if err := m.Release(ctx, "fn", "a"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued execution did not start after a release")
	}
}

func TestFIFO(t *testing.T) {
	m := newTestManager(t)
	m.cfg.MaxConcurrency = 1
	ctx := context.Background()

	if _, err := m.Acquire(ctx, "fn", "holder", 1); err != nil {
		t.Fatal(err)
	}

	order := make(chan string, 2)
	go func() {
		if _, err := m.Acquire(ctx, "fn", "a", 1); err != nil {
			t.Errorf("a: %v", err)
			order <- "a-err"
			return
		}
		order <- "a"
	}()
	waitStatus(t, m, "a", "queued")

	go func() {
		if _, err := m.Acquire(ctx, "fn", "b", 1); err != nil {
			t.Errorf("b: %v", err)
			order <- "b-err"
			return
		}
		order <- "b"
	}()
	waitStatus(t, m, "b", "queued")

	if err := m.Release(ctx, "fn", "holder"); err != nil {
		t.Fatal(err)
	}
	first := <-order
	if err := m.Release(ctx, "fn", first); err != nil {
		t.Fatal(err)
	}
	second := <-order
	if first != "a" || second != "b" {
		t.Fatalf("order = %s then %s, want a then b", first, second)
	}
}

func TestQueueFullRejectsWithoutWaiting(t *testing.T) {
	m := newTestManager(t)
	m.cfg.MaxConcurrency = 1
	m.cfg.QueueMax = 1
	m.cfg.MaxWait = 3 * time.Second
	ctx := context.Background()

	if _, err := m.Acquire(ctx, "fn", "holder", 1); err != nil {
		t.Fatal(err)
	}
	go func() {
		_, _ = m.Acquire(ctx, "fn", "waiter", 1)
	}()
	waitStatus(t, m, "waiter", "queued")

	start := time.Now()
	_, err := m.Acquire(ctx, "fn", "overflow", 1)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("err = %v, want ErrQueueFull", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("queue full waited %s", elapsed)
	}
	n, err := m.rdb.ZCard(ctx, m.queueKey("fn")).Result()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("queue len = %d, overflow should not be enqueued", n)
	}
	if _, err := m.rdb.Exists(ctx, m.execKey("overflow")).Result(); err != nil {
		t.Fatal(err)
	}
	exists, _ := m.rdb.Exists(ctx, m.execKey("overflow")).Result()
	if exists != 0 {
		t.Fatal("rejected execution should not keep a record")
	}
}

func TestQueueWaitTimeout(t *testing.T) {
	m := newTestManager(t)
	m.cfg.MaxConcurrency = 1
	m.cfg.MaxWait = 120 * time.Millisecond
	m.cfg.QueueLease = time.Second
	ctx := context.Background()

	if _, err := m.Acquire(ctx, "fn", "holder", 1); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	queuedMs, err := m.Acquire(ctx, "fn", "waiter", 1)
	if !errors.Is(err, ErrWaitTimeout) {
		t.Fatalf("err = %v, want ErrWaitTimeout", err)
	}
	if queuedMs < 50 {
		t.Fatalf("queuedMs = %d, want the time spent waiting", queuedMs)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("timeout took too long: %s", time.Since(start))
	}
	n, err := m.rdb.ZCard(ctx, m.queueKey("fn")).Result()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("queue len = %d after timeout", n)
	}
}

func TestSlotLeaseRecovery(t *testing.T) {
	m := newTestManager(t)
	m.cfg.MaxConcurrency = 1
	m.cfg.GlobalMax = 1
	m.cfg.Lease = time.Second
	base := time.Unix(1_700_000_000, 0)
	m.now = func() time.Time { return base }
	ctx := context.Background()

	if _, err := m.Acquire(ctx, "fn", "dead", 1); err != nil {
		t.Fatal(err)
	}
	m.stopKeepAlive("dead")

	// Still inside the original lease: a refresh must extend it.
	m.now = func() time.Time { return base.Add(400 * time.Millisecond) }
	if err := m.touch(ctx, "fn", "dead"); err != nil {
		t.Fatal(err)
	}
	// Past the original expiry, still inside the refreshed lease.
	m.now = func() time.Time { return base.Add(1200 * time.Millisecond) }
	status, err := m.admit(ctx, "fn", "other", 1)
	if err != nil {
		t.Fatal(err)
	}
	if status != "queued" {
		t.Fatalf("refreshed slot status = %s, want queued", status)
	}
	if err := m.leave(ctx, "fn", "other"); err != nil {
		t.Fatal(err)
	}

	// No further refresh: the lease expires and the next admission reclaims it.
	m.now = func() time.Time { return base.Add(3 * time.Second) }
	if _, err := m.Acquire(ctx, "fn", "live", 1); err != nil {
		t.Fatal(err)
	}
	slots, err := m.rdb.ZRange(ctx, m.slotsKey("fn"), 0, -1).Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(slots) != 1 || slots[0] != "live" {
		t.Fatalf("function slots = %#v", slots)
	}
	global, err := m.rdb.ZRange(ctx, m.globalKey(), 0, -1).Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(global) != 1 || global[0] != "live" {
		t.Fatalf("global slots = %#v", global)
	}
}

func TestDeadQueueHeadDoesNotBlock(t *testing.T) {
	m := newTestManager(t)
	m.cfg.MaxConcurrency = 1
	m.cfg.QueueLease = 50 * time.Millisecond
	m.cfg.MaxWait = 2 * time.Second
	ctx := context.Background()

	if _, err := m.Acquire(ctx, "fn", "holder", 1); err != nil {
		t.Fatal(err)
	}
	if err := m.rdb.HSet(ctx, m.execKey("dead"),
		"functionId", "fn",
		"status", "queued",
		"cancelled", "false",
		"heartbeat", "0",
	).Err(); err != nil {
		t.Fatal(err)
	}
	if err := m.rdb.ZAdd(ctx, m.queueKey("fn"), redis.Z{Score: 1, Member: "dead"}).Err(); err != nil {
		t.Fatal(err)
	}

	got := make(chan error, 1)
	go func() {
		_, err := m.Acquire(ctx, "fn", "live", 1)
		got <- err
	}()
	waitStatus(t, m, "live", "queued")

	if err := m.Release(ctx, "fn", "holder"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("live waiter stayed behind the dead head")
	}
	if _, err := m.rdb.ZScore(ctx, m.queueKey("fn"), "dead").Result(); !errors.Is(err, redis.Nil) {
		t.Fatalf("dead head still queued: %v", err)
	}
}

func TestGlobalContainerLimit(t *testing.T) {
	m := newTestManager(t)
	m.cfg.MaxConcurrency = 5
	m.cfg.GlobalMax = 1
	m.cfg.QueueMax = 5
	ctx := context.Background()

	if _, err := m.Acquire(ctx, "fn-a", "a", 0); err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	go func() {
		_, err := m.Acquire(ctx, "fn-b", "b", 0)
		got <- err
	}()
	waitStatus(t, m, "b", "queued")
	select {
	case err := <-got:
		t.Fatalf("global limit did not queue fn-b: %v", err)
	case <-time.After(40 * time.Millisecond):
	}

	if err := m.Release(ctx, "fn-a", "a"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fn-b did not start after the global slot was released")
	}
}

func TestGlobalLimitQueueFull(t *testing.T) {
	m := newTestManager(t)
	m.cfg.MaxConcurrency = 5
	m.cfg.GlobalMax = 1
	m.cfg.QueueMax = 1
	ctx := context.Background()

	if _, err := m.Acquire(ctx, "fn", "a", 0); err != nil {
		t.Fatal(err)
	}
	go func() {
		_, _ = m.Acquire(ctx, "fn", "b", 0)
	}()
	waitStatus(t, m, "b", "queued")

	_, err := m.Acquire(ctx, "fn", "c", 0)
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("err = %v, want ErrQueueFull", err)
	}
}

func TestFunctionsDoNotShareAQueue(t *testing.T) {
	m := newTestManager(t)
	m.cfg.MaxConcurrency = 1
	m.cfg.QueueMax = 1
	m.cfg.GlobalMax = 8
	ctx := context.Background()

	if _, err := m.Acquire(ctx, "fn-a", "a1", 1); err != nil {
		t.Fatal(err)
	}
	go func() {
		_, _ = m.Acquire(ctx, "fn-a", "a2", 1)
	}()
	waitStatus(t, m, "a2", "queued")

	if _, err := m.Acquire(ctx, "fn-a", "a3", 1); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("fn-a err = %v, want ErrQueueFull", err)
	}
	if _, err := m.Acquire(ctx, "fn-b", "b1", 1); err != nil {
		t.Fatalf("fn-b should run while fn-a is full: %v", err)
	}
}

func TestMaxConcurrencyOverride(t *testing.T) {
	m := newTestManager(t)
	m.cfg.MaxConcurrency = 1
	ctx := context.Background()

	if _, err := m.Acquire(ctx, "fn", "a", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Acquire(ctx, "fn", "b", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Acquire(ctx, "fn", "c", 3); err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	go func() {
		_, err := m.Acquire(ctx, "fn", "d", 3)
		got <- err
	}()
	waitStatus(t, m, "d", "queued")
	select {
	case err := <-got:
		t.Fatalf("fourth should wait under override 3: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
}

func TestCancelQueuedUnblocksNext(t *testing.T) {
	m := newTestManager(t)
	m.cfg.MaxConcurrency = 1
	ctx := context.Background()

	if _, err := m.Acquire(ctx, "fn", "holder", 1); err != nil {
		t.Fatal(err)
	}

	errA := make(chan error, 1)
	go func() {
		_, err := m.Acquire(ctx, "fn", "a", 1)
		errA <- err
	}()
	waitStatus(t, m, "a", "queued")

	errB := make(chan error, 1)
	go func() {
		_, err := m.Acquire(ctx, "fn", "b", 1)
		errB <- err
	}()
	waitStatus(t, m, "b", "queued")

	if err := m.MarkForCancellation(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.rdb.ZScore(ctx, m.queueKey("fn"), "a").Result(); !errors.Is(err, redis.Nil) {
		t.Fatalf("cancelled waiter still queued: %v", err)
	}

	if err := m.Release(ctx, "fn", "holder"); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-errA:
		if !errors.Is(err, ErrCancelled) {
			t.Fatalf("a err = %v, want ErrCancelled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled waiter did not return")
	}
	select {
	case err := <-errB:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("next waiter did not start after cancel")
	}
	if m.IsCancelled(ctx, "b") {
		t.Fatal("the execution that ran should not be cancelled")
	}
}

func TestCancelRunningKeepsSlot(t *testing.T) {
	m := newTestManager(t)
	m.cfg.MaxConcurrency = 1
	ctx := context.Background()

	if _, err := m.Acquire(ctx, "fn", "running", 1); err != nil {
		t.Fatal(err)
	}
	if err := m.MarkForCancellation(ctx, "running"); err != nil {
		t.Fatal(err)
	}
	if !m.IsCancelled(ctx, "running") {
		t.Fatal("expected cancelled flag")
	}
	if _, err := m.rdb.ZScore(ctx, m.slotsKey("fn"), "running").Result(); err != nil {
		t.Fatalf("running slot should stay until release: %v", err)
	}

	got := make(chan error, 1)
	go func() {
		_, err := m.Acquire(ctx, "fn", "next", 1)
		got <- err
	}()
	waitStatus(t, m, "next", "queued")
	select {
	case err := <-got:
		t.Fatalf("cancel of a running execution freed its slot: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
}

func TestCancelUnknown(t *testing.T) {
	m := newTestManager(t)
	if err := m.MarkForCancellation(context.Background(), "missing"); err == nil {
		t.Fatal("expected error")
	}
}

func TestCallerCancelLeavesTheQueue(t *testing.T) {
	m := newTestManager(t)
	m.cfg.MaxConcurrency = 1
	ctx := context.Background()
	if _, err := m.Acquire(ctx, "fn", "holder", 1); err != nil {
		t.Fatal(err)
	}

	waitCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := m.Acquire(waitCtx, "fn", "waiter", 1)
		done <- err
	}()
	waitStatus(t, m, "waiter", "queued")
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("acquire did not observe context cancel")
	}
	n, err := m.rdb.ZCard(ctx, m.queueKey("fn")).Result()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("queue len = %d", n)
	}
}

func TestReleaseDoesNotDropAnotherSlot(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	if _, err := m.Acquire(ctx, "fn", "a", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Acquire(ctx, "fn", "b", 2); err != nil {
		t.Fatal(err)
	}
	if err := m.Release(ctx, "fn", "other"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if _, err := m.rdb.ZScore(ctx, m.slotsKey("fn"), id).Result(); err != nil {
			t.Fatalf("slot %s missing after unrelated release: %v", id, err)
		}
	}
	if err := m.Release(ctx, "fn", "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.rdb.ZScore(ctx, m.slotsKey("fn"), "a").Result(); !errors.Is(err, redis.Nil) {
		t.Fatalf("a still held: %v", err)
	}
	if _, err := m.rdb.ZScore(ctx, m.slotsKey("fn"), "b").Result(); err != nil {
		t.Fatalf("b missing: %v", err)
	}
}

func TestSweepStaleSlotsDropsDeadHostsOnly(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	now := m.now().UnixMilli()

	if err := m.rdb.ZAdd(ctx, "nimbus:slots:machine:dead-host", redis.Z{Score: float64(now - 1000), Member: "ghost"}).Err(); err != nil {
		t.Fatal(err)
	}
	if err := m.rdb.ZAdd(ctx, "nimbus:slots:machine:live-peer", redis.Z{Score: float64(now + 60_000), Member: "alive"}).Err(); err != nil {
		t.Fatal(err)
	}
	if err := m.rdb.ZAdd(ctx, m.globalKey(), redis.Z{Score: float64(now - 50), Member: "old"}, redis.Z{Score: float64(now + 60_000), Member: "current"}).Err(); err != nil {
		t.Fatal(err)
	}
	if err := m.rdb.ZAdd(ctx, "nimbus:slots:fn:old-fn", redis.Z{Score: float64(now - 10), Member: "gone"}).Err(); err != nil {
		t.Fatal(err)
	}
	if err := m.rdb.ZAdd(ctx, "nimbus:queue:fn:old-fn", redis.Z{Score: 1, Member: "waiter"}).Err(); err != nil {
		t.Fatal(err)
	}

	n, err := m.SweepStaleSlots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("deleted keys = %d, want 2 (dead host and empty function set)", n)
	}
	if exists, _ := m.rdb.Exists(ctx, "nimbus:slots:machine:dead-host").Result(); exists != 0 {
		t.Fatal("dead host key still present")
	}
	if exists, _ := m.rdb.Exists(ctx, "nimbus:slots:fn:old-fn").Result(); exists != 0 {
		t.Fatal("empty function slot key still present")
	}
	score, err := m.rdb.ZScore(ctx, "nimbus:slots:machine:live-peer", "alive").Result()
	if err != nil || score < float64(now) {
		t.Fatalf("live peer slot = %v err=%v", score, err)
	}
	if _, err := m.rdb.ZScore(ctx, m.globalKey(), "old").Result(); !errors.Is(err, redis.Nil) {
		t.Fatalf("expired member on the current host was kept: %v", err)
	}
	if _, err := m.rdb.ZScore(ctx, m.globalKey(), "current").Result(); err != nil {
		t.Fatalf("live member on the current host was dropped: %v", err)
	}
	if n, err := m.rdb.ZCard(ctx, "nimbus:queue:fn:old-fn").Result(); err != nil || n != 1 {
		t.Fatalf("queue was swept: n=%d err=%v", n, err)
	}
}

func TestKeyFormat(t *testing.T) {
	m := newTestManager(t)
	if got := m.slotsKey("abc"); got != "nimbus:slots:fn:abc" {
		t.Fatalf("slots key %q", got)
	}
	if got := m.queueKey("abc"); got != "nimbus:queue:fn:abc" {
		t.Fatalf("queue key %q", got)
	}
	if got := m.globalKey(); got != "nimbus:slots:machine:test-machine" {
		t.Fatalf("global key %q", got)
	}
	if got := m.execKey("abc"); got != "nimbus:exec:abc" {
		t.Fatalf("exec key %q", got)
	}
}

func TestLoadConfig(t *testing.T) {
	t.Setenv("NIMBUS_FN_CONCURRENCY", "4")
	t.Setenv("NIMBUS_FN_QUEUE_MAX", "0")
	t.Setenv("NIMBUS_FN_QUEUE_WAIT_SEC", "9")
	t.Setenv("NIMBUS_MAX_CONTAINERS", "3")
	t.Setenv("NIMBUS_SLOT_LEASE_SEC", "12")

	cfg := LoadConfig()
	if cfg.MaxConcurrency != 4 || cfg.QueueMax != 0 || cfg.GlobalMax != 3 {
		t.Fatalf("counts %+v", cfg)
	}
	if cfg.MaxWait != 9*time.Second || cfg.Lease != 12*time.Second {
		t.Fatalf("durations %+v", cfg)
	}

	t.Setenv("NIMBUS_FN_CONCURRENCY", "nope")
	if got := LoadConfig().MaxConcurrency; got != 2 {
		t.Fatalf("invalid concurrency = %d", got)
	}
	t.Setenv("NIMBUS_FN_CONCURRENCY", "0")
	if got := LoadConfig().MaxConcurrency; got != 2 {
		t.Fatalf("zero concurrency = %d", got)
	}
}
