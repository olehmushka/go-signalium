//go:build integration

// Benchmarks for the outbox claim roundtrip. Gated by the `integration` tag so
// `go test ./...` (and `make bench`) stay hermetic; run with
// `make bench-integration` (Docker required).

package repo_test

import (
	"errors" //nolint:depguard // stdlib errors.Is for the eligible-rows-exhausted sentinel
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/olehmushka/go-signalium/internal/domain"
)

// BenchmarkMessages_ClaimPending measures the round-trip latency of the
// `FOR UPDATE SKIP LOCKED` outbox claim — the worker's hot path. Pre-seeds a
// large pool of PENDING rows so the loop never starves; reports both ns/op
// and an explicit "claims" counter so the reader can sanity-check the loop
// count vs. the seeded inventory.
func BenchmarkMessages_ClaimPending(b *testing.B) {
	pool := setupDB(b)
	r := newMessages(b, pool)
	ctx := b.Context()

	// Seed an upper bound on iterations so every iteration finds an eligible
	// row. `b.Loop()` adapts to the available time budget; over-seeding by a
	// healthy factor avoids ErrNotFound mid-bench.
	const seedRows = 200_000
	for i := 0; i < seedRows; i++ {
		_, err := r.Insert(ctx, newInsertParams(fmt.Sprintf("bench-%d", i), "+380111111111"))
		require.NoError(b, err)
	}

	b.ReportAllocs()
	b.ResetTimer()

	claims := 0
	for b.Loop() {
		_, err := r.ClaimPending(ctx, 30*time.Second)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				b.Fatalf("ran out of pending rows after %d claims — seed harder", claims)
			}
			b.Fatalf("claim pending: %v", err)
		}
		claims++
	}
	b.ReportMetric(float64(claims), "claims")
}
