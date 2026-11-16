package async

import (
	"testing"
	"time"
)

func TestBackoffDoublesAndCaps(t *testing.T) {
	base := 100 * time.Millisecond
	ceiling := 350 * time.Millisecond
	jitter := 40 * time.Millisecond

	if got := Backoff(1, base, jitter, ceiling, 0); got != base {
		t.Fatalf("attempt 1 = %s", got)
	}
	if got := Backoff(2, base, jitter, ceiling, 0); got != 200*time.Millisecond {
		t.Fatalf("attempt 2 = %s", got)
	}
	if got := Backoff(3, base, jitter, ceiling, 0); got != ceiling {
		t.Fatalf("attempt 3 = %s, want cap", got)
	}
	if got := Backoff(8, base, jitter, ceiling, 0); got != ceiling {
		t.Fatalf("attempt 8 = %s, want cap", got)
	}

	withJitter := Backoff(1, base, jitter, ceiling, 0.5)
	if withJitter != base+20*time.Millisecond {
		t.Fatalf("jitter = %s", withJitter)
	}
	full := Backoff(1, base, jitter, ceiling, 2)
	if full != base+jitter {
		t.Fatalf("jitter clamped = %s", full)
	}
	if got := Backoff(0, base, 0, ceiling, 0); got != base {
		t.Fatalf("attempt 0 = %s", got)
	}
}
