package async

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PedroCarassale/nimbus/compute-plane/internal/objects"
	ntrace "github.com/PedroCarassale/nimbus/compute-plane/internal/trace"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/redis/go-redis/v9"
)

const (
	defaultQueueName = "nimbus-async-jobs"
	defaultDLQName   = "nimbus-async-dlq"
)

// SQSSettings points the compute plane at Amazon SQS or at LocalStack.
// An empty Endpoint uses the public AWS endpoint for Region.
type SQSSettings struct {
	Region      string
	Endpoint    string
	AccessKey   string
	SecretKey   string
	StaticCreds bool
	QueueURL    string
	DLQURL      string
	QueueName   string
	DLQName     string
}

// LoadSQSSettings reads SQS_ENDPOINT, SQS_QUEUE_URL, SQS_DLQ_URL and the
// queue names. Credentials and region come from the same variables as S3.
func LoadSQSSettings() SQSSettings {
	base := objects.LoadConfig()
	endpoint := normalizeAWSEndpoint(os.Getenv("SQS_ENDPOINT"))
	key, secret, static := base.AccessKey, base.SecretKey, base.StaticCreds
	if endpoint != "" && !static {
		key, secret, static = "test", "test", true
	}
	name := strings.TrimSpace(os.Getenv("SQS_QUEUE_NAME"))
	if name == "" {
		name = defaultQueueName
	}
	dlqName := strings.TrimSpace(os.Getenv("SQS_DLQ_NAME"))
	if dlqName == "" {
		dlqName = defaultDLQName
	}
	return SQSSettings{
		Region:      base.Region,
		Endpoint:    endpoint,
		AccessKey:   key,
		SecretKey:   secret,
		StaticCreds: static,
		QueueURL:    strings.TrimSpace(os.Getenv("SQS_QUEUE_URL")),
		DLQURL:      strings.TrimSpace(os.Getenv("SQS_DLQ_URL")),
		QueueName:   name,
		DLQName:     dlqName,
	}
}

func normalizeAWSEndpoint(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	return strings.TrimRight(raw, "/")
}

// alignQueueURL rewrites the host of a queue URL to the configured endpoint.
// LocalStack sometimes returns a localhost URL that does not resolve inside
// the compose network.
func alignQueueURL(queueURL, endpoint string) string {
	if queueURL == "" || endpoint == "" {
		return queueURL
	}
	qu, err1 := url.Parse(queueURL)
	eu, err2 := url.Parse(endpoint)
	if err1 != nil || err2 != nil || eu.Host == "" {
		return queueURL
	}
	qu.Scheme = eu.Scheme
	qu.Host = eu.Host
	return qu.String()
}

