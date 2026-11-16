package async

import "testing"

func TestNextAction(t *testing.T) {
	cases := []struct {
		name    string
		state   State
		job     int
		max     int
		action  string
		attempt int
	}{
		{name: "first", state: State{Phase: PhaseQueued}, job: 1, max: 3, action: ActionRun, attempt: 1},
		{name: "scheduled", state: State{Phase: PhaseRetrying, Attempt: 2}, job: 2, max: 3, action: ActionRun, attempt: 2},
		{name: "stale", state: State{Phase: PhaseRetrying, Attempt: 2}, job: 1, max: 3, action: ActionAck, attempt: 2},
		{name: "crash bumps", state: State{Phase: PhaseRunning, Attempt: 1}, job: 1, max: 3, action: ActionRun, attempt: 2},
		{name: "crash at cap", state: State{Phase: PhaseRunning, Attempt: 3}, job: 1, max: 3, action: ActionDead, attempt: 3},
		{name: "done", state: State{Phase: PhaseSucceeded, Attempt: 1}, job: 1, max: 3, action: ActionAck, attempt: 1},
		{name: "failed", state: State{Phase: PhaseFailed, Attempt: 1}, job: 1, max: 3, action: ActionAck, attempt: 1},
		{name: "dead", state: State{Phase: PhaseDead, Attempt: 3}, job: 3, max: 3, action: ActionAck, attempt: 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			action, attempt := NextAction(tc.state, tc.job, tc.max)
			if action != tc.action || attempt != tc.attempt {
				t.Fatalf("got %s %d, want %s %d", action, attempt, tc.action, tc.attempt)
			}
		})
	}
}
