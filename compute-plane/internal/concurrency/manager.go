package concurrency

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrQueueFull is returned when the function queue cannot take another waiter.
var ErrQueueFull = errors.New("queue_full")

// ErrWaitTimeout is returned when an invocation waits longer than Config.MaxWait.
var ErrWaitTimeout = errors.New("queue_timeout")

// ErrCancelled is returned when the execution is cancelled before it starts.
var ErrCancelled = errors.New("cancelled")

// Manager admits executions with a Redis counting semaphore and a FIFO queue.
//
// Keys (all per compute process, except the per-function ones):
//
//	nimbus:slots:fn:{functionId}            ZSET  member=executionId  score=lease expiry (unix ms)
//	nimbus:queue:fn:{functionId}            ZSET  member=executionId  score=FIFO ticket
//	nimbus:queue:seq:fn:{functionId}        INCR  ticket source
//	nimbus:slots:machine:{machineId}        ZSET  member=executionId  score=lease expiry (unix ms)
//	nimbus:exec:{executionId}               HASH  functionId, status, heartbeat, cancelled
//
// Granted slots carry a lease. The holder refreshes it until Release. If the
// process dies, the next admission drops scores that are already due, so the
// slot and the global container count recover without a human.
//
// Queued members are not leases: a live waiter refreshes `heartbeat` on every
// poll. A waiter that stops heartbeating for QueueLease is removed, so a dead
// head cannot block the function forever.
type Manager struct {
	rdb       *redis.Client
	cfg       Config
	machineID string
	now       func() time.Time

	mu    sync.Mutex
	stops map[string]context.CancelFunc
}

func NewManager(rdb *redis.Client, cfg Config) *Manager {
	if cfg.MaxConcurrency < 1 {
		cfg.MaxConcurrency = 2
	}
	if cfg.QueueMax < 0 {
		cfg.QueueMax = 0
	}
	if cfg.GlobalMax < 1 {
		cfg.GlobalMax = 8
	}
	if cfg.MaxWait <= 0 {
		cfg.MaxWait = 30 * time.Second
	}
	if cfg.Lease <= 0 {
		cfg.Lease = 30 * time.Second
	}
	if cfg.QueueLease <= 0 {
		cfg.QueueLease = cfg.MaxWait
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 50 * time.Millisecond
	}
	return &Manager{
		rdb:       rdb,
		cfg:       cfg,
		machineID: machineID(),
		now:       time.Now,
		stops:     make(map[string]context.CancelFunc),
	}
}

func machineID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "local"
	}
	return host
}

func (m *Manager) slotsKey(functionID string) string {
	return "nimbus:slots:fn:" + functionID
}

func (m *Manager) queueKey(functionID string) string {
	return "nimbus:queue:fn:" + functionID
}

func (m *Manager) seqKey(functionID string) string {
	return "nimbus:queue:seq:fn:" + functionID
}

func (m *Manager) globalKey() string {
	return "nimbus:slots:machine:" + m.machineID
}

func (m *Manager) execKey(executionID string) string {
	return "nimbus:exec:" + executionID
}

// Acquire blocks until this execution holds a slot, the queue rejects it,
// the wait budget runs out, the execution is cancelled, or ctx is done.
// maxConcurrency <= 0 uses the configured default. A positive value is the
// cap checked for this attempt only.
// The returned queuedMs is zero when the slot was granted on the first try.
func (m *Manager) Acquire(ctx context.Context, functionID, executionID string, maxConcurrency int) (int64, error) {
	limit := maxConcurrency
	if limit <= 0 {
		limit = m.cfg.MaxConcurrency
	}

	var queuedAt time.Time
	deadline := m.now().Add(m.cfg.MaxWait)

	for {
		if err := ctx.Err(); err != nil {
			_ = m.leave(context.Background(), functionID, executionID)
			return m.queuedFor(queuedAt), err
		}

		status, err := m.admit(ctx, functionID, executionID, limit)
		if err != nil {
			_ = m.leave(context.Background(), functionID, executionID)
			return m.queuedFor(queuedAt), err
		}

		switch status {
		case "acquired":
			if m.isCancelled(ctx, executionID) {
				_ = m.Release(context.Background(), functionID, executionID)
				log.Printf("[concurrency] cancelled before start fn=%s exec=%s", functionID, executionID)
				return m.queuedFor(queuedAt), ErrCancelled
			}
			queuedMs := m.queuedFor(queuedAt)
			m.startKeepAlive(functionID, executionID)
			log.Printf("[concurrency] acquired slot fn=%s exec=%s queued_ms=%d limit=%d", functionID, executionID, queuedMs, limit)
			return queuedMs, nil
		case "rejected":
			log.Printf("[concurrency] queue full fn=%s exec=%s", functionID, executionID)
			return m.queuedFor(queuedAt), ErrQueueFull
		case "cancelled":
			log.Printf("[concurrency] cancelled while waiting fn=%s exec=%s", functionID, executionID)
			return m.queuedFor(queuedAt), ErrCancelled
		case "queued":
			if queuedAt.IsZero() {
				queuedAt = m.now()
				log.Printf("[concurrency] queued fn=%s exec=%s", functionID, executionID)
			}
			if !m.now().Before(deadline) {
				_ = m.leave(context.Background(), functionID, executionID)
				queuedMs := m.queuedFor(queuedAt)
				log.Printf("[concurrency] queue wait exceeded fn=%s exec=%s queued_ms=%d", functionID, executionID, queuedMs)
				return queuedMs, ErrWaitTimeout
			}
			timer := time.NewTimer(m.cfg.PollInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				_ = m.leave(context.Background(), functionID, executionID)
				return m.queuedFor(queuedAt), ctx.Err()
			case <-timer.C:
			}
		default:
			_ = m.leave(context.Background(), functionID, executionID)
			return m.queuedFor(queuedAt), fmt.Errorf("unexpected admission status %q", status)
		}
	}
}