// sqsAPI is the subset of the AWS SQS client used here. Tests substitute a fake.
type sqsAPI interface {
	SendMessage(ctx context.Context, params *sqs.SendMessageInput, optFns ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
	ReceiveMessage(ctx context.Context, params *sqs.ReceiveMessageInput, optFns ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(ctx context.Context, params *sqs.DeleteMessageInput, optFns ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
	ChangeMessageVisibility(ctx context.Context, params *sqs.ChangeMessageVisibilityInput, optFns ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error)
	GetQueueAttributes(ctx context.Context, params *sqs.GetQueueAttributesInput, optFns ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error)
	GetQueueUrl(ctx context.Context, params *sqs.GetQueueUrlInput, optFns ...func(*sqs.Options)) (*sqs.GetQueueUrlOutput, error)
}

// SQSQueue enqueues to SQS and keeps state, locks and results in Redis.
// The queue's RedrivePolicy moves a message to the DLQ after maxReceiveCount
// receives. This process does not copy the message itself.
type SQSQueue struct {
	*Queue
	api     sqsAPI
	jobsURL string
	dlqURL  string
	// pollMu keeps a single ReceiveMessage in flight per process. Some SQS
	// emulators deliver one message to every concurrent long poll and bump
	// ApproximateReceiveCount for each of them.
	pollMu *sync.Mutex
}

// NewSQS connects to SQS and resolves queue URLs when they were not set.
func NewSQS(ctx context.Context, rdb *redis.Client, cfg Config, settings SQSSettings) (*SQSQueue, error) {
	api, err := newSQSClient(ctx, settings)
	if err != nil {
		return nil, err
	}
	return newSQS(ctx, rdb, cfg, api, settings)
}

func newSQS(ctx context.Context, rdb *redis.Client, cfg Config, api sqsAPI, settings SQSSettings) (*SQSQueue, error) {
	jobs := settings.QueueURL
	dlq := settings.DLQURL
	var err error
	if jobs == "" {
		jobs, err = lookupQueueURL(ctx, api, settings.QueueName)
		if err != nil {
			return nil, fmt.Errorf("cola %s: %w", settings.QueueName, err)
		}
	}
	if dlq == "" {
		dlq, err = lookupQueueURL(ctx, api, settings.DLQName)
		if err != nil {
			return nil, fmt.Errorf("dlq %s: %w", settings.DLQName, err)
		}
	}
	return &SQSQueue{
		Queue:   New(rdb, cfg),
		api:     api,
		jobsURL: alignQueueURL(jobs, settings.Endpoint),
		dlqURL:  alignQueueURL(dlq, settings.Endpoint),
		pollMu:  &sync.Mutex{},
	}, nil
}

func (s *SQSQueue) JobsURL() string { return s.jobsURL }
func (s *SQSQueue) DLQURL() string  { return s.dlqURL }

func newSQSClient(ctx context.Context, settings SQSSettings) (*sqs.Client, error) {
	opts := []func(*config.LoadOptions) error{
		config.WithRegion(settings.Region),
	}
	if settings.StaticCreds {
		opts = append(opts, config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(settings.AccessKey, settings.SecretKey, ""),
		))
	}
	awsCfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("config SQS: %w", err)
	}
	return sqs.NewFromConfig(awsCfg, func(o *sqs.Options) {
		if settings.Endpoint != "" {
			o.BaseEndpoint = aws.String(settings.Endpoint)
		}
		// The default retryer repeats ReceiveMessage. Each call increments
		// ApproximateReceiveCount, so a retried poll looks like a failed
		// attempt and can trip the queue's maxReceiveCount.
		o.Retryer = aws.NopRetryer{}
		// The SDK's default HTTP client aborts a long poll and LocalStack still
		// counts that call. A plain client lets WaitTimeSeconds finish.
		// Cancel still comes from the call context.
		o.HTTPClient = &http.Client{Timeout: 5 * time.Second}
	}), nil
}

func lookupQueueURL(ctx context.Context, api sqsAPI, name string) (string, error) {
	out, err := api.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(name)})
	if err != nil {
		return "", err
	}
	if out.QueueUrl == nil || *out.QueueUrl == "" {
		return "", fmt.Errorf("sin url")
	}
	return *out.QueueUrl, nil
}

func (s *SQSQueue) Fork(name string) Backend {
	cp := *s
	cp.Queue = s.Queue.WithConsumer(name)
	return &cp
}

func (s *SQSQueue) EnsureGroup(ctx context.Context) error {
	if _, err := s.api.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       aws.String(s.jobsURL),
		AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameApproximateNumberOfMessages},
	}); err != nil {
		return err
	}
	_, err := s.DLQLen(ctx)
	return err
}

func (s *SQSQueue) PromoteDue(context.Context, int) (int, error) { return 0, nil }

func (s *SQSQueue) Claim(context.Context, int) ([]Delivery, error) { return nil, nil }

