package logs

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func testRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	return redis.NewClient(&redis.Options{Addr: mr.Addr()}), mr
}

func TestStreamWriterRoundTrip(t *testing.T) {
	rdb, mr := testRedis(t)
	ctx := context.Background()
	sw := NewStreamWriter(rdb, "exec-1")

	if sw.StreamKey() != "nimbus:logs:exec-1" {
		t.Fatalf("key %q", sw.StreamKey())
	}

	if err := sw.WriteStdout(ctx, "hola"); err != nil {
		t.Fatal(err)
	}
	if err := sw.WriteStderr(ctx, "aviso"); err != nil {
		t.Fatal(err)
	}
	if err := sw.Close(ctx); err != nil {
		t.Fatal(err)
	}

	ttl := mr.TTL(sw.StreamKey())
	if ttl <= 0 || ttl > StreamTTL {
		t.Fatalf("ttl = %s", ttl)
	}

	entries, err := NewStreamReader(rdb, "exec-1").ReadAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("len = %d %#v", len(entries), entries)
	}
	if entries[0].Stream != "stdout" || entries[0].Line != "hola" || entries[0].Sequence != 1 {
		t.Fatalf("stdout %#v", entries[0])
	}
	if entries[1].Stream != "stderr" || entries[1].Line != "aviso" || entries[1].Sequence != 2 {
		t.Fatalf("stderr %#v", entries[1])
	}
	if entries[2].Stream != "_end" || entries[2].Line != "execution_complete" || entries[2].Sequence != 3 {
		t.Fatalf("end %#v", entries[2])
	}
}

func TestStreamWriterConcurrentSequences(t *testing.T) {
	rdb, _ := testRedis(t)
	ctx := context.Background()
	sw := NewStreamWriter(rdb, "exec-race")

	const n = 40
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if err := sw.WriteStdout(ctx, "line"); err != nil {
				t.Errorf("write: %v", err)
			}
		}()
	}
	wg.Wait()
	if err := sw.Close(ctx); err != nil {
		t.Fatal(err)
	}

	entries, err := NewStreamReader(rdb, "exec-race").ReadAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != n+1 {
		t.Fatalf("len = %d", len(entries))
	}
	seen := map[int]bool{}
	for _, entry := range entries {
		if seen[entry.Sequence] {
			t.Fatalf("duplicate sequence %d", entry.Sequence)
		}
		seen[entry.Sequence] = true
	}
	if entries[len(entries)-1].Stream != "_end" {
		t.Fatalf("last = %#v", entries[len(entries)-1])
	}
}

func TestStreamReaderSubscribe(t *testing.T) {
	rdb, _ := testRedis(t)
	ctx := context.Background()
	sw := NewStreamWriter(rdb, "exec-sub")
	if err := sw.WriteStdout(ctx, "uno"); err != nil {
		t.Fatal(err)
	}
	if err := sw.Close(ctx); err != nil {
		t.Fatal(err)
	}

	ch, err := NewStreamReader(rdb, "exec-sub").Subscribe(ctx, "0")
	if err != nil {
		t.Fatal(err)
	}

	var got []LogEntry
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case entry, ok := <-ch:
			if !ok {
				if len(got) != 2 {
					t.Fatalf("got %#v", got)
				}
				if got[0].Line != "uno" || got[1].Stream != "_end" {
					t.Fatalf("got %#v", got)
				}
				return
			}
			got = append(got, entry)
		case <-timer.C:
			t.Fatalf("subscribe timed out, got %#v", got)
		}
	}
}

func TestStreamExistsAndEmpty(t *testing.T) {
	rdb, _ := testRedis(t)
	ctx := context.Background()
	reader := NewStreamReader(rdb, "missing")

	ok, err := reader.Exists(ctx)
	if err != nil || ok {
		t.Fatalf("exists = %v %v", ok, err)
	}
	entries, err := reader.ReadAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("entries = %#v", entries)
	}

	sw := NewStreamWriter(rdb, "missing")
	if err := sw.WriteStdout(ctx, "x"); err != nil {
		t.Fatal(err)
	}
	ok, err = reader.Exists(ctx)
	if err != nil || !ok {
		t.Fatalf("exists after write = %v %v", ok, err)
	}
}

func TestStreamWriterRedisDown(t *testing.T) {
	rdb, mr := testRedis(t)
	mr.Close()
	err := NewStreamWriter(rdb, "exec").WriteStdout(context.Background(), "x")
	if err == nil {
		t.Fatal("expected redis error")
	}
}
