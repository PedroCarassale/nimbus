package async

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"
)

// Runner executes one attempt. The compute plane passes the same function
// used by POST /executions.
type Runner func(ctx context.Context, body json.RawMessage) RunResult

// Worker consumes the job stream.
type Worker struct {
	q   *Queue
	run Runner
	cfg Config
}

func NewWorker(q *Queue, run Runner) *Worker {
	return &Worker{q: q, run: run, cfg: q.cfg}
}

// StartWorkers runs n consumers until stop is called. In-flight attempts are
// given a few seconds to ack; a hard kill leaves the message pending so the
// next process can claim it.
func StartWorkers(q *Queue, run Runner, n int) (stop func()) {
	if n < 1 {
		n = 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		name := fmt.Sprintf("%s-%d", q.consumer, i)
		consumer := q.WithConsumer(name)
		worker := NewWorker(consumer, run)
		go func() {
			defer wg.Done()
			if err := consumer.EnsureGroup(ctx); err != nil && ctx.Err() == nil {
				log.Printf("[async] no se pudo crear el consumer group: %v", err)
			}
			worker.Run(ctx)
		}()
	}
	return func() {
		cancel()
		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			log.Printf("[async] apagado con workers todavía en curso")
		}
	}
}

func (w *Worker) Run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		if _, err := w.q.PromoteDue(ctx, 32); err != nil && ctx.Err() == nil {
			log.Printf("[async] promote: %v", err)
			if !sleep(ctx, 500*time.Millisecond) {
				return
			}
			continue
		}
		claimed, err := w.q.Claim(ctx, 16)
		if err != nil && ctx.Err() == nil {
			log.Printf("[async] claim: %v", err)
			if !sleep(ctx, 500*time.Millisecond) {
				return
			}
			continue
		}
		for _, d := range claimed {
			w.Handle(ctx, d)
		}
		fresh, err := w.q.Read(ctx, 8, time.Second)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("[async] read: %v", err)
			if !sleep(ctx, 500*time.Millisecond) {
				return
			}
			continue
		}
		for _, d := range fresh {
			w.Handle(ctx, d)
		}
	}
}

// Handle applies one delivery. It is safe to call again after a crash: a
// finished invocation is acked, and a live one is left pending.
func (w *Worker) Handle(ctx context.Context, d Delivery) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[async] panic procesando %s: %v", d.Job.InvocationID, rec)
		}
	}()
	job := d.Job
	if job.InvocationID == "" || d.ID == "" {
		log.Printf("[async] mensaje sin invocación, se descarta %s", d.ID)
		_ = w.q.Ack(ctx, d.ID)
		return
	}
	if job.MaxAttempts < 1 {
		job.MaxAttempts = w.cfg.MaxAttempts
	}

	st, err := w.q.LoadState(ctx, job.InvocationID)
	if err != nil {
		log.Printf("[async] estado %s: %v", job.InvocationID, err)
		return
	}
	if isTerminal(st.Phase) {
		w.finishTerminal(ctx, d, st)
		return
	}

	token := w.q.consumer + ":" + d.ID
	ok, err := w.q.TryLock(ctx, job.InvocationID, token)
	if err != nil {
		log.Printf("[async] lock %s: %v", job.InvocationID, err)
		return
	}
	if !ok {
		return
	}
	defer func() { _ = w.q.Unlock(context.Background(), job.InvocationID, token) }()

	st, err = w.q.LoadState(ctx, job.InvocationID)
	if err != nil {
		log.Printf("[async] estado %s: %v", job.InvocationID, err)
		return
	}
	if isTerminal(st.Phase) {
		w.finishTerminal(ctx, d, st)
		return
	}

	action, attempt := NextAction(st, job.Attempt, job.MaxAttempts)
	switch action {
	case ActionAck:
		_ = w.q.Ack(ctx, d.ID)
	case ActionDead:
		w.dead(ctx, d, job, st, st.Attempt, firstNonEmpty(st.LastError, "el worker se cayó durante el intento"), st.ExitCode)
	default:
		w.runAttempt(ctx, d, job, st, attempt)
	}
}