func (m *Manager) queuedFor(queuedAt time.Time) int64 {
	if queuedAt.IsZero() {
		return 0
	}
	ms := m.now().Sub(queuedAt).Milliseconds()
	if ms < 0 {
		return 0
	}
	return ms
}

// Release drops the slot, the queue membership, and the execution record.
func (m *Manager) Release(ctx context.Context, functionID, executionID string) error {
	m.stopKeepAlive(executionID)
	_, err := releaseScript.Run(ctx, m.rdb, []string{
		m.slotsKey(functionID),
		m.queueKey(functionID),
		m.globalKey(),
		m.execKey(executionID),
	}, executionID).Result()
	if err != nil {
		return fmt.Errorf("release slot: %w", err)
	}
	log.Printf("[concurrency] released fn=%s exec=%s", functionID, executionID)
	return nil
}

// MarkForCancellation flags a running execution and removes a queued one so
// it does not start. The waiting Acquire observes the flag and returns ErrCancelled.
func (m *Manager) MarkForCancellation(ctx context.Context, executionID string) error {
	res, err := cancelScript.Run(ctx, m.rdb, []string{m.execKey(executionID)},
		"nimbus:slots:fn:",
		"nimbus:queue:fn:",
		m.globalKey(),
		executionID,
	).Result()
	if err != nil {
		return fmt.Errorf("cancel execution: %w", err)
	}
	status, err := parseStatus(res)
	if err != nil {
		return err
	}
	if status == "missing" {
		return fmt.Errorf("ejecución %s no encontrada", executionID)
	}
	log.Printf("[concurrency] cancel %s exec=%s", status, executionID)
	return nil
}

// IsCancelled reports the flag stored for a live execution record.
func (m *Manager) IsCancelled(ctx context.Context, executionID string) bool {
	return m.isCancelled(ctx, executionID)
}

func (m *Manager) isCancelled(ctx context.Context, executionID string) bool {
	cancelled, err := m.rdb.HGet(ctx, m.execKey(executionID), "cancelled").Result()
	if err != nil {
		return false
	}
	return cancelled == "1" || cancelled == "true"
}

func (m *Manager) admit(ctx context.Context, functionID, executionID string, limit int) (string, error) {
	now := m.now().UnixMilli()
	res, err := admitScript.Run(ctx, m.rdb, []string{
		m.slotsKey(functionID),
		m.queueKey(functionID),
		m.globalKey(),
		m.execKey(executionID),
		m.seqKey(functionID),
	},
		executionID,
		functionID,
		limit,
		m.cfg.QueueMax,
		m.cfg.GlobalMax,
		now,
		m.cfg.Lease.Milliseconds(),
		m.cfg.QueueLease.Milliseconds(),
		m.metaTTL().Milliseconds(),
		"nimbus:exec:",
	).Result()
	if err != nil {
		return "", fmt.Errorf("admit: %w", err)
	}
	return parseStatus(res)
}

func (m *Manager) leave(ctx context.Context, functionID, executionID string) error {
	_, err := leaveScript.Run(ctx, m.rdb, []string{
		m.execKey(executionID),
		m.queueKey(functionID),
	}, executionID).Result()
	if err != nil {
		return fmt.Errorf("leave queue: %w", err)
	}
	return nil
}

func (m *Manager) touch(ctx context.Context, functionID, executionID string) error {
	now := m.now().UnixMilli()
	_, err := touchScript.Run(ctx, m.rdb, []string{
		m.slotsKey(functionID),
		m.queueKey(functionID),
		m.globalKey(),
		m.execKey(executionID),
	},
		executionID,
		now,
		m.cfg.Lease.Milliseconds(),
		m.cfg.QueueLease.Milliseconds(),
		m.metaTTL().Milliseconds(),
		"nimbus:exec:",
	).Result()
	if err != nil {
		return fmt.Errorf("refresh lease: %w", err)
	}
	return nil
}

func (m *Manager) metaTTL() time.Duration {
	ttl := m.cfg.MaxWait + 4*m.cfg.Lease
	if ttl < time.Minute {
		ttl = time.Minute
	}
	return ttl
}

func (m *Manager) startKeepAlive(functionID, executionID string) {
	interval := m.cfg.Lease / 3
	if interval < 50*time.Millisecond {
		interval = 50 * time.Millisecond
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	if old, ok := m.stops[executionID]; ok {
		old()
	}
	m.stops[executionID] = cancel
	m.mu.Unlock()

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := m.touch(context.Background(), functionID, executionID); err != nil {
					log.Printf("[concurrency] lease refresh failed fn=%s exec=%s: %v", functionID, executionID, err)
				}
			}
		}
	}()
}

func (m *Manager) stopKeepAlive(executionID string) {
	m.mu.Lock()
	cancel, ok := m.stops[executionID]
	if ok {
		delete(m.stops, executionID)
		cancel()
	}
	m.mu.Unlock()
}

func (m *Manager) stopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, cancel := range m.stops {
		cancel()
		delete(m.stops, id)
	}
}

func parseStatus(res interface{}) (string, error) {
	switch v := res.(type) {
	case []interface{}:
		if len(v) == 0 {
			return "", fmt.Errorf("empty admission reply")
		}
		return fmt.Sprint(v[0]), nil
	case string:
		return v, nil
	default:
		return "", fmt.Errorf("unexpected admission reply %T", res)
	}
}
