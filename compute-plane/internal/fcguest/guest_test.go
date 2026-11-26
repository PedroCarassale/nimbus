package fcguest

import (
	"os/exec"
	"testing"
)

func TestExitClass(t *testing.T) {
	code, oom := exitClass(nil)
	if code != 0 || oom {
		t.Fatalf("nil: %d %v", code, oom)
	}
	code, oom = exitClass(exec.Command("sh", "-c", "exit 7").Run())
	if code != 7 || oom {
		t.Fatalf("exit 7: %d %v", code, oom)
	}
	code, oom = exitClass(exec.Command("sh", "-c", "kill -9 $$").Run())
	if code != 137 || !oom {
		t.Fatalf("sigkill: %d %v", code, oom)
	}
	code, oom = exitClass(exec.Command("sh", "-c", "kill -6 $$").Run())
	if code != 134 || !oom {
		t.Fatalf("sigabrt: %d %v", code, oom)
	}
}
