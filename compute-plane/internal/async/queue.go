package async

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"time"

	ntrace "github.com/PedroCarassale/nimbus/compute-plane/internal/trace"
	"github.com/redis/go-redis/v9"
)

// ErrNotFound is returned when a DLQ id is not waiting to be redriven.
var ErrNotFound = errors.New("dlq entry not found")

// Queue is the Redis side of async invoke: stream, retry set, lock, DLQ.
type Queue struct {
	rdb      *redis.Client
	cfg      Config
	consumer string
	now      func() time.Time
	rnd      func() float64
}

func New(rdb *redis.Client, cfg Config) *Queue {
	host, _ := os.Hostname()
	if host == "" {
		host = "worker"
	}
	return &Queue{
		rdb:      rdb,
		cfg:      cfg,
		consumer: fmt.Sprintf("%s-%d", host, os.Getpid()),
		now:      time.Now,
		rnd:      rand.Float64,
	}
}

// WithConsumer returns a copy that claims messages under another name.
// Restarts use a new name so pending entries are picked up by XAUTOCLAIM.
func (q *Queue) WithConsumer(name string) *Queue {
	cp := *q
	cp.consumer = name
	return &cp
}

func (q *Queue) Config() Config { return q.cfg }

func (q *Queue) Now() time.Time {
	if q.now == nil {
		return time.Now()
	}
	return q.now()
}

func (q *Queue) EnsureGroup(ctx context.Context) error {
	err := q.rdb.XGroupCreateMkStream(ctx, streamJobs, groupWorkers, "$").Err()
	if err == nil || isBusyGroup(err) {
		return nil
	}
	return err
}

// Enqueue adds a job once. A repeated invocation id returns the original
// execution id and duplicate=true without a second stream entry.
func (q *Queue) Enqueue(ctx context.Context, invocationID, executionID string, body json.RawMessage) (string, bool, error) {
	now := q.Now().UnixMilli()
	maxAttempts := q.cfg.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = DefaultMaxAttempts
	}
	job := Job{
		InvocationID: invocationID,
		Attempt:      1,
		MaxAttempts:  maxAttempts,
		EnqueuedAt:   now,
		Body:         body,
		TraceParent:  ntrace.Header(ctx),
		TraceState:   ntrace.State(ctx),
	}
	jobJSON, err := json.Marshal(job)
	if err != nil {
		return "", false, err
	}
	stJSON, err := json.Marshal(State{Phase: PhaseQueued, EnqueuedAt: now})
	if err != nil {
		return "", false, err
	}
	meta := peekBody(body)
	res, err := enqueueScript.Run(ctx, q.rdb, []string{
		acceptKey(invocationID),
		streamJobs,
		stateKey(invocationID),
		execKey(executionID),
	}, executionID, string(jobJSON), string(stJSON), meta.FunctionID, now, pendingExecTTL.Milliseconds()).Result()
	if err != nil {
		return "", false, err
	}
	status, stored, err := twoStrings(res)
	if err != nil {
		return "", false, err
	}
	return stored, status == "duplicate", nil
}

// Read takes new messages for this consumer. block < 0 does not wait.
func (q *Queue) Read(ctx context.Context, count int, block time.Duration) ([]Delivery, error) {
	if count < 1 {
		count = 1
	}
	args := &redis.XReadGroupArgs{
		Group:    groupWorkers,
		Consumer: q.consumer,
		Streams:  []string{streamJobs, ">"},
		Count:    int64(count),
		Block:    block,
	}
	streams, err := q.rdb.XReadGroup(ctx, args).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parseStreams(streams), nil
}

// Claim takes messages that have been pending longer than the visibility
// timeout, including ones left by a consumer that died.
func (q *Queue) Claim(ctx context.Context, count int) ([]Delivery, error) {
	if count < 1 {
		count = 1
	}
	msgs, _, err := q.rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream:   streamJobs,
		Group:    groupWorkers,
		Consumer: q.consumer,
		MinIdle:  q.cfg.ClaimIdle,
		Start:    "0-0",
		Count:    int64(count),
	}).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parseMessages(msgs), nil
}

func (q *Queue) Ack(ctx context.Context, d Delivery) error {
	if d.ID == "" {
		return nil
	}
	return q.rdb.XAck(ctx, streamJobs, groupWorkers, d.ID).Err()
}