func (w *Worker) runAttempt(ctx context.Context, d Delivery, job Job, st State, attempt int) {
	times := append([]int64{}, st.AttemptTimes...)
	times = append(times, w.q.Now().UnixMilli())
	running := State{
		Phase:        PhaseRunning,
		Attempt:      attempt,
		LastError:    st.LastError,
		ExitCode:     st.ExitCode,
		EnqueuedAt:   firstNonZero(st.EnqueuedAt, job.EnqueuedAt),
		AttemptTimes: times,
	}
	if err := w.q.SaveState(ctx, job.InvocationID, running); err != nil {
		log.Printf("[async] no se pudo marcar running %s: %v", job.InvocationID, err)
		return
	}
	meta := peekBody(job.Body)
	_ = w.q.Publish(ctx, Result{
		InvocationID: job.InvocationID,
		ExecutionID:  meta.ExecutionID,
		Status:       "RUNNING",
		Attempts:     attempt,
		Error:        st.LastError,
		At:           w.q.Now().UnixMilli(),
	})

	refreshCtx, stopRefresh := context.WithCancel(context.Background())
	go w.refresh(refreshCtx, job.InvocationID, w.q.consumer+":"+d.ID)

	result := w.run(ctx, job.Body)
	stopRefresh()

	switch result.Kind {
	case KindOK:
		res := w.terminal(job, meta, "SUCCESS", attempt, result)
		if !w.seal(ctx, job.InvocationID, &res) {
			return
		}
		done := running
		done.Phase = PhaseSucceeded
		done.LastError = ""
		done.ExitCode = 0
		done.Result = &res
		if err := w.q.SaveState(ctx, job.InvocationID, done); err != nil {
			log.Printf("[async] no se pudo cerrar %s: %v", job.InvocationID, err)
			return
		}
		_ = w.q.Publish(ctx, res)
		_ = w.q.Ack(ctx, d.ID)
	case KindValidation, KindFatal:
		if result.Error == "" {
			result.Error = "la invocación no se puede ejecutar"
		}
		res := w.terminal(job, meta, "ERROR", attempt, result)
		if !w.seal(ctx, job.InvocationID, &res) {
			return
		}
		done := running
		done.Phase = PhaseFailed
		done.LastError = result.Error
		done.ExitCode = result.ExitCode
		done.Result = &res
		if err := w.q.CommitTerminal(ctx, job.InvocationID, done, nil); err != nil {
			log.Printf("[async] no se pudo marcar el fallo %s: %v", job.InvocationID, err)
			return
		}
		_ = w.q.Publish(ctx, res)
		_ = w.q.Ack(ctx, d.ID)
	default:
		if result.Error == "" {
			result.Error = "la ejecución falló"
		}
		if attempt >= job.MaxAttempts {
			w.dead(ctx, d, job, running, attempt, result.Error, result.ExitCode)
			return
		}
		next := job
		next.Attempt = attempt + 1
		delay := w.q.RetryDelay(attempt)
		retryState := State{
			Phase:        PhaseRetrying,
			Attempt:      attempt + 1,
			LastError:    result.Error,
			ExitCode:     result.ExitCode,
			EnqueuedAt:   running.EnqueuedAt,
			AttemptTimes: times,
		}
		if err := w.q.ScheduleRetry(ctx, next, delay, retryState); err != nil {
			log.Printf("[async] no se pudo programar el reintento %s: %v", job.InvocationID, err)
			return
		}
		_ = w.q.Publish(ctx, Result{
			InvocationID: job.InvocationID,
			ExecutionID:  meta.ExecutionID,
			Status:       "RUNNING",
			Error:        result.Error,
			ExitCode:     result.ExitCode,
			Attempts:     attempt,
			At:           w.q.Now().UnixMilli(),
		})
		_ = w.q.Ack(ctx, d.ID)
		log.Printf("[async] reintento %s intento=%d espera=%s", job.InvocationID, attempt+1, delay)
	}
}

