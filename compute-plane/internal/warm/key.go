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
