package logs

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	StreamKeyPrefix = "nimbus:logs:"
	StreamMaxLen    = 1000
	StreamTTL       = 1 * time.Hour
)

type LogEntry struct {
	Timestamp   int64  `json:"timestamp"`
	ExecutionID string `json:"executionId"`
	Stream      string `json:"stream"`
	Line        string `json:"line"`
	Sequence    int    `json:"sequence"`
}

type StreamWriter struct {
	rdb         *redis.Client
	executionID string
	streamKey   string
	sequence    int
}

func NewStreamWriter(rdb *redis.Client, executionID string) *StreamWriter {
	return &StreamWriter{
		rdb:         rdb,
		executionID: executionID,
		streamKey:   StreamKeyPrefix + executionID,
		sequence:    0,
	}
}

func (sw *StreamWriter) StreamKey() string {
	return sw.streamKey
}

func (sw *StreamWriter) Write(ctx context.Context, stream string, line string) error {
	sw.sequence++

	entry := LogEntry{
		Timestamp:   time.Now().UnixMilli(),
		ExecutionID: sw.executionID,
		Stream:      stream,
		Line:        line,
		Sequence:    sw.sequence,
	}

	entryJSON, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("error marshaling log entry: %w", err)
	}

	_, err = sw.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: sw.streamKey,
		MaxLen: StreamMaxLen,
		Approx: true,
		Values: map[string]interface{}{
			"data": string(entryJSON),
		},
	}).Result()

	if err != nil {
		return fmt.Errorf("error writing to stream: %w", err)
	}

	return nil
}

func (sw *StreamWriter) WriteStdout(ctx context.Context, line string) error {
	return sw.Write(ctx, "stdout", line)
}

func (sw *StreamWriter) WriteStderr(ctx context.Context, line string) error {
	return sw.Write(ctx, "stderr", line)
}

func (sw *StreamWriter) Close(ctx context.Context) error {
	entry := LogEntry{
		Timestamp:   time.Now().UnixMilli(),
		ExecutionID: sw.executionID,
		Stream:      "_end",
		Line:        "execution_complete",
		Sequence:    sw.sequence + 1,
	}

	entryJSON, _ := json.Marshal(entry)
	sw.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: sw.streamKey,
		MaxLen: StreamMaxLen,
		Approx: true,
		Values: map[string]interface{}{
			"data": string(entryJSON),
		},
	})

	if err := sw.rdb.Expire(ctx, sw.streamKey, StreamTTL).Err(); err != nil {
		log.Printf("[logs] Warning: no se pudo establecer TTL para %s: %v", sw.streamKey, err)
	}

	return nil
}

type StreamReader struct {
	rdb       *redis.Client
	streamKey string
}

func NewStreamReader(rdb *redis.Client, executionID string) *StreamReader {
	return &StreamReader{
		rdb:       rdb,
		streamKey: StreamKeyPrefix + executionID,
	}
}

func (sr *StreamReader) StreamKey() string {
	return sr.streamKey
}

func (sr *StreamReader) ReadAll(ctx context.Context) ([]LogEntry, error) {
	msgs, err := sr.rdb.XRange(ctx, sr.streamKey, "-", "+").Result()
	if err != nil {
		return nil, fmt.Errorf("error reading stream: %w", err)
	}

	entries := make([]LogEntry, 0, len(msgs))
	for _, msg := range msgs {
		data, ok := msg.Values["data"].(string)
		if !ok {
			continue
		}

		var entry LogEntry
		if err := json.Unmarshal([]byte(data), &entry); err != nil {
			continue
		}
		entries = append(entries, entry)
	}

	return entries, nil
}

func (sr *StreamReader) Subscribe(ctx context.Context, lastID string) (<-chan LogEntry, error) {
	ch := make(chan LogEntry, 100)

	if lastID == "" {
		lastID = "0"
	}

	go func() {
		defer close(ch)

		currentID := lastID

		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			streams, err := sr.rdb.XRead(ctx, &redis.XReadArgs{
				Streams: []string{sr.streamKey, currentID},
				Block:   1 * time.Second,
				Count:   100,
			}).Result()

			if err == redis.Nil || err == context.DeadlineExceeded {
				continue
			}
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Printf("[logs] Error reading stream: %v", err)
				time.Sleep(100 * time.Millisecond)
				continue
			}

			for _, stream := range streams {
				for _, msg := range stream.Messages {
					data, ok := msg.Values["data"].(string)
					if !ok {
						continue
					}

					var entry LogEntry
					if err := json.Unmarshal([]byte(data), &entry); err != nil {
						continue
					}

					select {
					case ch <- entry:
						currentID = msg.ID
					case <-ctx.Done():
						return
					}

					if entry.Stream == "_end" {
						return
					}
				}
			}
		}
	}()

	return ch, nil
}

func (sr *StreamReader) Exists(ctx context.Context) (bool, error) {
	exists, err := sr.rdb.Exists(ctx, sr.streamKey).Result()
	if err != nil {
		return false, err
	}
	return exists > 0, nil
}
