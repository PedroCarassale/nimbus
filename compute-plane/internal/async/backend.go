package async

import (
	"context"
	"encoding/json"
	"time"
)

// Backend is the async transport. *Queue is the Redis stream. *SQSQueue is
// Amazon SQS (or LocalStack) plus the same Redis records for state, locks,
// cancel and results.
type Backend interface {
	EnsureGroup(context.Context) error
	PromoteDue(context.Context, int) (int, error)
	Claim(context.Context, int) ([]Delivery, error)
	Read(context.Context, int, time.Duration) ([]Delivery, error)
	Ack(context.Context, Delivery) error
	LoadState(context.Context, string) (State, error)
	SaveState(context.Context, string, State) error
	TryLock(context.Context, string, string) (bool, error)
	RefreshLock(context.Context, string, string) error
	Unlock(context.Context, string, string) error
	Publish(context.Context, Result) error
	NextSeq(context.Context, string) (int64, error)
	RetryDelay(int) time.Duration
	Now() time.Time
	Config() Config
	Consumer() string
	Fork(string) Backend
	Postpone(context.Context, Delivery, Job, time.Duration, State) error
	SealFailed(context.Context, Delivery, State) error
	SealDead(context.Context, Delivery, State, DLQEntry) error
	Extend(context.Context, Delivery) error
}

// Queuer is the HTTP surface: enqueue, list the DLQ and redrive one entry.
type Queuer interface {
	Enqueue(ctx context.Context, invocationID, executionID string, body json.RawMessage) (string, bool, error)
	ListDLQ(ctx context.Context) ([]DLQEntry, error)
	Redrive(ctx context.Context, id string) (DLQEntry, error)
	DLQLen(ctx context.Context) (int64, error)
}
