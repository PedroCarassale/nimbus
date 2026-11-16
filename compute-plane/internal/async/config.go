// Package async is the durable background path for invocations.
//
// Jobs live in a Redis stream. Workers in this process claim them with a
// consumer group, run the same execution path as POST /executions, and
// either ack, delay a retry, or move the job to a dead-letter stream.
// Results are published on a second stream so the control plane can record
// the final row without sharing a database.
package async

import (
	"log"
	"os"
	"strconv"
	"time"
)

const (
	// DefaultMaxAttempts is the first try plus two retries, matching Lambda.
	DefaultMaxAttempts = 3

	streamJobs    = "nimbus:async:jobs"
	streamDLQ     = "nimbus:async:dlq"
	streamResults = "nimbus:async:results"
	groupWorkers  = "workers"
	retryKey      = "nimbus:async:retry"
	dlqItemsKey   = "nimbus:async:dlq:items"
	dlqIndexKey   = "nimbus:async:dlq:index"
)

// Config is the compute-plane side of async invoke.
type Config struct {
	Enabled       bool
	MaxAttempts   int
	BackoffBase   time.Duration
	BackoffMax    time.Duration
	BackoffJitter time.Duration
	ClaimIdle     time.Duration
	LockTTL       time.Duration
	Workers       int
}

// LoadConfig reads the async environment. Invalid numbers keep the default.
//
//	NIMBUS_ASYNC_ENABLED            default on. 0/false/off/no stops workers.
//	NIMBUS_ASYNC_MAX_ATTEMPTS       default 3 (one try + two retries).
//	NIMBUS_ASYNC_BACKOFF_BASE_MS    default 500. Delay after attempt 1.
//	NIMBUS_ASYNC_BACKOFF_MAX_MS     default 30000.
//	NIMBUS_ASYNC_BACKOFF_JITTER_MS  default 250. Added at random, 0..jitter.
//	NIMBUS_ASYNC_CLAIM_IDLE_MS      default 20000. XAUTOCLAIM min idle.
//	NIMBUS_ASYNC_LOCK_MS            default 15000. Refreshed while running.
//	NIMBUS_ASYNC_WORKERS            default 4.
func LoadConfig() Config {
	cfg := Config{
		Enabled:       envBool("NIMBUS_ASYNC_ENABLED", true),
		MaxAttempts:   DefaultMaxAttempts,
		BackoffBase:   500 * time.Millisecond,
		BackoffMax:    30 * time.Second,
		BackoffJitter: 250 * time.Millisecond,
		ClaimIdle:     20 * time.Second,
		LockTTL:       15 * time.Second,
		Workers:       4,
	}
	if n, ok := envInt("NIMBUS_ASYNC_MAX_ATTEMPTS"); ok && n >= 1 {
		cfg.MaxAttempts = n
	} else if ok {
		log.Printf("[async] NIMBUS_ASYNC_MAX_ATTEMPTS fuera de rango, se usa %d", cfg.MaxAttempts)
	}
	cfg.BackoffBase = envDuration("NIMBUS_ASYNC_BACKOFF_BASE_MS", cfg.BackoffBase)
	cfg.BackoffMax = envDuration("NIMBUS_ASYNC_BACKOFF_MAX_MS", cfg.BackoffMax)
	if cfg.BackoffMax < cfg.BackoffBase {
		cfg.BackoffMax = cfg.BackoffBase
	}
	cfg.BackoffJitter = envDuration("NIMBUS_ASYNC_BACKOFF_JITTER_MS", cfg.BackoffJitter)
	cfg.ClaimIdle = envDuration("NIMBUS_ASYNC_CLAIM_IDLE_MS", cfg.ClaimIdle)
	cfg.LockTTL = envDuration("NIMBUS_ASYNC_LOCK_MS", cfg.LockTTL)
	if n, ok := envInt("NIMBUS_ASYNC_WORKERS"); ok && n >= 1 {
		cfg.Workers = n
	} else if ok {
		log.Printf("[async] NIMBUS_ASYNC_WORKERS fuera de rango, se usa %d", cfg.Workers)
	}
	return cfg
}

func envInt(key string) (int, bool) {
	raw := os.Getenv(key)
	if raw == "" {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		log.Printf("[async] %s inválido, se ignora", key)
		return 0, false
	}
	return n, true
}

func envDuration(key string, fallback time.Duration) time.Duration {
	n, ok := envInt(key)
	if !ok {
		return fallback
	}
	if n < 0 {
		log.Printf("[async] %s negativo, se usa el default", key)
		return fallback
	}
	return time.Duration(n) * time.Millisecond
}

func envBool(key string, fallback bool) bool {
	switch os.Getenv(key) {
	case "":
		return fallback
	case "0", "false", "off", "no", "FALSE", "OFF", "NO":
		return false
	default:
		return true
	}
}

func stateKey(id string) string  { return "nimbus:async:state:" + id }
func lockKey(id string) string   { return "nimbus:async:lock:" + id }
func acceptKey(id string) string { return "nimbus:async:accepted:" + id }
func seqKey(id string) string    { return "nimbus:async:seq:" + id }
