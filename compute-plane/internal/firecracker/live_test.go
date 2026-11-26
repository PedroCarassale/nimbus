package firecracker

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PedroCarassale/nimbus/compute-plane/internal/fnconfig"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/runtime"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/warm"
)

// TestLiveNodeHello boots one microVM when NIMBUS_FC_LIVE=1.
// go test without that variable does not touch KVM.
func TestLiveNodeHello(t *testing.T) {
	if os.Getenv("NIMBUS_FC_LIVE") != "1" {
		t.Skip("NIMBUS_FC_LIVE no está en 1")
	}
	work := t.TempDir()
	src := []byte("exports.handler = async (event) => ({ message: 'Hola, ' + (event.name || 'mundo'), uid: process.getuid() });\n")
	if err := os.WriteFile(filepath.Join(work, "index.js"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	sess, err := NewFromEnv().Start(ctx, warm.Spec{
		FunctionID: "live",
		Version:    "v",
		WorkDir:    work,
		Config: fnconfig.Resolved{
			TimeoutSec:    30,
			MemoryMB:      128,
			CPUsMilli:     1000,
			Handler:       "index.handler",
			HandlerFile:   "index",
			HandlerExport: "handler",
			Runtime:       runtime.NodeJS20,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Destroy(context.Background())
	out := sess.Invoke(ctx, warm.Invocation{
		ExecutionID: "live-1",
		Event:       map[string]interface{}{"name": "Ada"},
	})
	if !out.Success {
		t.Fatalf("%+v", out)
	}
	msg, _ := out.Output.(map[string]interface{})
	if msg["message"] != "Hola, Ada" {
		t.Fatalf("output %#v initMs=%d", out.Output, sess.InitMs())
	}
	t.Logf("initMs=%d", sess.InitMs())
}
