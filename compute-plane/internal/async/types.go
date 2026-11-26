package async

import "encoding/json"

// Job is one durable unit of work. Body is the POST /executions JSON.
type Job struct {
	InvocationID string          `json:"invocationId"`
	Attempt      int             `json:"attempt"`
	MaxAttempts  int             `json:"maxAttempts"`
	EnqueuedAt   int64           `json:"enqueuedAt"`
	Body         json.RawMessage `json:"body"`
	// TraceParent and TraceState are the W3C headers captured when the job
	// was accepted. Retries keep them, so every attempt stays on the same trace.
	TraceParent string `json:"traceparent,omitempty"`
	TraceState  string `json:"tracestate,omitempty"`
}

// State is the per-invocation record. Result is set only on a terminal phase
// so a reclaimed message can republish it without running again.
type State struct {
	Phase        string  `json:"phase"`
	Attempt      int     `json:"attempt"`
	LastError    string  `json:"lastError,omitempty"`
	ExitCode     int     `json:"exitCode,omitempty"`
	EnqueuedAt   int64   `json:"enqueuedAt,omitempty"`
	AttemptTimes []int64 `json:"attemptTimes,omitempty"`
	Result       *Result `json:"result,omitempty"`
}

// Result is what the control plane applies to the Invocation row.
type Result struct {
	InvocationID string      `json:"invocationId"`
	ExecutionID  string      `json:"executionId"`
	Status       string      `json:"status"`
	Output       interface{} `json:"output,omitempty"`
	Error        string      `json:"error,omitempty"`
	ExitCode     int         `json:"exitCode,omitempty"`
	DurationMs   int64       `json:"durationMs,omitempty"`
	Attempts     int         `json:"attempts"`
	ColdStart    bool        `json:"coldStart,omitempty"`
	InitMs       int64       `json:"initMs,omitempty"`
	QueuedMs     int64       `json:"queuedMs,omitempty"`
	Redrive      bool        `json:"redrive,omitempty"`
	At           int64       `json:"at"`
	// Seq orders updates for one invocation. The control plane ignores a
	// lower seq, so a reclaimed copy cannot overwrite a newer row.
	Seq int64 `json:"seq"`
}

// DLQEntry is one invocation that used every attempt.
type DLQEntry struct {
	ID           string          `json:"id"`
	InvocationID string          `json:"invocationId"`
	ExecutionID  string          `json:"executionId"`
	FunctionID   string          `json:"functionId"`
	Version      string          `json:"version"`
	Event        json.RawMessage `json:"event,omitempty"`
	Payload      json.RawMessage `json:"payload"`
	LastError    string          `json:"lastError"`
	ExitCode     int             `json:"exitCode"`
	Attempts     int             `json:"attempts"`
	MaxAttempts  int             `json:"maxAttempts"`
	EnqueuedAt   int64           `json:"enqueuedAt"`
	AttemptTimes []int64         `json:"attemptTimes"`
	FailedAt     int64           `json:"failedAt"`
	DeadAt       int64           `json:"deadAt"`
	DurationMs   int64           `json:"durationMs"`
}

// RunResult is the outcome of one attempt, produced by the execution path.
type RunResult struct {
	Kind       string
	Error      string
	ExitCode   int
	Output     interface{}
	DurationMs int64
	ColdStart  bool
	InitMs     int64
	QueuedMs   int64
}

// Delivery is a stream or SQS message already parsed into a job.
// ReceiptHandle and ReceiveCount are set for SQS. ReceiveCount 0 means the
// Redis path, which keeps the attempt number on the job itself.
type Delivery struct {
	ID            string
	ReceiptHandle string
	ReceiveCount  int
	Job           Job
}
