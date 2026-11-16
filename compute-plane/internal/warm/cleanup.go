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
// names. A docker error is returned to the caller; missing docker is not fatal
// for unit tests because they do not call this.
func CleanupOrphans(ctx context.Context) error {
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
	args := append([]string{"rm", "-f"}, ids...)
	rmCtx, rmCancel := context.WithTimeout(ctx, 30*time.Second)
	defer rmCancel()
	return exec.CommandContext(rmCtx, "docker", args...).Run()
}
