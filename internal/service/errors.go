package service

import (
	werror "github.com/palantir/witchcraft-go-error"

	signalapi "github.com/olehmushka/go-signalium/internal/generated/signalium/api"
)

// The service returns conjure error types directly so the handler edge doesn't
// have to translate twice (handler.errors.go already accepts conjure errors as
// pass-through). docs/style.md "Errors" notes that mapping to conjure happens
// at the handler boundary; sites that *only* surface validation/sender errors
// stay readable when the service produces the typed error inline.

func invalid(field, reason string) error {
	return signalapi.WrapWithInvalidSignalMessage(
		werror.Error("invalid signal message",
			werror.SafeParam("field", field),
			werror.SafeParam("reason", reason)),
		field, reason,
	)
}

func senderMismatch(configured, requested string) error {
	return signalapi.WrapWithSenderMismatch(
		werror.Error("sender phone number does not match configured",
			werror.SafeParam("configured", configured),
			werror.SafeParam("requested", requested)),
		configured, requested,
	)
}

func attachmentUploadFailed(filename string, cause error) error {
	return signalapi.WrapWithAttachmentUploadFailed(
		werror.Wrap(cause, "attachment upload failed",
			werror.SafeParam("filename", filename)),
		filename,
	)
}
