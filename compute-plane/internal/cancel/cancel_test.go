package cancel

import (
	"context"
	"testing"
	"time"
)

func TestCauseReachesTimeoutChild(t *testing.T) {
	parent, stop := context.WithCancelCause(context.Background())
	child, cancelChild := context.WithTimeout(parent, time.Hour)
	defer cancelChild()

	stop(ErrRun)
	if !Is(child) {
		t.Fatalf("cause = %v", context.Cause(child))
	}
	if child.Err() == context.DeadlineExceeded {
		t.Fatal("user cancel must not look like a timeout")
	}
}

func TestDeadlineIsNotUserCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	<-ctx.Done()
	if Is(ctx) {
		t.Fatal("deadline was reported as a user cancel")
	}
}
