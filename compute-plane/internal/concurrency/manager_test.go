package concurrency

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestManager(t *testing.T) (*Manager, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	m := NewManager(rdb)
	m.maxWait = 200 * time.Millisecond
	m.pollDelay = 20 * time.Millisecond
	m.lockTTL = time.Minute
	return m, mr
}

func TestAcquireAndRelease(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := context.Background()

	ok, err := m.AcquireLock(ctx, "fn-1", "exec-1")
	if err != nil || !ok {
		t.Fatalf("acquire = %v %v", ok, err)
	}

	ok, err = m.AcquireLock(ctx, "fn-1", "exec-2")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("second acquire should fail while the lock is held")
	}

	if err := m.ReleaseLock(ctx, "fn-1", "exec-other"); err != nil {
		t.Fatal(err)
	}
	held, err := m.rdb.Get(ctx, m.lockKey("fn-1")).Result()
	if err != nil {
		t.Fatal(err)
	}
	if held != "exec-1" {
		t.Fatalf("lock owner = %q", held)
	}

	if err := m.ReleaseLock(ctx, "fn-1", "exec-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.rdb.Get(ctx, m.lockKey("fn-1")).Result(); err != redis.Nil {
		t.Fatalf("lock still present: %v", err)
	}

	ok, err = m.AcquireLock(ctx, "fn-1", "exec-3")
	if err != nil || !ok {
		t.Fatalf("reacquire = %v %v", ok, err)
	}
}

func TestReleaseMissingLock(t *testing.T) {
	m, _ := newTestManager(t)
	if err := m.ReleaseLock(context.Background(), "missing", "exec"); err != nil {
		t.Fatal(err)
	}
}

func TestMarkForCancellation(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := context.Background()

	if err := m.MarkForCancellation(ctx, "nope"); err == nil {
		t.Fatal("expected error for unknown execution")
	}

	ok, err := m.AcquireLock(ctx, "fn-1", "exec-1")
	if err != nil || !ok {
		t.Fatal(err)
	}
	if m.IsCancelled(ctx, "exec-1") {
		t.Fatal("should not be cancelled yet")
	}
	if err := m.MarkForCancellation(ctx, "exec-1"); err != nil {
		t.Fatal(err)
	}
	if !m.IsCancelled(ctx, "exec-1") {
		t.Fatal("expected cancelled")
	}
}

func TestAcquireLockContextCancel(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := context.Background()
	ok, err := m.AcquireLock(ctx, "fn-1", "exec-1")
	if err != nil || !ok {
		t.Fatal(err)
	}

	short, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	ok, err = m.AcquireLock(short, "fn-1", "exec-2")
	if ok {
		t.Fatal("acquire should not succeed")
	}
	if err == nil {
		t.Fatal("expected context error")
	}
}

func TestLockKeyFormat(t *testing.T) {
	m, _ := newTestManager(t)
	if got := m.lockKey("abc"); got != "nimbus:lock:fn:abc" {
		t.Fatalf("lock key %q", got)
	}
	if got := m.execKey("abc"); got != "nimbus:exec:abc" {
		t.Fatalf("exec key %q", got)
	}
}