func (w *Worker) dead(ctx context.Context, d Delivery, job Job, st State, attempts int, lastError string, exitCode int) {
	meta := peekBody(job.Body)
	now := w.q.Now().UnixMilli()
	if attempts < 1 {
		attempts = st.Attempt
	}
	status := "ERROR"
	if exitCode == 124 {
		status = "TIMEOUT"
	}
	result := Result{
		InvocationID: job.InvocationID,
		ExecutionID:  meta.ExecutionID,
		Status:       status,
		Error:        lastError,
		ExitCode:     exitCode,
		Attempts:     attempts,
		At:           now,
	}
	if !w.seal(ctx, job.InvocationID, &result) {
		return
	}
	rawJob, err := json.Marshal(job)
	if err != nil {
		log.Printf("[async] payload DLQ %s: %v", job.InvocationID, err)
		return
	}
	failedAt := now
	if n := len(st.AttemptTimes); n > 0 {
		failedAt = st.AttemptTimes[n-1]
	}
	entry := DLQEntry{
		ID:           job.InvocationID,
		InvocationID: job.InvocationID,
		ExecutionID:  meta.ExecutionID,
		FunctionID:   meta.FunctionID,
		Version:      meta.Version,
		Event:        meta.Event,
		Payload:      rawJob,
		LastError:    lastError,
		ExitCode:     exitCode,
		Attempts:     attempts,
		MaxAttempts:  job.MaxAttempts,
		EnqueuedAt:   firstNonZero(st.EnqueuedAt, job.EnqueuedAt),
		AttemptTimes: st.AttemptTimes,
		FailedAt:     failedAt,
		DeadAt:       now,
	}
	done := st
	done.Phase = PhaseDead
	done.Attempt = attempts
	done.LastError = lastError
	done.ExitCode = exitCode
	done.Result = &result
	if err := w.q.CommitTerminal(ctx, job.InvocationID, done, &entry); err != nil {
		log.Printf("[async] DLQ %s: %v", job.InvocationID, err)
		return
	}
	_ = w.q.Publish(ctx, result)
	_ = w.q.Ack(ctx, d.ID)
	log.Printf("[async] DLQ %s intentos=%d", job.InvocationID, attempts)
}

func (w *Worker) seal(ctx context.Context, invocationID string, result *Result) bool {
	if result.Seq != 0 {
		return true
	}
	seq, err := w.q.NextSeq(ctx, invocationID)
	if err != nil {
		log.Printf("[async] secuencia %s: %v", invocationID, err)
		return false
	}
	result.Seq = seq
	return true
}

func (w *Worker) finishTerminal(ctx context.Context, d Delivery, st State) {
	if st.Result != nil {
		_ = w.q.Publish(ctx, *st.Result)
	}
	_ = w.q.Ack(ctx, d.ID)
}

func (w *Worker) terminal(job Job, meta bodyMeta, status string, attempts int, result RunResult) Result {
	return Result{
		InvocationID: job.InvocationID,
		ExecutionID:  meta.ExecutionID,
		Status:       status,
		Output:       result.Output,
		Error:        result.Error,
		ExitCode:     result.ExitCode,
		DurationMs:   result.DurationMs,
		Attempts:     attempts,
		ColdStart:    result.ColdStart,
		InitMs:       result.InitMs,
		QueuedMs:     result.QueuedMs,
		At:           w.q.Now().UnixMilli(),
	}
}

func (w *Worker) refresh(ctx context.Context, invocationID, token string) {
	interval := w.cfg.LockTTL / 3
	if interval < 20*time.Millisecond {
		interval = 20 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := w.q.RefreshLock(context.Background(), invocationID, token); err != nil {
				log.Printf("[async] no se pudo renovar el lock %s: %v", invocationID, err)
			}
		}
	}
}

func isTerminal(phase string) bool {
	return phase == PhaseSucceeded || phase == PhaseFailed || phase == PhaseDead
}

func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func firstNonZero(a, b int64) int64 {
	if a != 0 {
		return a
	}
	return b
}