func (q *Queue) Consumer() string { return q.consumer }

// Fork returns a Redis consumer with its own name. SQS overrides this so a
// forked worker keeps the SQS client.
func (q *Queue) Fork(name string) Backend { return q.WithConsumer(name) }

// Extend is a no-op on Redis. SQS refreshes the visibility timeout here.
func (q *Queue) Extend(context.Context, Delivery) error { return nil }

// Abandon leaves the Redis stream entry pending. The next claim takes it.
func (q *Queue) Abandon(context.Context, Delivery) error { return nil }

// Postpone records the retry and acks the stream entry. SQS changes
// visibility instead of acking, so the same message is received again.
func (q *Queue) Postpone(ctx context.Context, d Delivery, job Job, delay time.Duration, st State) error {
	if err := q.ScheduleRetry(ctx, job, delay, st); err != nil {
		return err
	}
	return q.Ack(ctx, d)
}

// SealFailed stores a terminal state that must not enter the DLQ (validation
// or an explicit cancel) and acks the delivery.
func (q *Queue) SealFailed(ctx context.Context, d Delivery, st State) error {
	if err := q.CommitTerminal(ctx, d.Job.InvocationID, st, nil); err != nil {
		return err
	}
	return q.Ack(ctx, d)
}

// SealDead stores the DLQ entry and acks the stream message. SQS leaves the
// message in place so the queue redrive policy can move it.
func (q *Queue) SealDead(ctx context.Context, d Delivery, st State, entry DLQEntry) error {
	if err := q.CommitTerminal(ctx, d.Job.InvocationID, st, &entry); err != nil {
		return err
	}
	return q.Ack(ctx, d)
}

// Reserve records the invocation as accepted without publishing it. The SQS
// backend sends the message itself. A duplicate invocation returns the
// original execution id.
func (q *Queue) Reserve(ctx context.Context, invocationID, executionID string, job Job) (string, bool, error) {
	jobJSON, err := json.Marshal(job)
	if err != nil {
		return "", false, err
	}
	stJSON, err := json.Marshal(State{Phase: PhaseQueued, EnqueuedAt: job.EnqueuedAt})
	if err != nil {
		return "", false, err
	}
	meta := peekBody(job.Body)
	res, err := reserveScript.Run(ctx, q.rdb, []string{
		acceptKey(invocationID),
		stateKey(invocationID),
		execKey(executionID),
	}, executionID, string(jobJSON), string(stJSON), meta.FunctionID, job.EnqueuedAt, pendingExecTTL.Milliseconds()).Result()
	if err != nil {
		return "", false, err
	}
	status, stored, err := twoStrings(res)
	if err != nil {
		return "", false, err
	}
	return stored, status == "duplicate", nil
}

// ReleaseAccept forgets an accept that never made it onto the transport.
func (q *Queue) ReleaseAccept(ctx context.Context, invocationID string) error {
	return q.rdb.Del(ctx, acceptKey(invocationID)).Err()
}

// TouchPending recreates the cancel hash when a redrive finds it expired.
func (q *Queue) TouchPending(ctx context.Context, executionID, functionID string) error {
	if executionID == "" {
		return nil
	}
	key := execKey(executionID)
	n, err := q.rdb.Exists(ctx, key).Result()
	if err != nil || n > 0 {
		return err
	}
	if err := q.rdb.HSet(ctx, key,
		"functionId", functionID,
		"status", "pending",
		"cancelled", "false",
		"heartbeat", q.Now().UnixMilli(),
	).Err(); err != nil {
		return err
	}
	return q.rdb.PExpire(ctx, key, pendingExecTTL).Err()
}

// PromoteDue moves due retries back onto the job stream.
func (q *Queue) PromoteDue(ctx context.Context, limit int) (int, error) {
	if limit < 1 {
		limit = 32
	}
	res, err := promoteScript.Run(ctx, q.rdb, []string{retryKey, streamJobs},
		q.Now().UnixMilli(), limit).Result()
	if err != nil {
		return 0, err
	}
	n, err := asInt(res)
	if err != nil {
		return 0, err
	}
	return n, nil
}

func (q *Queue) LoadState(ctx context.Context, invocationID string) (State, error) {
	raw, err := q.rdb.Get(ctx, stateKey(invocationID)).Result()
	if err == redis.Nil {
		return State{}, nil
	}
	if err != nil {
		return State{}, err
	}
	var st State
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return State{}, err
	}
	return st, nil
}

