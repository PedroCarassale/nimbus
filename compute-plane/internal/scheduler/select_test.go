package scheduler

import "testing"

func worker(id string, active, max int, state string, keys ...string) Worker {
	return Worker{
		ID:           id,
		URL:          "http://" + id + ":8080",
		Active:       active,
		Max:          max,
		IdleWarmKeys: keys,
		State:        state,
		Live:         true,
	}
}

func TestSelectWarmAffinity(t *testing.T) {
	key := "fn:v1:nodejs20:abc"
	busy := worker("a", 0, 8, "healthy")
	warm := worker("b", 5, 8, "healthy", key)
	got, reason, ok := Select([]Worker{busy, warm}, key, nil, nil)
	if !ok || got.ID != "b" || reason != "warm" {
		t.Fatalf("got %s reason %s ok %v", got.ID, reason, ok)
	}
}

func TestSelectWarmSkipsFullWorker(t *testing.T) {
	key := "fn:v1:nodejs20:abc"
	full := worker("warm", 8, 8, "healthy", key)
	other := worker("other", 3, 8, "healthy")
	got, reason, ok := Select([]Worker{full, other}, key, nil, nil)
	if !ok || got.ID != "other" || reason != "least_loaded" {
		t.Fatalf("got %s reason %s ok %v", got.ID, reason, ok)
	}
}

func TestSelectLeastLoaded(t *testing.T) {
	// 1/2 = 0.5 is busier than 2/8 = 0.25.
	a := worker("a", 1, 2, "healthy")
	b := worker("b", 2, 8, "healthy")
	got, reason, ok := Select([]Worker{a, b}, "", nil, nil)
	if !ok || got.ID != "b" || reason != "least_loaded" {
		t.Fatalf("got %s reason %s ok %v", got.ID, reason, ok)
	}
}

func TestSelectTieBreakFewestActive(t *testing.T) {
	// Same utilization 0.25. Fewer active wins, even if its id sorts later.
	a := worker("b", 1, 4, "healthy")
	c := worker("a", 2, 8, "healthy")
	got, _, ok := Select([]Worker{c, a}, "", nil, nil)
	if !ok || got.ID != "b" {
		t.Fatalf("got %s", got.ID)
	}
}

func TestSelectFiltersDrainingDeadAndFull(t *testing.T) {
	draining := worker("d", 0, 8, "draining")
	dead := worker("x", 0, 8, "healthy")
	dead.Live = false
	full := worker("f", 8, 8, "healthy")
	okOne := worker("ok", 1, 8, "healthy")
	got, reason, ok := Select([]Worker{draining, dead, full, okOne}, "missing", nil, nil)
	if !ok || got.ID != "ok" || reason != "least_loaded" {
		t.Fatalf("got %s reason %s ok %v", got.ID, reason, ok)
	}
	if _, _, ok := Select([]Worker{draining, dead, full}, "", nil, nil); ok {
		t.Fatal("expected no eligible worker")
	}
}

func TestSelectInflightSpreadsParallelChoices(t *testing.T) {
	p := NewPicker()
	workers := []Worker{
		worker("a", 0, 8, "healthy"),
		worker("b", 0, 8, "healthy"),
	}
	first, _, ok := p.Choose(workers, "", nil)
	if !ok {
		t.Fatal("first")
	}
	second, _, ok := p.Choose(workers, "", nil)
	if !ok || second.ID == first.ID {
		t.Fatalf("second %s first %s", second.ID, first.ID)
	}
	p.Done(first.ID)
	p.Done(second.ID)
	third, _, ok := p.Choose(workers, "", nil)
	if !ok || third.ID != "a" {
		t.Fatalf("after release, tie breaks to a, got %s", third.ID)
	}
}

func TestSelectSkipUsedByFailover(t *testing.T) {
	workers := []Worker{
		worker("dead", 0, 8, "healthy"),
		worker("live", 1, 8, "healthy"),
	}
	got, reason, ok := Select(workers, "", nil, map[string]bool{"dead": true})
	if !ok || got.ID != "live" || reason != "least_loaded" {
		t.Fatalf("got %s reason %s", got.ID, reason)
	}
}
