package async

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

const (
	testJobsURL = "http://localstack:4566/000000000000/nimbus-async-jobs"
	testDLQURL  = "http://localstack:4566/000000000000/nimbus-async-dlq"
)

func TestSecondsCeil(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want int32
	}{
		{0, 0},
		{-time.Millisecond, 0},
		{time.Millisecond, 1},
		{400 * time.Millisecond, 1},
		{1500 * time.Millisecond, 2},
		{20 * time.Second, 20},
		{13 * time.Hour, 12 * 60 * 60},
	}
	for _, tc := range cases {
		if got := secondsCeil(tc.in); got != tc.want {
			t.Errorf("secondsCeil(%s)=%d want %d", tc.in, got, tc.want)
		}
	}
	if visibilitySeconds(0) != 30 {
		t.Fatalf("zero claim idle = %d", visibilitySeconds(0))
	}
}

func TestAlignQueueURL(t *testing.T) {
	got := alignQueueURL("http://localhost:4566/000000000000/nimbus-async-jobs", "http://localstack:4566")
	if got != testJobsURL {
		t.Fatalf("aligned = %s", got)
	}
	if alignQueueURL(testJobsURL, "") != testJobsURL {
		t.Fatal("empty endpoint should keep the queue url")
	}
}

func TestParseBackend(t *testing.T) {
	name, err := ParseBackend("")
	if err != nil || name != "redis" {
		t.Fatalf("empty = %s %v", name, err)
	}
	name, err = ParseBackend("SQS")
	if err != nil || name != "sqs" {
		t.Fatalf("sqs = %s %v", name, err)
	}
	if _, err := ParseBackend("kafka"); err == nil {
		t.Fatal("expected error")
	}
}

func TestLoadSQSSettingsIgnoresS3Endpoint(t *testing.T) {
	t.Setenv("S3_ENDPOINT", "http://localstack:4566")
	t.Setenv("SQS_ENDPOINT", "")
	t.Setenv("SQS_QUEUE_URL", "")
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	settings := LoadSQSSettings()
	if settings.Endpoint != "" {
		t.Fatalf("SQS endpoint = %q, empty means real AWS SQS", settings.Endpoint)
	}
	if settings.QueueName != defaultQueueName || settings.DLQName != defaultDLQName {
		t.Fatalf("names %+v", settings)
	}
}