func (s *SQSQueue) Read(ctx context.Context, count int, block time.Duration) ([]Delivery, error) {
	if count < 1 {
		count = 1
	}
	// One message per poll. LocalStack 4.14 drops a message that arrives during
	// a long poll when MaxNumberOfMessages is greater than 1, and still counts
	// that poll toward ApproximateReceiveCount.
	if count > 1 {
		count = 1
	}
	wait := int32(0)
	// WaitTimeSeconds >= 1 is long polling. One second keeps a quiet queue
	// from busy-spinning and still returns a message that is already there.
	// A 20s poll is late for the local checks when the emulator does not
	// cut the wait short.
	if block >= time.Second {
		wait = 1
	}
	visibility := visibilitySeconds(s.Config().ClaimIdle)
	if s.pollMu != nil {
		s.pollMu.Lock()
		defer s.pollMu.Unlock()
	}
	msgs, err := s.receive(ctx, s.jobsURL, count, wait, visibility)
	if err != nil {
		return nil, err
	}
	// Hide the message before another worker can poll. A ReceiveMessage
	// visibility timeout is not always applied by SQS stand-ins.
	for _, m := range msgs {
		if chErr := s.changeVisibility(ctx, s.jobsURL, m.ReceiptHandle, visibility); chErr != nil {
			log.Printf("[async] no se pudo ocultar %s: %v", m.Job.InvocationID, chErr)
		}
	}
	return msgs, nil
}

// Ack deletes a finished message. A dead-lettered invocation is left for the
// queue redrive policy. A message already waiting out its backoff is left
// alone: SQS has a single copy, unlike a Redis stream.
func (s *SQSQueue) Ack(ctx context.Context, d Delivery) error {
	if d.ReceiptHandle == "" {
		return nil
	}
	if d.Job.InvocationID != "" {
		st, err := s.LoadState(ctx, d.Job.InvocationID)
		if err == nil && st.Phase == PhaseDead {
			return s.nudgeRedrive(ctx, d)
		}
		if err == nil && st.Phase == PhaseRetrying {
			return nil
		}
	}
	return s.delete(ctx, s.jobsURL, d.ReceiptHandle)
}

func (s *SQSQueue) Extend(ctx context.Context, d Delivery) error {
	if d.ReceiptHandle == "" {
		return nil
	}
	return s.changeVisibility(ctx, s.jobsURL, d.ReceiptHandle, visibilitySeconds(s.Config().ClaimIdle))
}

func (s *SQSQueue) Postpone(ctx context.Context, d Delivery, job Job, delay time.Duration, st State) error {
	if err := s.SaveState(ctx, job.InvocationID, st); err != nil {
		return err
	}
	return s.changeVisibility(ctx, s.jobsURL, d.ReceiptHandle, secondsCeil(delay))
}

func (s *SQSQueue) SealFailed(ctx context.Context, d Delivery, st State) error {
	if err := s.SaveState(ctx, d.Job.InvocationID, st); err != nil {
		return err
	}
	return s.delete(ctx, s.jobsURL, d.ReceiptHandle)
}

func (s *SQSQueue) SealDead(ctx context.Context, d Delivery, st State, entry DLQEntry) error {
	raw, err := json.Marshal(metaFrom(entry))
	if err != nil {
		return err
	}
	if err := s.Queue.rdb.Set(ctx, dlqMetaKey(entry.ID), raw, 7*24*time.Hour).Err(); err != nil {
		return err
	}
	if err := s.SaveState(ctx, entry.InvocationID, st); err != nil {
		return err
	}
	return s.nudgeRedrive(ctx, d)
}

func (s *SQSQueue) Enqueue(ctx context.Context, invocationID, executionID string, body json.RawMessage) (string, bool, error) {
	maxAttempts := s.Config().MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = DefaultMaxAttempts
	}
	job := Job{
		InvocationID: invocationID,
		Attempt:      1,
		MaxAttempts:  maxAttempts,
		EnqueuedAt:   s.Now().UnixMilli(),
		Body:         body,
		TraceParent:  ntrace.Header(ctx),
		TraceState:   ntrace.State(ctx),
	}
	stored, dup, err := s.Reserve(ctx, invocationID, executionID, job)
	if err != nil || dup {
		return stored, dup, err
	}
	if err := s.send(ctx, s.jobsURL, job); err != nil {
		_ = s.ReleaseAccept(ctx, invocationID)
		return "", false, err
	}
	return stored, false, nil
}

func (s *SQSQueue) DLQLen(ctx context.Context) (int64, error) {
	out, err := s.api.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(s.dlqURL),
		AttributeNames: []types.QueueAttributeName{
			types.QueueAttributeNameApproximateNumberOfMessages,
			types.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
		},
	})
	if err != nil {
		return 0, err
	}
	return parseAttr(out.Attributes, "ApproximateNumberOfMessages") +
		parseAttr(out.Attributes, "ApproximateNumberOfMessagesNotVisible"), nil
}