func (q *Queue) SaveState(ctx context.Context, invocationID string, st State) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return q.rdb.Set(ctx, stateKey(invocationID), raw, 0).Err()
}

// TryLock is the single-flight guard. True means this token owns the run.
func (q *Queue) TryLock(ctx context.Context, invocationID, token string) (bool, error) {
	ok, err := q.rdb.SetNX(ctx, lockKey(invocationID), token, q.cfg.LockTTL).Result()
	if err != nil {
		return false, err
	}
	return ok, nil
}

func (q *Queue) RefreshLock(ctx context.Context, invocationID, token string) error {
	ttl := q.cfg.LockTTL
	if ttl < time.Millisecond {
		ttl = time.Millisecond
	}
	_, err := refreshLockScript.Run(ctx, q.rdb, []string{lockKey(invocationID)},
		token, ttl.Milliseconds()).Result()
	return err
}

func (q *Queue) Unlock(ctx context.Context, invocationID, token string) error {
	_, err := unlockScript.Run(ctx, q.rdb, []string{lockKey(invocationID)}, token).Result()
	return err
}

// ScheduleRetry records the next attempt and the moment it becomes due.
func (q *Queue) ScheduleRetry(ctx context.Context, job Job, delay time.Duration, st State) error {
	rawJob, err := json.Marshal(job)
	if err != nil {
		return err
	}
	rawState, err := json.Marshal(st)
	if err != nil {
		return err
	}
	due := q.Now().Add(delay).UnixMilli()
	meta := peekBody(job.Body)
	return scheduleScript.Run(ctx, q.rdb, []string{retryKey, stateKey(job.InvocationID), execKey(meta.ExecutionID)},
		due, string(rawJob), string(rawState), meta.FunctionID, q.Now().UnixMilli(), pendingExecTTL.Milliseconds()).Err()
}

// NextSeq reserves the next result sequence for one invocation.
func (q *Queue) NextSeq(ctx context.Context, invocationID string) (int64, error) {
	return q.rdb.Incr(ctx, seqKey(invocationID)).Result()
}

func (q *Queue) Publish(ctx context.Context, result Result) error {
	if result.At == 0 {
		result.At = q.Now().UnixMilli()
	}
	if result.Seq == 0 {
		seq, err := q.NextSeq(ctx, result.InvocationID)
		if err != nil {
			return err
		}
		result.Seq = seq
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return q.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: streamResults,
		MaxLen: 10000,
		Approx: true,
		Values: map[string]interface{}{"data": string(raw)},
	}).Err()
}

// CommitTerminal stores a finished state. When entry is non-nil the invocation
// is also appended to the DLQ in the same Redis round.
func (q *Queue) CommitTerminal(ctx context.Context, invocationID string, st State, entry *DLQEntry) error {
	rawState, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if entry == nil {
		return q.rdb.Set(ctx, stateKey(invocationID), rawState, 0).Err()
	}
	rawEntry, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	return deadScript.Run(ctx, q.rdb, []string{
		dlqItemsKey,
		dlqIndexKey,
		streamDLQ,
		stateKey(invocationID),
	}, entry.ID, string(rawEntry), string(rawState)).Err()
}

// DLQLen is the number of invocations waiting in the dead-letter hash.
func (q *Queue) DLQLen(ctx context.Context) (int64, error) {
	return q.rdb.HLen(ctx, dlqItemsKey).Result()
}

func (q *Queue) ListDLQ(ctx context.Context) ([]DLQEntry, error) {
	raw, err := q.rdb.HGetAll(ctx, dlqItemsKey).Result()
	if err != nil {
		return nil, err
	}
	entries := make([]DLQEntry, 0, len(raw))
	for _, item := range raw {
		var entry DLQEntry
		if err := json.Unmarshal([]byte(item), &entry); err != nil {
			continue
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].DeadAt == entries[j].DeadAt {
			return entries[i].ID > entries[j].ID
		}
		return entries[i].DeadAt > entries[j].DeadAt
	})
	if len(entries) > 200 {
		entries = entries[:200]
	}
	return entries, nil
}

