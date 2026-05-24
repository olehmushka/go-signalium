package handler

// Internal-package benchmark so the un-exported mapToConjureError is reachable
// without changing visibility just for benches.

import (
	"testing"

	werror "github.com/palantir/witchcraft-go-error"

	"github.com/olehmushka/go-signalium/internal/domain"
	signalapi "github.com/olehmushka/go-signalium/internal/generated/signalium/api"
)

// BenchmarkMapToConjureError covers every branch of the sentinel-to-Conjure
// dispatcher every error response traverses on its way out the door.
func BenchmarkMapToConjureError(b *testing.B) {
	cases := []struct {
		name string
		err  error
	}{
		{"NotFound", domain.ErrNotFound},
		{"IdempotencyConflict", domain.ErrIdempotencyConflict},
		{"Internal", werror.Error("unexpected boom from below")},
		{"PassThrough", signalapi.WrapWithInvalidSignalMessage(werror.Error("x"), "field", "reason")},
	}
	for _, tc := range cases {
		tc := tc
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			var sink error
			for b.Loop() {
				sink = mapToConjureError(tc.err)
			}
			_ = sink
		})
	}
}