func (s *SQSQueue) ListDLQ(ctx context.Context) ([]DLQEntry, error) {
	msgs, err := s.scan(ctx, s.dlqURL, 200)
	if err != nil {
		return nil, err
	}
	entries := make([]DLQEntry, 0, len(msgs))
	for _, m := range msgs {
		if m.Job.InvocationID == "" {
			continue
		}
		entries = append(entries, s.entryFrom(ctx, m))
	}
	s.release(ctx, s.dlqURL, msgs)
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

func (s *SQSQueue) Redrive(ctx context.Context, id string) (DLQEntry, error) {
	msgs, err := s.scan(ctx, s.dlqURL, 200)
	if err != nil {
		return DLQEntry{}, err
	}
	var found *Delivery
	rest := make([]Delivery, 0, len(msgs))
	for i := range msgs {
		if found == nil && msgs[i].Job.InvocationID == id {
			cp := msgs[i]
			found = &cp
			continue
		}
		rest = append(rest, msgs[i])
	}
	s.release(ctx, s.dlqURL, rest)
	if found == nil {
		return DLQEntry{}, ErrNotFound
	}
	entry := s.entryFrom(ctx, *found)
	if err := s.delete(ctx, s.dlqURL, found.ReceiptHandle); err != nil {
		return DLQEntry{}, err
	}
	_ = s.Queue.rdb.Del(ctx, dlqMetaKey(id)).Err()
	job := found.Job
	job.Attempt = 1
	if job.MaxAttempts < 1 {
		job.MaxAttempts = s.Config().MaxAttempts
	}
	meta := peekBody(job.Body)
	now := s.Now().UnixMilli()
	if err := s.SaveState(ctx, id, State{
		Phase:      PhaseQueued,
		EnqueuedAt: firstNonZero(entry.EnqueuedAt, job.EnqueuedAt),
	}); err != nil {
		return DLQEntry{}, err
	}
	if err := s.TouchPending(ctx, meta.ExecutionID, meta.FunctionID); err != nil {
		return DLQEntry{}, err
	}
	if err := s.send(ctx, s.jobsURL, job); err != nil {
		return DLQEntry{}, err
	}
	if err := s.Publish(ctx, Result{
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

// nudgeRedrive makes the source message visible so the next receive exceeds
// maxReceiveCount and SQS moves it to the DLQ. The message is not deleted.
func (s *SQSQueue) nudgeRedrive(ctx context.Context, d Delivery) error {
	_ = s.changeVisibility(ctx, s.jobsURL, d.ReceiptHandle, 0)
	deadline := time.Now().Add(8 * time.Second)
	for {
		batch, err := s.receive(ctx, s.jobsURL, 10, 1, 0)
		if err != nil && ctx.Err() == nil {
			log.Printf("[async] receive para redrive %s: %v", d.Job.InvocationID, err)
		}
		for _, m := range batch {
			_ = s.changeVisibility(ctx, s.jobsURL, m.ReceiptHandle, 0)
		}
		ok, err := s.onDLQ(ctx, d.Job.InvocationID)
		if err != nil {
			return err
		}
		if ok {
			log.Printf("[async] %s quedó en la DLQ por RedrivePolicy", d.Job.InvocationID)
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("SQS no movió %s a la DLQ", d.Job.InvocationID)
		}
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *SQSQueue) onDLQ(ctx context.Context, invocationID string) (bool, error) {
	msgs, err := s.scan(ctx, s.dlqURL, 200)
	if err != nil {
		return false, err
	}
	found := false
	for _, m := range msgs {
		if m.Job.InvocationID == invocationID {
			found = true
			break
		}
	}
	s.release(ctx, s.dlqURL, msgs)
	return found, nil
}

func (s *SQSQueue) scan(ctx context.Context, queueURL string, limit int) ([]Delivery, error) {
	if limit < 1 {
		limit = 1
	}
	seen := map[string]Delivery{}
	for len(seen) < limit {
		batch, err := s.receive(ctx, queueURL, 10, 0, 3)
		if err != nil {
			s.release(ctx, queueURL, values(seen))
			return nil, err
		}
		if len(batch) == 0 {
			break
		}
		added := 0
		for _, m := range batch {
			if m.ID == "" {
				continue
			}
			if _, ok := seen[m.ID]; ok {
				continue
			}
			seen[m.ID] = m
			added++
		}
		if added == 0 || len(batch) < 10 {
			break
		}
	}
	return values(seen), nil
}

func values(seen map[string]Delivery) []Delivery {
	out := make([]Delivery, 0, len(seen))
	for _, m := range seen {
		out = append(out, m)
	}
	return out
}

func (s *SQSQueue) release(ctx context.Context, queueURL string, msgs []Delivery) {
	for _, m := range msgs {
		_ = s.changeVisibility(ctx, queueURL, m.ReceiptHandle, 0)
	}
}

func (s *SQSQueue) receive(ctx context.Context, queueURL string, count int, wait, visibility int32) ([]Delivery, error) {
	out, err := s.api.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:                    aws.String(queueURL),
		MaxNumberOfMessages:         int32(count),
		WaitTimeSeconds:             wait,
		VisibilityTimeout:           visibility,
		MessageAttributeNames:       []string{"All"},
		MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameApproximateReceiveCount},
	})
	if err != nil {
		return nil, err
	}
	deliveries := make([]Delivery, 0, len(out.Messages))
	for _, m := range out.Messages {
		d, ok := parseSQSMessage(m)
		if !ok {
			log.Printf("[async] mensaje SQS ilegible %s", aws.ToString(m.MessageId))
			_ = s.delete(ctx, queueURL, aws.ToString(m.ReceiptHandle))
			continue
		}
		deliveries = append(deliveries, d)
	}
	return deliveries, nil
}

func (s *SQSQueue) send(ctx context.Context, queueURL string, job Job) error {
	raw, err := json.Marshal(job)
	if err != nil {
		return err
	}
	input := &sqs.SendMessageInput{
		QueueUrl:    aws.String(queueURL),
		MessageBody: aws.String(string(raw)),
	}
	attrs := map[string]types.MessageAttributeValue{}
	if job.TraceParent != "" {
		attrs["traceparent"] = types.MessageAttributeValue{
			DataType:    aws.String("String"),
			StringValue: aws.String(job.TraceParent),
		}
	}
	if job.TraceState != "" {
		attrs["tracestate"] = types.MessageAttributeValue{
			DataType:    aws.String("String"),
			StringValue: aws.String(job.TraceState),
		}
	}
	if len(attrs) > 0 {
		input.MessageAttributes = attrs
	}
	_, err = s.api.SendMessage(ctx, input)
	return err
}

func (s *SQSQueue) delete(ctx context.Context, queueURL, receipt string) error {
	if receipt == "" {
		return nil
	}
	_, err := s.api.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(queueURL),
		ReceiptHandle: aws.String(receipt),
	})
	return err
}