// Redrive puts one DLQ entry back on the job stream as attempt 1.
func (q *Queue) Redrive(ctx context.Context, id string) (DLQEntry, error) {
	rawExisting, err := q.rdb.HGet(ctx, dlqItemsKey, id).Result()
	if err == redis.Nil {
		return DLQEntry{}, ErrNotFound
	}
	if err != nil {
		return DLQEntry{}, err
	}
	var entry DLQEntry
	if err := json.Unmarshal([]byte(rawExisting), &entry); err != nil {
		return DLQEntry{}, err
	}
	var job Job
	if err := json.Unmarshal(entry.Payload, &job); err != nil {
		return DLQEntry{}, err
	}
	maxAttempts := job.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = q.cfg.MaxAttempts
	}
	job.Attempt = 1
	job.MaxAttempts = maxAttempts
	jobJSON, err := json.Marshal(job)
	if err != nil {
		return DLQEntry{}, err
	}
	now := q.Now().UnixMilli()
	stJSON, err := json.Marshal(State{Phase: PhaseQueued, EnqueuedAt: job.EnqueuedAt})
	if err != nil {
		return DLQEntry{}, err
	}
	res, err := redriveScript.Run(ctx, q.rdb, []string{
		dlqItemsKey,
		dlqIndexKey,
		streamDLQ,
		streamJobs,
		stateKey(id),
		execKey(entry.ExecutionID),
	}, id, string(jobJSON), string(stJSON), entry.FunctionID, now, pendingExecTTL.Milliseconds()).Result()
	if err != nil {
		return DLQEntry{}, err
	}
	if res == nil || fmt.Sprint(res) == "" || fmt.Sprint(res) == "0" || fmt.Sprint(res) == "false" {
		return DLQEntry{}, ErrNotFound
	}
	meta := peekBody(job.Body)
	if err := q.Publish(ctx, Result{
		InvocationID: job.InvocationID,
		ExecutionID:  meta.ExecutionID,
		Status:       "PENDING",
		Attempts:     0,
		Redrive:      true,
		At:           now,
	}); err != nil {
		return DLQEntry{}, err
	}
	return entry, nil
}

func (q *Queue) RetryDelay(attempt int) time.Duration {
	unit := 0.0
	if q.rnd != nil {
		unit = q.rnd()
	}
	return Backoff(attempt, q.cfg.BackoffBase, q.cfg.BackoffJitter, q.cfg.BackoffMax, unit)
}

func isBusyGroup(err error) bool {
	return err != nil && strings.Contains(err.Error(), "BUSYGROUP")
}

func twoStrings(res interface{}) (string, string, error) {
	arr, ok := res.([]interface{})
	if !ok || len(arr) < 2 {
		return "", "", fmt.Errorf("respuesta de script inesperada: %T", res)
	}
	return fmt.Sprint(arr[0]), fmt.Sprint(arr[1]), nil
}

func asInt(res interface{}) (int, error) {
	switch v := res.(type) {
	case int64:
		return int(v), nil
	case int:
		return v, nil
	case string:
		var n int
		_, err := fmt.Sscan(v, &n)
		return n, err
	default:
		var n int
		_, err := fmt.Sscan(fmt.Sprint(v), &n)
		return n, err
	}
}

func parseStreams(streams []redis.XStream) []Delivery {
	var out []Delivery
	for _, stream := range streams {
		out = append(out, parseMessages(stream.Messages)...)
	}
	return out
}

func parseMessages(msgs []redis.XMessage) []Delivery {
	out := make([]Delivery, 0, len(msgs))
	for _, msg := range msgs {
		raw, ok := msg.Values["data"]
		if !ok {
			continue
		}
		var job Job
		if err := json.Unmarshal([]byte(fmt.Sprint(raw)), &job); err != nil {
			continue
		}
		out = append(out, Delivery{ID: msg.ID, Job: job})
	}
	return out
}

type bodyMeta struct {
	ExecutionID string          `json:"executionId"`
	FunctionID  string          `json:"functionId"`
	Version     string          `json:"version"`
	Event       json.RawMessage `json:"event"`
}

func peekBody(body json.RawMessage) bodyMeta {
	var meta bodyMeta
	_ = json.Unmarshal(body, &meta)
	return meta
}

// pendingExecTTL keeps the cancel flag alive while the job sits on the
// stream. The concurrency hash created at admit time expires with the slot
// lease, which is shorter than a backlog of async work.
const pendingExecTTL = 24 * time.Hour

