// Package cancel is the in-process signal for POST /executions/:id/cancel.
//
// The Redis flag stops an execution that has not started. A running one also
// needs this process to abandon the container: the HTTP handler cancels the
// context with ErrRun, and the executor kills that container.
package cancel

import (
	"context"
	"errors"
)

// ErrRun is the cause stored on the execution context when the user cancels
// a running invocation. A timeout and a disconnected client use other causes.
var ErrRun = errors.New("execution cancelled")

// Message is the error returned to Nest and stored on the invocation.
// Prisma has no CANCELLED status; the row stays ERROR with this text.
const Message = "Ejecución cancelada"

// ExitCode is the platform exit for a killed invocation.
// It is not 124 (timeout) and not 137/134 (OOM). docker rm -f also ends in
// 137, so callers must apply this code when Is reports a user cancel.
const ExitCode = 130

// Is reports whether ctx was cancelled by Abort, including through a child
// timeout context. The first cause wins: a deadline that already fired is
// not rewritten into a cancel.
func Is(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), ErrRun)
}
