package scheduler

import (
	"encoding/json"
	"strings"

	"github.com/PedroCarassale/nimbus/compute-plane/internal/fnconfig"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/runtime"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/warm"
)

// affinityKey is the warm token for a POST /executions body.
// An unreadable body or a config the worker would reject yields "" and the
// request is still forwarded: the worker answers 400.
func affinityKey(body []byte, defaults fnconfig.Defaults) string {
	if len(body) == 0 {
		return ""
	}
	var req struct {
		FunctionID string          `json:"functionId"`
		Version    string          `json:"version"`
		TimeoutSec json.RawMessage `json:"timeoutSec"`
		MemoryMB   json.RawMessage `json:"memoryMb"`
		CPUs       json.RawMessage `json:"cpus"`
		Handler    json.RawMessage `json:"handler"`
		EnvVars    json.RawMessage `json:"envVars"`
		Runtime    string          `json:"runtime"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return ""
	}
	if req.FunctionID == "" || req.Version == "" {
		return ""
	}
	resolved, err := fnconfig.Resolve(fnconfig.Raw{
		TimeoutSec: req.TimeoutSec,
		MemoryMB:   req.MemoryMB,
		CPUs:       req.CPUs,
		Handler:    req.Handler,
		EnvVars:    req.EnvVars,
	}, defaults)
	if err != nil {
		return ""
	}
	rt, err := runtime.Normalize(req.Runtime)
	if err != nil {
		return ""
	}
	resolved.Runtime = rt
	if rt == runtime.Go1x && handlerOmitted(req.Handler) {
		resolved.Handler = "bootstrap"
	}
	return warm.AffinityKey(req.FunctionID, req.Version, resolved)
}

func handlerOmitted(raw json.RawMessage) bool {
	text := strings.TrimSpace(string(raw))
	return text == "" || text == "null"
}
