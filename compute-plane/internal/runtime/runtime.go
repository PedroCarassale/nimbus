// Package runtime names the language images the compute plane can start.
// The set is closed on purpose: a metric label and a Docker image per name.
package runtime

import (
	"strings"

	"github.com/PedroCarassale/nimbus/compute-plane/internal/fnconfig"
)

const (
	// NodeJS20 is the default. An omitted runtime field keeps this value so
	// older clients stay on the Node image.
	NodeJS20 = "nodejs20"
	// Python312 loads a file.function handler from the zip.
	Python312 = "python3.12"
	// Go1x runs a static linux binary shipped in the zip (provided-style).
	// The compute plane does not compile Go source: the container has no
	// network and no toolchain.
	Go1x = "go1.x"
)

// Normalize returns a canonical runtime name.
// An empty value is nodejs20. Anything else is a field error on "runtime".
func Normalize(raw string) (string, error) {
	switch strings.TrimSpace(raw) {
	case "", NodeJS20:
		return NodeJS20, nil
	case Python312:
		return Python312, nil
	case Go1x:
		return Go1x, nil
	default:
		return "", &fnconfig.FieldError{
			Field:   "runtime",
			Message: "runtime debe ser nodejs20, python3.12 o go1.x",
		}
	}
}

// Canonical maps empty and unknown values to nodejs20.
// Callers that already passed Normalize will not see an unknown value.
func Canonical(name string) string {
	switch name {
	case Python312, Go1x:
		return name
	default:
		return NodeJS20
	}
}

// Image is the Docker tag for a runtime. Node keeps the historical name
// nimbus-node; NIMBUS_IMAGE can still override that one image.
func Image(name string) string {
	switch Canonical(name) {
	case Python312:
		return "nimbus-python"
	case Go1x:
		return "nimbus-go"
	default:
		return "nimbus-node"
	}
}

// SourceDir is the repo directory that builds image, when image is one of
// the three platform images. A custom NIMBUS_IMAGE has no directory here.
func SourceDir(image string) (string, bool) {
	switch image {
	case "nimbus-node":
		return "runtime-node", true
	case "nimbus-python":
		return "runtime-python", true
	case "nimbus-go":
		return "runtime-go", true
	default:
		return "", false
	}
}

// RunsAsNode reports whether the cold container must drop to the image's
// unprivileged user. The Node cold path stays root, which is the previous
// contract. Warm containers of every runtime already use --user node.
func RunsAsNode(name string) bool {
	switch Canonical(name) {
	case Python312, Go1x:
		return true
	default:
		return false
	}
}

// TaskPath is NIMBUS_HANDLER_PATH inside the container.
func TaskPath(rt, handler, handlerFile string) string {
	switch Canonical(rt) {
	case Python312:
		return "/var/task/" + handlerFile + ".py"
	case Go1x:
		rel := handler
		if rel == "" {
			rel = "bootstrap"
		}
		return "/var/task/" + rel
	default:
		return "/var/task/" + handlerFile + ".js"
	}
}

// Label is the metric value. Only the three known names are returned, so a
// bug that skipped validation cannot open a new series per request.
func Label(name string) string {
	return Canonical(name)
}
