package firecracker

import (
	"testing"

	"github.com/PedroCarassale/nimbus/compute-plane/internal/fnconfig"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/runtime"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/warm"
)

func TestMemMiBKeepsTheRequestAndAddsKernelReserve(t *testing.T) {
	if MemMiB(64) != 128 || MemMiB(128) != 192 || MemMiB(0) != 192 {
		t.Fatalf("64=%d 128=%d 0=%d", MemMiB(64), MemMiB(128), MemMiB(0))
	}
}

func TestVCPUCount(t *testing.T) {
	cases := []struct {
		milli int
		want  int
	}{
		{0, 1},
		{100, 1},
		{1000, 1},
		{1001, 2},
		{2000, 2},
		{2500, 4},
		{4000, 4},
		{9000, 4},
	}
	for _, tc := range cases {
		if got := VCPUCount(tc.milli); got != tc.want {
			t.Fatalf("milli %d: got %d want %d", tc.milli, got, tc.want)
		}
	}
}

func TestMachineConfigHasNoNIC(t *testing.T) {
	body := machineBody(128, 2)
	if body["mem_size_mib"] != 128 || body["vcpu_count"] != 2 {
		t.Fatalf("%v", body)
	}
	if _, ok := body["smt"]; !ok {
		t.Fatal("smt")
	}
	for _, banned := range []string{"network", "nic", "iface"} {
		if _, ok := body[banned]; ok {
			t.Fatalf("network field %s", banned)
		}
	}
}

func TestGuestConfigPlatformWins(t *testing.T) {
	spec := warm.Spec{
		TraceID:     "abc",
		TraceParent: "00-abc-def-01",
		Config: fnconfig.Resolved{
			Runtime:       runtime.Python312,
			Handler:       "index.handler",
			HandlerFile:   "index",
			HandlerExport: "handler",
			MemoryMB:      128,
			TimeoutSec:    5,
			Env: []fnconfig.EnvVar{
				{Name: "GREETING", Value: "hola"},
				{Name: "NIMBUS_WARM", Value: "0"},
			},
		},
	}
	cfg := buildGuestConfig(spec)
	if cfg.Env["GREETING"] != "hola" {
		t.Fatalf("user env %+v", cfg.Env["GREETING"])
	}
	if cfg.Env["NIMBUS_WARM"] != "1" || cfg.Env["NIMBUS_HANDLER"] != "index.handler" {
		t.Fatalf("platform %+v", cfg.Env)
	}
	if cfg.Env["NIMBUS_HANDLER_PATH"] != "/var/task/index.py" {
		t.Fatalf("path %s", cfg.Env["NIMBUS_HANDLER_PATH"])
	}
	if cfg.Env["NIMBUS_TRACE_ID"] != "abc" || cfg.Env["TRACEPARENT"] == "" {
		t.Fatalf("trace %+v", cfg.Env)
	}
	if cfg.UID != 1000 || cfg.Argv[0] != "/usr/local/bin/python3" {
		t.Fatalf("argv/uid %+v", cfg)
	}
	if cfg.Env["HOME"] != "/tmp" {
		t.Fatal("home")
	}
}

func TestArgvPerRuntime(t *testing.T) {
	if argvFor(runtime.NodeJS20)[0] != "/usr/local/bin/node" {
		t.Fatal("node")
	}
	if argvFor(runtime.Go1x)[0] != "/var/runtime/bootstrap" {
		t.Fatal("go")
	}
}
