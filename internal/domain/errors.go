package domain

import werror "github.com/palantir/witchcraft-go-error"

// Sentinel errors raised by repo/service code and matched at the handler edge
// via errors.Is. See docs/style.md ("Errors").
var (
	// ErrNotFound is returned when a lookup by id/external id/idempotency key
	// matches no live row.
	ErrNotFound = werror.Error("signal message not found")

	// ErrIdempotencyConflict is returned when a POST supplies an
	// idempotency_key that already maps to a different external_id.
	ErrIdempotencyConflict = werror.Error("idempotency key conflict")
)
