package async

import "time"

// secondsCeil converts a backoff to the integer seconds SQS accepts.
// Any positive duration shorter than a second becomes 1. Zero stays zero,
// which makes the message visible immediately. The API caps visibility at 12h.
func secondsCeil(d time.Duration) int32 {
	if d <= 0 {
		return 0
	}
	secs := (d + time.Second - 1) / time.Second
	if secs < 1 {
		secs = 1
	}
	const max = 12 * time.Hour
	if secs > max/time.Second {
		secs = max / time.Second
	}
	return int32(secs)
}

// visibilitySeconds is the timeout applied while a worker holds a message.
// A zero claim idle falls back to 30s so the message is not immediately
// visible to another worker.
func visibilitySeconds(d time.Duration) int32 {
	s := secondsCeil(d)
	if s < 1 {
		return 30
	}
	return s
}
