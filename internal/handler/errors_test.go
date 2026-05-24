package handler

// Internal package test so the un-exported mapToConjureError is reachable
// without changing visibility just for tests.

import (
	"testing"

	werror "github.com/palantir/witchcraft-go-error"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/olehmushka/go-signalium/internal/domain"
	signalapi "github.com/olehmushka/go-signalium/internal/generated/signalium/api"
)

func TestMapToConjureError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      error
		isError func(error) bool // signalapi.IsX matcher
	}{
		{"nil passes through", nil, nil},
		{"ErrNotFound maps to SignalMessageNotFound", domain.ErrNotFound, signalapi.IsSignalMessageNotFound},
		{"ErrIdempotencyConflict maps to IdempotencyConflict", domain.ErrIdempotencyConflict, signalapi.IsIdempotencyConflict},
		{"unmapped error becomes InternalServiceError", werror.Error("boom"), signalapi.IsInternalServiceError},
		{
			"pre-wrapped InvalidSignalMessage passes through",
			signalapi.WrapWithInvalidSignalMessage(werror.Error("x"), "f", "r"),
			signalapi.IsInvalidSignalMessage,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := mapToConjureError(tc.in)
			if tc.in == nil {
				assert.NoError(t, got)
				return
			}
			require.Error(t, got)
			if tc.isError != nil {
				assert.True(t, tc.isError(got), "expected matcher to recognise the conjure error")
			}
		})
	}
}
