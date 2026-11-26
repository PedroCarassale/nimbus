// Package isolate names the sandbox backend.
// Docker is the default. Firecracker is selected with NIMBUS_ISOLATION and
// does not change the invoke contract.
package isolate

import (
	"fmt"
	"os"
	"strings"
)

const (
	Docker      = "docker"
	Firecracker = "firecracker"
)

// Parse accepts an empty value as Docker. Anything else but the two names
// is an error the process should refuse to start with.
func Parse(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", Docker:
		return Docker, nil
	case Firecracker:
		return Firecracker, nil
	default:
		return "", fmt.Errorf("NIMBUS_ISOLATION debe ser docker o firecracker (llegó %q)", strings.TrimSpace(raw))
	}
}

// Current reads NIMBUS_ISOLATION. Invalid values are not a metric label;
// the process refuses them at startup, and a scrape still needs a value.
func Current() string {
	name, err := Parse(os.Getenv("NIMBUS_ISOLATION"))
	if err != nil {
		return Docker
	}
	return name
}

// Label collapses unknown values onto docker so a bug cannot open a series.
func Label(name string) string {
	if name == Firecracker {
		return Firecracker
	}
	return Docker
}
