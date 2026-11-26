// Package registry stores compute workers in Redis so a scheduler can route
// without a Nest change. Each worker refreshes nimbus:worker:{id} before the
// TTL, and nimbus:exec:{executionId} records which worker accepted the run.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// StateHealthy is a worker that can take new executions.
	StateHealthy = "healthy"
	// StateDraining is a worker that is shutting down. It stays visible so
	// cancel can still find it, and the scheduler does not send new work.
	StateDraining = "draining"

	workersSet   = "nimbus:workers"
	workerPrefix = "nimbus:worker:"
	execPrefix   = "nimbus:exec:"

	// DefaultInterval is how often a worker refreshes its registration.
	DefaultInterval = 2 * time.Second
	// DefaultTTL is how long a registration lives without a refresh.
	// A SIGKILL leaves the key until this expires; the scheduler then drops it.
	DefaultTTL = 6 * time.Second
)

// Snapshot is one heartbeat. IdleWarmKeys use
// functionId:version:runtime:configHash.
type Snapshot struct {
	WorkerID         string   `json:"workerId"`
	URL              string   `json:"url"`
	ActiveExecutions int      `json:"activeExecutions"`
	MaxContainers    int      `json:"maxContainers"`
	IdleWarmKeys     []string `json:"idleWarmKeys"`
	State            string   `json:"state"`
}

// Registry reads and writes worker heartbeats.
type Registry struct {
	rdb *redis.Client
	ttl time.Duration
}

func New(rdb *redis.Client, ttl time.Duration) *Registry {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &Registry{rdb: rdb, ttl: ttl}
}

// WorkerID is NIMBUS_WORKER_ID, or the hostname when that variable is empty.
func WorkerID() string {
	if id := strings.TrimSpace(os.Getenv("NIMBUS_WORKER_ID")); id != "" {
		return id
	}
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		return "local"
	}
	return host
}

// WorkerURL is NIMBUS_WORKER_URL, or http://{workerId}:{port} when unset.
func WorkerURL(port string) string {
	if raw := strings.TrimSpace(os.Getenv("NIMBUS_WORKER_URL")); raw != "" {
		return strings.TrimRight(raw, "/")
	}
	if port == "" {
		port = "8080"
	}
	return "http://" + WorkerID() + ":" + port
}

// Heartbeat writes the snapshot and refreshes the TTL. An empty state is healthy.
func (r *Registry) Heartbeat(ctx context.Context, snap Snapshot) error {
	if r == nil || r.rdb == nil {
		return errors.New("registry sin redis")
	}
	snap.WorkerID = strings.TrimSpace(snap.WorkerID)
	if snap.WorkerID == "" {
		return errors.New("workerId vacío")
	}
	if snap.State == "" {
		snap.State = StateHealthy
	}
	if snap.IdleWarmKeys == nil {
		snap.IdleWarmKeys = []string{}
	}
	sort.Strings(snap.IdleWarmKeys)
	raw, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	key := workerPrefix + snap.WorkerID
	pipe := r.rdb.TxPipeline()
	pipe.Set(ctx, key, raw, r.ttl)
	pipe.SAdd(ctx, workersSet, snap.WorkerID)
	_, err = pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("heartbeat %s: %w", snap.WorkerID, err)
	}
	return nil
}

// Remove drops the registration. A graceful shutdown calls this after it has
// published state=draining.
func (r *Registry) Remove(ctx context.Context, workerID string) error {
	if r == nil || r.rdb == nil {
		return errors.New("registry sin redis")
	}
	workerID = strings.TrimSpace(workerID)
	if workerID == "" {
		return nil
	}
	pipe := r.rdb.TxPipeline()
	pipe.Del(ctx, workerPrefix+workerID)
	pipe.SRem(ctx, workersSet, workerID)
	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("remove worker %s: %w", workerID, err)
	}
	return nil
}

// List returns workers whose heartbeat key is still alive. Members left in
// the set after a TTL expiry are removed as they are seen.
func (r *Registry) List(ctx context.Context) ([]Snapshot, error) {
	if r == nil || r.rdb == nil {
		return nil, errors.New("registry sin redis")
	}
	ids, err := r.rdb.SMembers(ctx, workersSet).Result()
	if err != nil {
		return nil, err
	}
	out := make([]Snapshot, 0, len(ids))
	for _, id := range ids {
		raw, err := r.rdb.Get(ctx, workerPrefix+id).Bytes()
		if err == redis.Nil {
			_ = r.rdb.SRem(ctx, workersSet, id).Err()
			continue
		}
		if err != nil {
			return nil, err
		}
		var snap Snapshot
		if err := json.Unmarshal(raw, &snap); err != nil {
			_ = r.rdb.SRem(ctx, workersSet, id).Err()
			_ = r.rdb.Del(ctx, workerPrefix+id).Err()
			continue
		}
		if snap.WorkerID == "" {
			snap.WorkerID = id
		}
		if snap.IdleWarmKeys == nil {
			snap.IdleWarmKeys = []string{}
		}
		out = append(out, snap)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].WorkerID < out[j].WorkerID })
	return out, nil
}

// ExecutionWorker reads workerId from nimbus:exec:{executionId}.
// A missing hash or field returns an empty id and a nil error.
func (r *Registry) ExecutionWorker(ctx context.Context, executionID string) (string, error) {
	if r == nil || r.rdb == nil {
		return "", errors.New("registry sin redis")
	}
	executionID = strings.TrimSpace(executionID)
	if executionID == "" {
		return "", nil
	}
	got, err := r.rdb.HGet(ctx, execPrefix+executionID, "workerId").Result()
	if err == redis.Nil {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return got, nil
}