func TestSQSEnqueueTraceVisibilityDLQAndRedrive(t *testing.T) {
	tp := sdktrace.NewTracerProvider()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	ctx, span := tp.Tracer("test").Start(context.Background(), "nimbus.async.enqueue")
	defer span.End()

	q, fake := newTestSQS(t, Config{
		MaxAttempts:   3,
		BackoffBase:   400 * time.Millisecond,
		BackoffMax:    5 * time.Second,
		BackoffJitter: 0,
		ClaimIdle:     1500 * time.Millisecond,
		LockTTL:       time.Hour,
	})
	if _, dup, err := q.Enqueue(ctx, "inv-1", "exec-1", jobBody(t, "exec-1")); err != nil || dup {
		t.Fatalf("enqueue dup=%v err=%v", dup, err)
	}
	if _, dup, err := q.Enqueue(ctx, "inv-1", "exec-other", jobBody(t, "exec-other")); err != nil || !dup {
		t.Fatalf("duplicate dup=%v err=%v", dup, err)
	}
	if n := fake.count(testJobsURL); n != 1 {
		t.Fatalf("jobs=%d", n)
	}
	msg := fake.snapshot(testJobsURL)[0]
	attr := msg.attrs["traceparent"]
	if attr.StringValue == nil || !strings.Contains(*attr.StringValue, span.SpanContext().TraceID().String()) {
		t.Fatalf("traceparent attribute = %+v", attr.StringValue)
	}

	var calls int
	worker := NewWorker(q, func(context.Context, json.RawMessage) RunResult {
		calls++
		return RunResult{Kind: KindRetry, Error: "boom", ExitCode: 1, DurationMs: 42}
	})
	for attempt := 1; attempt <= 3; attempt++ {
		msgs, err := q.Read(context.Background(), 5, -1)
		if err != nil || len(msgs) != 1 {
			t.Fatalf("attempt %d read %+v err=%v", attempt, msgs, err)
		}
		if msgs[0].ReceiveCount != attempt {
			t.Fatalf("attempt %d receiveCount=%d", attempt, msgs[0].ReceiveCount)
		}
		if !strings.Contains(msgs[0].Job.TraceParent, span.SpanContext().TraceID().String()) {
			t.Fatalf("delivery lost traceparent %q", msgs[0].Job.TraceParent)
		}
		worker.Handle(context.Background(), msgs[0])
	}
	if calls != 3 {
		t.Fatalf("calls=%d", calls)
	}
	if fake.count(testJobsURL) != 0 || fake.count(testDLQURL) != 1 {
		t.Fatalf("jobs=%d dlq=%d", fake.count(testJobsURL), fake.count(testDLQURL))
	}
	var backoff []int32
	for _, v := range fake.visibilityTimeouts() {
		if v > 0 {
			backoff = append(backoff, v)
		}
	}
	// 2s is the claim-idle hide applied on each receive. 1s is the backoff.
	if len(backoff) != 5 || backoff[0] != 2 || backoff[1] != 1 || backoff[2] != 2 || backoff[3] != 1 || backoff[4] != 2 {
		t.Fatalf("visibility seconds = %v, want [2 1 2 1 2]", backoff)
	}

	list, err := q.ListDLQ(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("list %+v", list)
	}
	entry := list[0]
	if entry.Attempts != 3 || entry.LastError != "boom" || entry.DurationMs != 42 || entry.ExitCode != 1 {
		t.Fatalf("entry %+v", entry)
	}
	if entry.ExecutionID != "exec-1" || entry.FunctionID != "fn-1" || len(entry.Payload) == 0 || !strings.Contains(string(entry.Payload), "inv-1") {
		t.Fatalf("payload %+v", entry)
	}
	if len(entry.AttemptTimes) != 3 {
		t.Fatalf("attemptTimes %+v", entry.AttemptTimes)
	}

	redriven, err := q.Redrive(context.Background(), "inv-1")
	if err != nil {
		t.Fatal(err)
	}
	if redriven.InvocationID != "inv-1" || redriven.LastError != "boom" {
		t.Fatalf("redrive %+v", redriven)
	}
	if fake.count(testDLQURL) != 0 || fake.count(testJobsURL) != 1 {
		t.Fatalf("after redrive jobs=%d dlq=%d", fake.count(testJobsURL), fake.count(testDLQURL))
	}
	again, err := q.Read(context.Background(), 1, -1)
	if err != nil || len(again) != 1 || again[0].ReceiveCount != 1 {
		t.Fatalf("redriven delivery %+v err=%v", again, err)
	}
	if _, err := q.Redrive(context.Background(), "missing"); err != ErrNotFound {
		t.Fatalf("missing redrive err=%v", err)
	}
}

func TestSQSValidationAndSuccessDeleteTheMessage(t *testing.T) {
	q, fake := newTestSQS(t, Config{MaxAttempts: 3, LockTTL: time.Hour, ClaimIdle: time.Second})
	ctx := context.Background()
	if _, _, err := q.Enqueue(ctx, "inv-bad", "exec-bad", jobBody(t, "exec-bad")); err != nil {
		t.Fatal(err)
	}
	msgs, err := q.Read(ctx, 1, -1)
	if err != nil || len(msgs) != 1 {
		t.Fatal(err)
	}
	NewWorker(q, func(context.Context, json.RawMessage) RunResult {
		return RunResult{Kind: KindValidation, Error: "timeoutSec", ExitCode: 400}
	}).Handle(ctx, msgs[0])
	if fake.count(testJobsURL) != 0 || fake.count(testDLQURL) != 0 {
		t.Fatalf("validation jobs=%d dlq=%d", fake.count(testJobsURL), fake.count(testDLQURL))
	}

	if _, _, err := q.Enqueue(ctx, "inv-ok", "exec-ok", jobBody(t, "exec-ok")); err != nil {
		t.Fatal(err)
	}
	msgs, err = q.Read(ctx, 1, -1)
	if err != nil || len(msgs) != 1 {
		t.Fatal(err)
	}
	NewWorker(q, func(context.Context, json.RawMessage) RunResult {
		return RunResult{Kind: KindOK, Output: map[string]any{"ok": true}, DurationMs: 5}
	}).Handle(ctx, msgs[0])
	if fake.count(testJobsURL) != 0 || fake.count(testDLQURL) != 0 {
		t.Fatalf("success jobs=%d dlq=%d", fake.count(testJobsURL), fake.count(testDLQURL))
	}
	list, err := q.ListDLQ(ctx)
	if err != nil || len(list) != 0 {
		t.Fatalf("list %+v err=%v", list, err)
	}
}

