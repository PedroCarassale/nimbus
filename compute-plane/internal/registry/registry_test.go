package registry

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func testReg(t *testing.T) (*Registry, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return New(rdb, 6*time.Second), mr
}

func snap(id string, active int, state string, keys ...string) Snapshot {
	return Snapshot{
		WorkerID:         id,
		URL:              "http://" + id + ":8080",
		ActiveExecutions: active,
		MaxContainers:    8,
		IdleWarmKeys:     keys,
		State:            state,
	}
}

func TestHeartbeatListAndRemove(t *testing.T) {
	reg, _ := testReg(t)
	ctx := context.Background()
	if err := reg.Heartbeat(ctx, snap("b", 1, StateHealthy, "fn:v1:nodejs20:abc")); err != nil {
		t.Fatal(err)
	}
	if err := reg.Heartbeat(ctx, snap("a", 0, StateDraining)); err != nil {
		t.Fatal(err)
	}
	got, err := reg.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].WorkerID != "a" || got[1].WorkerID != "b" {
		t.Fatalf("list = %+v", got)
	}
	if got[0].State != StateDraining || got[1].ActiveExecutions != 1 {
		t.Fatalf("fields = %+v", got)
	}
	if len(got[1].IdleWarmKeys) != 1 || got[1].IdleWarmKeys[0] != "fn:v1:nodejs20:abc" {
		t.Fatalf("idle keys = %v", got[1].IdleWarmKeys)
	}
	if got[0].IdleWarmKeys == nil {
		t.Fatal("idle keys should be an empty slice, not null")
	}
	if err := reg.Remove(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	got, err = reg.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].WorkerID != "a" {
		t.Fatalf("after remove = %+v", got)
	}
}

func TestHeartbeatTTLExpiryDropsWorker(t *testing.T) {
	reg, mr := testReg(t)
	ctx := context.Background()
	if err := reg.Heartbeat(ctx, snap("w", 0, StateHealthy)); err != nil {
		t.Fatal(err)
	}
	mr.FastForward(4 * time.Second)
	if err := reg.Heartbeat(ctx, snap("w", 2, StateHealthy)); err != nil {
		t.Fatal(err)
	}
	mr.FastForward(4 * time.Second)
	got, err := reg.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ActiveExecutions != 2 {
		t.Fatalf("refresh should keep the worker: %+v", got)
	}
	mr.FastForward(7 * time.Second)
	got, err = reg.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expired worker still listed: %+v", got)
	}
	members, err := reg.rdb.SMembers(ctx, workersSet).Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 0 {
		t.Fatalf("stale set member left behind: %v", members)
	}
}

func TestExecutionWorker(t *testing.T) {
	reg, _ := testReg(t)
	ctx := context.Background()
	if err := reg.rdb.HSet(ctx, execPrefix+"exec-1", "workerId", "compute-worker-1", "status", "running").Err(); err != nil {
		t.Fatal(err)
	}
	got, err := reg.ExecutionWorker(ctx, "exec-1")
	if err != nil || got != "compute-worker-1" {
		t.Fatalf("worker = %q err=%v", got, err)
	}
	got, err = reg.ExecutionWorker(ctx, "missing")
	if err != nil || got != "" {
		t.Fatalf("missing = %q err=%v", got, err)
	}
}

func TestBeaterDrainThenStop(t *testing.T) {
	reg, _ := testReg(t)
	ctx := context.Background()
	b := Start(reg, func() Snapshot {
		return Snapshot{ActiveExecutions: 1, MaxContainers: 4}
	}, 50*time.Millisecond, "w1", "http://w1:8080")
	got, err := reg.List(ctx)
	if err != nil || len(got) != 1 || got[0].State != StateHealthy || got[0].URL != "http://w1:8080" {
		t.Fatalf("first beat = %+v err=%v", got, err)
	}
	b.Drain()
	got, err = reg.List(ctx)
	if err != nil || len(got) != 1 || got[0].State != StateDraining {
		t.Fatalf("draining = %+v err=%v", got, err)
	}
	b.Stop()
	got, err = reg.List(ctx)
	if err != nil || len(got) != 0 {
		t.Fatalf("after stop = %+v err=%v", got, err)
	}
}
