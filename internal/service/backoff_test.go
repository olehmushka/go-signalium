package service_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/olehmushka/go-signalium/internal/service"
)

func TestBackoff(t *testing.T) {
	t.Parallel()

	const base = 2 * time.Second
	const maxD = time.Hour

	tests := []struct {
		name     string
		attempts int
		upper    time.Duration // jitter is full-random in [0, raw); raw at this attempt
	}{
		{"zero attempts is zero", 0, 0},
		{"negative attempts is zero", -3, 0},
		{"first attempt jitter <= base", 1, base},
		{"second attempt jitter <= 2*base", 2, 2 * base},
		{"third attempt jitter <= 4*base", 3, 4 * base},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := service.Backoff(tc.attempts, base, maxD)
			require.GreaterOrEqual(t, got, time.Duration(0))
			if tc.attempts <= 0 {
				assert.Equal(t, time.Duration(0), got)
				return
			}
			assert.LessOrEqual(t, got, tc.upper)
		})
	}
}

func TestBackoff_CappedAtMax(t *testing.T) {
	t.Parallel()

	// attempts=20 with base=1s gives raw=524288s which overshoots maxD=1m for
	// every random draw — the cap must clamp it deterministically.
	const base = time.Second
	const maxD = time.Minute
	for i := 0; i < 100; i++ {
		assert.LessOrEqual(t, service.Backoff(20, base, maxD), maxD)
	}
}
