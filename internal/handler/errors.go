// Package handler provides the witchcraft-facing implementations of the
// Conjure-generated service interfaces plus the raw multipart upload handler
// (M4). For M3 the service handlers all return a 501 conjure error; M4+ swap
// each method body for the real service-layer call.
//
// The translation from internal/domain sentinels (errors.Is comparable) to
// the Conjure-generated error types lives in mapToConjureError so that
// service/repo code only handles plain sentinels and the HTTP-shaped errors
// stay at the edge. See docs/rest-api.md "Errors".
package handler

import (
	"errors" //nolint:depguard // stdlib errors.Is is the canonical sentinel check; docs/style.md "Errors"

	werror "github.com/palantir/witchcraft-go-error"

	"github.com/olehmushka/go-signalium/internal/domain"
	signalapi "github.com/olehmushka/go-signalium/internal/generated/signalium/api"
)

// mapToConjureError converts an internal error to the Conjure error that
// should be rendered on the wire. Pass-through if err is already a Conjure
// type; sentinel matches dispatch to the corresponding NewX constructor;
// otherwise wrap as InternalServiceError so the framework returns 500 without
// leaking internals.
func mapToConjureError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case signalapi.IsIdempotencyConflict(err),
		signalapi.IsInvalidSignalMessage(err),
		signalapi.IsSignalMessageNotFound(err),
		signalapi.IsSenderMismatch(err),
		signalapi.IsAttachmentUploadFailed(err),
		signalapi.IsSignalCliUnavailable(err),
		signalapi.IsInternalServiceError(err):
		return err
	case errors.Is(err, domain.ErrNotFound):
		return signalapi.WrapWithSignalMessageNotFound(err, "")
	case errors.Is(err, domain.ErrIdempotencyConflict):
		return signalapi.WrapWithIdempotencyConflict(err, "")
	}
	return signalapi.WrapWithInternalServiceError(werror.Wrap(err, "internal service error"))
}
