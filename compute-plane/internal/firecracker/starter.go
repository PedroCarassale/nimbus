package firecracker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/PedroCarassale/nimbus/compute-plane/internal/isolate"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/warm"
)

// Starter boots a microVM per warm.Spec. It implements isolate's backend
// and warm.Starter, so the pool does not know which sandbox it is holding.
type Starter struct {
	cfg Config
}

func NewFromEnv() Starter {
	return Starter{cfg: LoadConfig()}
}

func (s Starter) Name() string { return isolate.Firecracker }

func (s Starter) Start(ctx context.Context, spec warm.Spec) (warm.Session, error) {
	started := time.Now()
	vm, err := boot(ctx, s.cfg, spec)
	if err != nil {
		return nil, err
	}
	sess := newSession(vm.ID, vm.conn, vm.Kill)
	sess.notes = vm.LogTail
	sess.readyFrom = started
	if err := sess.waitReady(ctx); err != nil {
		extra := vm.LogTail()
		_ = sess.Destroy(context.Background())
		if extra != "" {
			err = fmt.Errorf("%w: %s", err, extra)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	return sess, nil
}

// CleanupLeaked kills microVMs whose pid files survived a process crash.
func CleanupLeaked() {
	cfg := LoadConfig()
	entries, err := os.ReadDir(cfg.runDir())
	if err != nil {
		return
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".pid" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(cfg.runDir(), entry.Name()))
		if err != nil {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil || pid <= 1 {
			continue
		}
		proc, err := os.FindProcess(pid)
		if err == nil {
			_ = proc.Signal(os.Interrupt)
			_ = proc.Kill()
		}
	}
	_ = os.RemoveAll(cfg.runDir())
}