func TestSQSCancelDoesNotEnterDLQ(t *testing.T) {
	q, fake := newTestSQS(t, Config{MaxAttempts: 3, LockTTL: time.Hour, ClaimIdle: time.Second})
	ctx := context.Background()
	if _, _, err := q.Enqueue(ctx, "inv-c", "exec-c", jobBody(t, "exec-c")); err != nil {
		t.Fatal(err)
	}
	msgs, err := q.Read(ctx, 1, -1)
	if err != nil || len(msgs) != 1 {
		t.Fatal(err)
	}
	NewWorker(q, func(context.Context, json.RawMessage) RunResult {
		return RunResult{Kind: KindFatal, Error: "Ejecución cancelada", ExitCode: 130, DurationMs: 12}
	}).Handle(ctx, msgs[0])
	if fake.count(testJobsURL) != 0 || fake.count(testDLQURL) != 0 {
		t.Fatalf("cancel jobs=%d dlq=%d", fake.count(testJobsURL), fake.count(testDLQURL))
	}
}

func newTestSQS(t *testing.T, cfg Config) (*SQSQueue, *fakeSQS) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	if cfg.MaxAttempts == 0 {
		cfg.MaxAttempts = 3
	}
	fake := newFakeSQS(testJobsURL, testDLQURL, cfg.MaxAttempts)
	q := &SQSQueue{
		Queue:   New(rdb, cfg),
		api:     fake,
		jobsURL: testJobsURL,
		dlqURL:  testDLQURL,
		pollMu:  &sync.Mutex{},
	}
	q.rnd = func() float64 { return 0 }
	return q, fake
}

type fakeMsg struct {
	id        string
	receipt   string
	body      string
	attrs     map[string]types.MessageAttributeValue
	receives  int
	visibleAt time.Time
}

type fakeSQS struct {
	mu         sync.Mutex
	jobsURL    string
	dlqURL     string
	jobs       []*fakeMsg
	dlq        []*fakeMsg
	maxReceive int
	seq        int
	vis        []int32
	immediate  bool
}

func newFakeSQS(jobsURL, dlqURL string, maxReceive int) *fakeSQS {
	return &fakeSQS{jobsURL: jobsURL, dlqURL: dlqURL, maxReceive: maxReceive, immediate: true}
}

func (f *fakeSQS) count(queueURL string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(*f.bucket(queueURL))
}

func (f *fakeSQS) snapshot(queueURL string) []fakeMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	src := *f.bucket(queueURL)
	out := make([]fakeMsg, len(src))
	for i, m := range src {
		out[i] = *m
	}
	return out
}

func (f *fakeSQS) visibilityTimeouts() []int32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]int32(nil), f.vis...)
	return out
}

func (f *fakeSQS) bucket(queueURL string) *[]*fakeMsg {
	if queueURL == f.dlqURL {
		return &f.dlq
	}
	return &f.jobs
}

func (f *fakeSQS) SendMessage(_ context.Context, params *sqs.SendMessageInput, _ ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	msg := &fakeMsg{
		id:    fmt.Sprintf("m-%d", f.seq),
		body:  aws.ToString(params.MessageBody),
		attrs: params.MessageAttributes,
	}
	bucket := f.bucket(aws.ToString(params.QueueUrl))
	*bucket = append(*bucket, msg)
	return &sqs.SendMessageOutput{MessageId: aws.String(msg.id)}, nil
}

