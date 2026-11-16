package handlers

import (
	"context"
	"sync"

	"github.com/PedroCarassale/nimbus/compute-plane/internal/cancel"
)

// runStops maps an execution that is inside Execute to the function that
// abandons it. Queued executions are not registered: Redis removes them.
type runStops struct {
	mu sync.Mutex
	m  map[string]context.CancelCauseFunc
}

func newRunStops() *runStops {
	return &runStops{m: map[string]context.CancelCauseFunc{}}
}

func (s *runStops) track(executionID string, stop context.CancelCauseFunc) {
	if s == nil || executionID == "" || stop == nil {
		return
	}
	s.mu.Lock()
	s.m[executionID] = stop
	s.mu.Unlock()
}

func (s *runStops) untrack(executionID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.m, executionID)
	s.mu.Unlock()
}

// abort cancels the in-process run, if this process is the one executing it.
func (s *runStops) abort(executionID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	stop := s.m[executionID]
	s.mu.Unlock()
	if stop != nil {
		stop(cancel.ErrRun)
	}
}
