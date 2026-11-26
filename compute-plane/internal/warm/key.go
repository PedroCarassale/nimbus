package warm

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"

	"github.com/PedroCarassale/nimbus/compute-plane/internal/fnconfig"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/runtime"
)

// PoolKey identifies containers that can serve the same invocation.
// Timeout is not part of the key: the compute plane enforces it from
// outside, and a warm process can serve a different deadline next time.
// Memory, CPU, environment, and runtime are part of the container, so they
// are. An empty runtime shares the nodejs20 pool.
func PoolKey(functionID, version string, cfg fnconfig.Resolved) string {
	return strings.Join([]string{
		functionID,
		version,
		runtime.Canonical(cfg.Runtime),
		cfg.Handler,
		strconv.Itoa(cfg.MemoryMB),
		strconv.Itoa(cfg.CPUsMilli),
		EnvHash(cfg.Env),
	}, "\x1f")
}

// AffinityKey is the idle-warm token a worker publishes and the scheduler
// matches. The shape is functionId:version:runtime:configHash. configHash
// covers handler, memory, CPU and env, which is the part of PoolKey that is
// not already in the prefix. Timeout is excluded, same as PoolKey.
func AffinityKey(functionID, version string, cfg fnconfig.Resolved) string {
	return strings.Join([]string{
		functionID,
		version,
		runtime.Canonical(cfg.Runtime),
		ConfigHash(cfg),
	}, ":")
}

// ConfigHash is a stable digest of the container config that distinguishes
// two warm pools for the same function version and runtime.
func ConfigHash(cfg fnconfig.Resolved) string {
	raw := strings.Join([]string{
		cfg.Handler,
		strconv.Itoa(cfg.MemoryMB),
		strconv.Itoa(cfg.CPUsMilli),
		EnvHash(cfg.Env),
	}, "\n")
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// EnvHash is a stable digest of the user environment. Names are sorted
// so two requests with the same variables share a container. The digest
// is safe to log; the values are not.
func EnvHash(env []fnconfig.EnvVar) string {
	pairs := make([]string, len(env))
	for i, ev := range env {
		pairs[i] = ev.Name + "=" + ev.Value
	}
	sort.Strings(pairs)
	sum := sha256.Sum256([]byte(strings.Join(pairs, "\n")))
	return hex.EncodeToString(sum[:])
}
