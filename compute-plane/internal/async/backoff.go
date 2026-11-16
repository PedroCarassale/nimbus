package async

import "time"

// Backoff is the wait after a failed attempt before the next one starts.
// attempt is 1-based: the attempt that just failed. The delay doubles each
// time and is capped, then unit (0..1) adds up to jitter on top.
func Backoff(attempt int, base, jitter, ceiling time.Duration, unit float64) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if base < 0 {
		base = 0
	}
	shift := attempt - 1
	if shift > 16 {
		shift = 16
	}
	delay := base
	if shift > 0 && base > 0 {
		delay = base << shift
		if delay/base != 1<<shift {
			delay = ceiling
		}
	}
	if ceiling > 0 && delay > ceiling {
		delay = ceiling
	}
	if jitter > 0 && unit > 0 {
		if unit > 1 {
			unit = 1
		}
		delay += time.Duration(unit * float64(jitter))
	}
	if delay < 0 {
		return 0
	}
	return delay
}