func (s *SQSQueue) changeVisibility(ctx context.Context, queueURL, receipt string, timeout int32) error {
	if receipt == "" {
		return nil
	}
	_, err := s.api.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl:          aws.String(queueURL),
		ReceiptHandle:     aws.String(receipt),
		VisibilityTimeout: timeout,
	})
	return err
}

func parseSQSMessage(m types.Message) (Delivery, bool) {
	body := aws.ToString(m.Body)
	var job Job
	if err := json.Unmarshal([]byte(body), &job); err != nil || job.InvocationID == "" {
		return Delivery{}, false
	}
	if attr, ok := m.MessageAttributes["traceparent"]; ok && attr.StringValue != nil && *attr.StringValue != "" {
		job.TraceParent = *attr.StringValue
	}
	if attr, ok := m.MessageAttributes["tracestate"]; ok && attr.StringValue != nil && *attr.StringValue != "" {
		job.TraceState = *attr.StringValue
	}
	count := 0
	if raw := m.Attributes["ApproximateReceiveCount"]; raw != "" {
		count, _ = strconv.Atoi(raw)
	}
	return Delivery{
		ID:            aws.ToString(m.MessageId),
		ReceiptHandle: aws.ToString(m.ReceiptHandle),
		ReceiveCount:  count,
		Job:           job,
	}, true
}