// execKey matches concurrency.Manager. A worker cannot have started before
// this script returns, so writing the hash here lets POST /executions/:id/cancel
// see a job that is still waiting for a worker.
func execKey(executionID string) string { return "nimbus:exec:" + executionID }

const reserveLua = `
local existing = redis.call('GET', KEYS[1])
if existing and existing ~= false then
  return {'duplicate', existing}
end
if redis.call('EXISTS', KEYS[3]) == 0 then
  redis.call('HSET', KEYS[3],
    'functionId', ARGV[4],
    'status', 'pending',
    'cancelled', 'false',
    'heartbeat', tostring(ARGV[5]))
  redis.call('PEXPIRE', KEYS[3], ARGV[6])
end
redis.call('SET', KEYS[1], ARGV[1])
redis.call('SET', KEYS[2], ARGV[3])
return {'ok', ARGV[1]}
`

const enqueueLua = `
local existing = redis.call('GET', KEYS[1])
if existing and existing ~= false then
  return {'duplicate', existing}
end
if redis.call('EXISTS', KEYS[4]) == 0 then
  redis.call('HSET', KEYS[4],
    'functionId', ARGV[4],
    'status', 'pending',
    'cancelled', 'false',
    'heartbeat', tostring(ARGV[5]))
  redis.call('PEXPIRE', KEYS[4], ARGV[6])
end
redis.call('SET', KEYS[1], ARGV[1])
redis.call('SET', KEYS[3], ARGV[3])
redis.call('XADD', KEYS[2], '*', 'data', ARGV[2])
return {'ok', ARGV[1]}
`

const promoteLua = `
local due = redis.call('ZRANGEBYSCORE', KEYS[1], '-inf', ARGV[1], 'LIMIT', '0', ARGV[2])
local n = 0
for _, item in ipairs(due) do
  if redis.call('ZREM', KEYS[1], item) == 1 then
    redis.call('XADD', KEYS[2], '*', 'data', item)
    n = n + 1
  end
end
return n
`

const scheduleLua = `
redis.call('ZADD', KEYS[1], ARGV[1], ARGV[2])
redis.call('SET', KEYS[2], ARGV[3])
if redis.call('EXISTS', KEYS[3]) == 0 then
  redis.call('HSET', KEYS[3],
    'functionId', ARGV[4],
    'status', 'pending',
    'cancelled', 'false',
    'heartbeat', tostring(ARGV[5]))
  redis.call('PEXPIRE', KEYS[3], ARGV[6])
end
return 1
`

const refreshLockLua = `
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('PEXPIRE', KEYS[1], ARGV[2])
end
return 0
`

const unlockLua = `
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0
`

const deadLua = `
redis.call('HSET', KEYS[1], ARGV[1], ARGV[2])
local sid = redis.call('XADD', KEYS[3], '*', 'data', ARGV[2])
redis.call('HSET', KEYS[2], ARGV[1], sid)
redis.call('SET', KEYS[4], ARGV[3])
return sid
`

const redriveLua = `
local raw = redis.call('HGET', KEYS[1], ARGV[1])
if not raw or raw == false then
  return false
end
redis.call('HDEL', KEYS[1], ARGV[1])
local sid = redis.call('HGET', KEYS[2], ARGV[1])
if sid and sid ~= false then
  redis.call('XDEL', KEYS[3], sid)
  redis.call('HDEL', KEYS[2], ARGV[1])
end
redis.call('SET', KEYS[5], ARGV[3])
if redis.call('EXISTS', KEYS[6]) == 0 then
  redis.call('HSET', KEYS[6],
    'functionId', ARGV[4],
    'status', 'pending',
    'cancelled', 'false',
    'heartbeat', tostring(ARGV[5]))
  redis.call('PEXPIRE', KEYS[6], ARGV[6])
end
redis.call('XADD', KEYS[4], '*', 'data', ARGV[2])
return raw
`

var (
	reserveScript     = redis.NewScript(reserveLua)
	enqueueScript     = redis.NewScript(enqueueLua)
	promoteScript     = redis.NewScript(promoteLua)
	scheduleScript    = redis.NewScript(scheduleLua)
	refreshLockScript = redis.NewScript(refreshLockLua)
	unlockScript      = redis.NewScript(unlockLua)
	deadScript        = redis.NewScript(deadLua)
	redriveScript     = redis.NewScript(redriveLua)
)
