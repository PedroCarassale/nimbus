package async

const (
	PhaseQueued    = "queued"
	PhaseRunning   = "running"
	PhaseRetrying  = "retrying"
	PhaseSucceeded = "succeeded"
	PhaseFailed    = "failed" // terminal, not a dead letter (validation / cancel)
	PhaseDead      = "dead"

	ActionAck  = "ack"
	ActionRun  = "run"
	ActionDead = "dead"

	KindOK         = "ok"
	KindRetry      = "retry"
	KindValidation = "validation"
	KindFatal      = "fatal"
)

// NextAction decides what to do with a delivery once this worker holds the
// run lock. The lock being held means nobody else is executing it: a phase
// of "running" is a worker that died mid-attempt.
//
// jobAttempt is the attempt number stored on the message. maxAttempts counts
// the first try. The returned attempt is the one this worker should start.
func NextAction(st State, jobAttempt, maxAttempts int) (action string, attempt int) {
	switch st.Phase {
	case PhaseSucceeded, PhaseFailed, PhaseDead:
		return ActionAck, st.Attempt
	}
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	if jobAttempt < 1 {
		jobAttempt = 1
	}

	// A retry is already queued. This delivery is an older copy.
	if st.Phase == PhaseRetrying && jobAttempt < st.Attempt {
		return ActionAck, st.Attempt
	}

	// The attempt in state started and the lock expired, so that worker is gone.
	if st.Phase == PhaseRunning {
		next := st.Attempt + 1
		if next < jobAttempt {
			next = jobAttempt
		}
		if next > maxAttempts {
			return ActionDead, st.Attempt
		}
		return ActionRun, next
	}

	if jobAttempt > maxAttempts {
		return ActionDead, jobAttempt
	}
	return ActionRun, jobAttempt
}