type storedDLQMeta struct {
	LastError    string          `json:"lastError"`
	ExitCode     int             `json:"exitCode"`
	Attempts     int             `json:"attempts"`
	DurationMs   int64           `json:"durationMs"`
	AttemptTimes []int64         `json:"attemptTimes"`
	EnqueuedAt   int64           `json:"enqueuedAt"`
	FailedAt     int64           `json:"failedAt"`
	DeadAt       int64           `json:"deadAt"`
	ExecutionID  string          `json:"executionId"`
	FunctionID   string          `json:"functionId"`
	Version      string          `json:"version"`
	Event        json.RawMessage `json:"event,omitempty"`
	MaxAttempts  int             `json:"maxAttempts"`
}

func metaFrom(entry DLQEntry) storedDLQMeta {
	return storedDLQMeta{
		LastError:    entry.LastError,
		ExitCode:     entry.ExitCode,
		Attempts:     entry.Attempts,
		DurationMs:   entry.DurationMs,
		AttemptTimes: entry.AttemptTimes,
		EnqueuedAt:   entry.EnqueuedAt,
		FailedAt:     entry.FailedAt,
		DeadAt:       entry.DeadAt,
		ExecutionID:  entry.ExecutionID,
		FunctionID:   entry.FunctionID,
		Version:      entry.Version,
		Event:        entry.Event,
		MaxAttempts:  entry.MaxAttempts,
	}
}

func dlqMetaKey(id string) string { return "nimbus:async:dlqmeta:" + id }

func (s *SQSQueue) entryFrom(ctx context.Context, d Delivery) DLQEntry {
	bodyMeta := peekBody(d.Job.Body)
	rawBody, _ := json.Marshal(d.Job)
	entry := DLQEntry{
		ID:           d.Job.InvocationID,
		InvocationID: d.Job.InvocationID,
		ExecutionID:  bodyMeta.ExecutionID,
		FunctionID:   bodyMeta.FunctionID,
		Version:      bodyMeta.Version,
		Event:        bodyMeta.Event,
		Payload:      rawBody,
		Attempts:     d.ReceiveCount,
		MaxAttempts:  d.Job.MaxAttempts,
		EnqueuedAt:   d.Job.EnqueuedAt,
	}
	if entry.Attempts == 0 {
		entry.Attempts = d.Job.Attempt
	}
	raw, err := s.Queue.rdb.Get(ctx, dlqMetaKey(d.Job.InvocationID)).Result()
	if err != nil {
		return entry
	}
	var saved storedDLQMeta
	if json.Unmarshal([]byte(raw), &saved) != nil {
		return entry
	}
	entry.LastError = saved.LastError
	entry.ExitCode = saved.ExitCode
	entry.DurationMs = saved.DurationMs
	entry.AttemptTimes = saved.AttemptTimes
	entry.FailedAt = saved.FailedAt
	entry.DeadAt = saved.DeadAt
	if saved.Attempts > 0 {
		entry.Attempts = saved.Attempts
	}
	if saved.EnqueuedAt != 0 {
		entry.EnqueuedAt = saved.EnqueuedAt
	}
	if saved.ExecutionID != "" {
		entry.ExecutionID = saved.ExecutionID
	}
	if saved.FunctionID != "" {
		entry.FunctionID = saved.FunctionID
	}
	if saved.Version != "" {
		entry.Version = saved.Version
	}
	if len(saved.Event) > 0 {
		entry.Event = saved.Event
	}
	if saved.MaxAttempts > 0 {
		entry.MaxAttempts = saved.MaxAttempts
	}
	return entry
}

func parseAttr(attrs map[string]string, key string) int64 {
	n, _ := strconv.ParseInt(attrs[key], 10, 64)
	if n < 0 {
		return 0
	}
	return n
}