func (f *fakeSQS) ReceiveMessage(_ context.Context, params *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	queueURL := aws.ToString(params.QueueUrl)
	bucket := f.bucket(queueURL)
	max := int(params.MaxNumberOfMessages)
	if max < 1 {
		max = 1
	}
	now := time.Now()
	var kept []*fakeMsg
	var out []types.Message
	for _, m := range *bucket {
		if len(out) >= max || (!f.immediate && now.Before(m.visibleAt)) {
			kept = append(kept, m)
			continue
		}
		next := m.receives + 1
		if queueURL == f.jobsURL && f.maxReceive > 0 && next > f.maxReceive {
			cp := *m
			cp.receives = next
			cp.visibleAt = time.Time{}
			f.dlq = append(f.dlq, &cp)
			continue
		}
		f.seq++
		m.receives = next
		m.receipt = fmt.Sprintf("rh-%d", f.seq)
		if !f.immediate && params.VisibilityTimeout > 0 {
			m.visibleAt = now.Add(time.Duration(params.VisibilityTimeout) * time.Second)
		}
		kept = append(kept, m)
		out = append(out, f.toAWS(m))
	}
	*bucket = kept
	return &sqs.ReceiveMessageOutput{Messages: out}, nil
}

func (f *fakeSQS) DeleteMessage(_ context.Context, params *sqs.DeleteMessageInput, _ ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	bucket := f.bucket(aws.ToString(params.QueueUrl))
	receipt := aws.ToString(params.ReceiptHandle)
	next := (*bucket)[:0]
	found := false
	for _, m := range *bucket {
		if m.receipt == receipt {
			found = true
			continue
		}
		next = append(next, m)
	}
	*bucket = next
	if !found {
		return nil, fmt.Errorf("receipt not found")
	}
	return &sqs.DeleteMessageOutput{}, nil
}

func (f *fakeSQS) ChangeMessageVisibility(_ context.Context, params *sqs.ChangeMessageVisibilityInput, _ ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vis = append(f.vis, params.VisibilityTimeout)
	receipt := aws.ToString(params.ReceiptHandle)
	for _, bucket := range []*[]*fakeMsg{&f.jobs, &f.dlq} {
		for _, m := range *bucket {
			if m.receipt != receipt {
				continue
			}
			if !f.immediate && params.VisibilityTimeout > 0 {
				m.visibleAt = time.Now().Add(time.Duration(params.VisibilityTimeout) * time.Second)
			} else {
				m.visibleAt = time.Time{}
			}
			return &sqs.ChangeMessageVisibilityOutput{}, nil
		}
	}
	return &sqs.ChangeMessageVisibilityOutput{}, nil
}

func (f *fakeSQS) GetQueueAttributes(_ context.Context, params *sqs.GetQueueAttributesInput, _ ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	visible, hidden := 0, 0
	for _, m := range *f.bucket(aws.ToString(params.QueueUrl)) {
		if !f.immediate && now.Before(m.visibleAt) {
			hidden++
		} else {
			visible++
		}
	}
	return &sqs.GetQueueAttributesOutput{Attributes: map[string]string{
		"ApproximateNumberOfMessages":           strconv.Itoa(visible),
		"ApproximateNumberOfMessagesNotVisible": strconv.Itoa(hidden),
	}}, nil
}

func (f *fakeSQS) GetQueueUrl(_ context.Context, params *sqs.GetQueueUrlInput, _ ...func(*sqs.Options)) (*sqs.GetQueueUrlOutput, error) {
	name := aws.ToString(params.QueueName)
	switch name {
	case defaultQueueName:
		return &sqs.GetQueueUrlOutput{QueueUrl: aws.String(f.jobsURL)}, nil
	case defaultDLQName:
		return &sqs.GetQueueUrlOutput{QueueUrl: aws.String(f.dlqURL)}, nil
	default:
		return nil, fmt.Errorf("unknown queue %s", name)
	}
}

func (f *fakeSQS) toAWS(m *fakeMsg) types.Message {
	return types.Message{
		MessageId:         aws.String(m.id),
		ReceiptHandle:     aws.String(m.receipt),
		Body:              aws.String(m.body),
		MessageAttributes: m.attrs,
		Attributes: map[string]string{
			"ApproximateReceiveCount": strconv.Itoa(m.receives),
		},
	}
}

var _ sqsAPI = (*fakeSQS)(nil)
