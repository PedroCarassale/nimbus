package concurrency

import (
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the admission policy for one compute process.
// Zero-value fields are filled by NewManager.
type Config struct {
	// MaxConcurrency is the default number of simultaneous executions per function.
	MaxConcurrency int
	// QueueMax is how many invocations may wait per function. Zero means no waiting.
	QueueMax int
	// GlobalMax is the maximum number of executions this process may run at once.
	GlobalMax int
	// MaxWait is how long an invocation may sit in the queue.
	MaxWait time.Duration
	// Lease is how long a granted slot stays valid without a refresh.
	// A dead process stops refreshing and the slot can be taken by someone else.
	Lease time.Duration
	// QueueLease is how long a queued invocation may go without a heartbeat
	// before the next admission drops it. Zero means MaxWait.
	QueueLease time.Duration
	// PollInterval is how often a waiter retries admission.
	PollInterval time.Duration
}

// LoadConfig reads the process environment.
//
//	NIMBUS_FN_CONCURRENCY      default 2
//	NIMBUS_FN_QUEUE_MAX        default 32 (0 disables the queue)
//	NIMBUS_FN_QUEUE_WAIT_SEC   default 30
//	NIMBUS_MAX_CONTAINERS      default 8
//	NIMBUS_SLOT_LEASE_SEC      default 30
func LoadConfig() Config {
	return Config{
		MaxConcurrency: envInt("NIMBUS_FN_CONCURRENCY", 2, false),
		QueueMax:       envInt("NIMBUS_FN_QUEUE_MAX", 32, true),
		GlobalMax:      envInt("NIMBUS_MAX_CONTAINERS", 8, false),
		MaxWait:        time.Duration(envInt("NIMBUS_FN_QUEUE_WAIT_SEC", 30, false)) * time.Second,
		Lease:          time.Duration(envInt("NIMBUS_SLOT_LEASE_SEC", 30, false)) * time.Second,
		PollInterval:   50 * time.Millisecond,
	}
}

func envInt(key string, def int, allowZero bool) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 || (n == 0 && !allowZero) {
		log.Printf("[concurrency] invalid %s=%q, using %d", key, raw, def)
		return def
	}
	return n
}
