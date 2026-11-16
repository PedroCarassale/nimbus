// Package warm keeps a few user containers running so the next invocation
// of the same function does not pay for docker run.
package warm

import (
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the warm pool policy for one compute process.
type Config struct {
	// Enabled is false when NIMBUS_WARM disables the pool. The executor
	// then uses a fresh container per invocation.
	Enabled bool
	// IdleTTL is how long an idle container stays in the pool.
	IdleTTL time.Duration
	// MaxPerFunction bounds warm containers (busy and idle) for one function.
	// It tracks the per-function concurrency limit.
	MaxPerFunction int
	// GlobalMax bounds warm containers in this process. Idle containers count.
	// When a new one would pass the cap, the oldest idle container is removed.
	GlobalMax int
}

// LoadConfig reads the process environment.
//
//	NIMBUS_WARM              default on. 0, false, off, or no disables it.
//	NIMBUS_WARM_IDLE_SEC     default 60. Idle containers are removed after this.
//	NIMBUS_FN_CONCURRENCY    default 2. Max warm containers per function.
//	NIMBUS_MAX_CONTAINERS    default 8. Max warm containers in this process.
func LoadConfig() Config {
	cfg := Config{
		Enabled:        warmEnabled(os.Getenv("NIMBUS_WARM")),
		IdleTTL:        time.Duration(envInt("NIMBUS_WARM_IDLE_SEC", 60)) * time.Second,
		MaxPerFunction: envInt("NIMBUS_FN_CONCURRENCY", 2),
		GlobalMax:      envInt("NIMBUS_MAX_CONTAINERS", 8),
	}
	return cfg.normalized()
}

func (c Config) normalized() Config {
	if c.IdleTTL < time.Second {
		c.IdleTTL = 60 * time.Second
	}
	if c.MaxPerFunction < 1 {
		c.MaxPerFunction = 2
	}
	if c.GlobalMax < 1 {
		c.GlobalMax = 8
	}
	if c.MaxPerFunction > c.GlobalMax {
		c.MaxPerFunction = c.GlobalMax
	}
	return c
}

func warmEnabled(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

func envInt(key string, def int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		log.Printf("[warm] invalid %s=%q, using %d", key, raw, def)
		return def
	}
	return n
}
