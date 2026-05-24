// Package service holds the application-layer orchestration: the send-message
// service (handler-facing) and small helpers (backoff) used by the worker. It
// owns interfaces it depends on (storage, signal-cli client, repo) per
// docs/style.md "Interfaces".
package service

import (
	"math"
	"math/rand/v2"
	"time"
)

// Backoff returns the delay to wait before the next attempt: exponential in
// the attempt count with full jitter (uniform draw in [0, raw)), capped at
// max so a long-failed row never schedules past the ceiling.
//
//	raw    := base * 2^(attempts-1)
//	jitter := rand() * raw    // "full jitter" — uniform in [0, raw)
//	return min(max, jitter)
//
// Full jitter (vs. a small fixed-fraction jitter on top of raw) avoids
// thundering-herd alignment when many rows transition to FAILED at once —
// every retry resolves on its own uniform offset rather than clustering.
//
// attempts is 1-based (first failure → attempts=1). Calling with attempts<=0
// returns 0 so callers don't have to special-case the pre-first-attempt path.
func Backoff(attempts int, base, max time.Duration) time.Duration {
	if attempts <= 0 {
		return 0
	}
	// math.Pow(2, attempts-1) overflows int64 around attempts=64 in nanoseconds
	// terms long before that — guard via direct comparison against max.
	raw := float64(base) * math.Pow(2, float64(attempts-1))
	jitter := rand.Float64() * raw //nolint:gosec // backoff jitter, not crypto
	if jitter > float64(max) {
		return max
	}
	return time.Duration(jitter)
}
