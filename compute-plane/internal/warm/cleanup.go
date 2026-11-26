package warm

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// WarmLabel selects containers this pool owns.
const WarmLabel = "nimbus.warm=1"

// CleanupOrphans removes warm containers left behind by a dead process.
// They are labeled at start so a new process can find them without tracking
// names. workerID limits the sweep to this worker's label plus containers
// that have no nimbus.worker label (a previous process). An empty workerID
// removes every warm container. A peer's label is left alone, so two workers
// sharing one Docker daemon do not delete each other's pool on startup.
// A docker error is returned to the caller; missing docker is not fatal
// for unit tests because they do not call this.
func CleanupOrphans(ctx context.Context, workerID string) error {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "docker", "ps", "-aq", "--filter", "label="+WarmLabel).Output()
	if err != nil {
		return err
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return nil
	}
	victims := make([]string, 0, len(ids))
	for _, id := range ids {
		if workerID == "" || orphanBelongs(ctx, id, workerID) {
			victims = append(victims, id)
		}
	}
	if len(victims) == 0 {
		return nil
	}
	args := append([]string{"rm", "-f"}, victims...)
	rmCtx, rmCancel := context.WithTimeout(ctx, 30*time.Second)
	defer rmCancel()
	return exec.CommandContext(rmCtx, "docker", args...).Run()
}

func orphanBelongs(ctx context.Context, containerID, workerID string) bool {
	ictx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ictx, "docker", "inspect", "-f", `{{index .Config.Labels "nimbus.worker"}}`, containerID).Output()
	if err != nil {
		return false
	}
	label := strings.TrimSpace(string(out))
	return label == "" || label == workerID
}
