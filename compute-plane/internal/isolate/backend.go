package isolate

import (
	"context"

	"github.com/PedroCarassale/nimbus/compute-plane/internal/warm"
)

// Backend starts one sandbox. Docker and Firecracker both implement it, and
// the warm pool only sees warm.Starter.
type Backend interface {
	Name() string
	Start(ctx context.Context, spec warm.Spec) (warm.Session, error)
}
