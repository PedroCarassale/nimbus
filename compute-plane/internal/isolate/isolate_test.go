package isolate

import "testing"

func TestParse(t *testing.T) {
	for _, raw := range []string{"", "docker", " Docker "} {
		got, err := Parse(raw)
		if err != nil || got != Docker {
			t.Fatalf("%q -> %s %v", raw, got, err)
		}
	}
	got, err := Parse("Firecracker")
	if err != nil || got != Firecracker {
		t.Fatalf("firecracker: %s %v", got, err)
	}
	if _, err := Parse("gvisor"); err == nil {
		t.Fatal("expected error")
	}
}

func TestLabelClosesTheSet(t *testing.T) {
	if Label("") != Docker || Label("nope") != Docker {
		t.Fatal("docker")
	}
	if Label("firecracker") != Firecracker {
		t.Fatal("firecracker")
	}
}
