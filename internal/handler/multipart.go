package handler

import (
	"context"
	"errors" //nolint:depguard // stdlib errors.As/Is for wire-error mapping at the handler edge
	"io"
	"mime/multipart"
	"net/http"

	googleuuid "github.com/google/uuid"
	"github.com/palantir/conjure-go-runtime/v3/conjure-go-contract/codecs"
	conjerrors "github.com/palantir/conjure-go-runtime/v3/conjure-go-contract/errors"
	palantiruuid "github.com/palantir/pkg/uuid"
	werror "github.com/palantir/witchcraft-go-error"
	"github.com/palantir/witchcraft-go-logging/wlog/svclog/svc1log"

	signalapi "github.com/olehmushka/go-signalium/internal/generated/signalium/api"
	"github.com/olehmushka/go-signalium/internal/service"
)

// MultipartPath is the conjure-bypass path the multipart handler registers
// against. Kept in a constant so the server module can register it without
// duplicating the literal.
const MultipartPath = "/api/v1/signal-messages"

// MultipartHandler accepts a multipart/form-data POST and forwards the
// validated metadata + each attachment stream to SendMessageService.Enqueue.
// See docs/rest-api.md "Send" for the wire contract and
// docs/decisions/0008-conjure-bypass-for-multipart.md for the rationale.
type MultipartHandler struct {
	svc    service.SendMessageEnqueuer
	logger svc1log.Logger
}

// NewMultipartHandler is the fx provider.
func NewMultipartHandler(svc service.SendMessageEnqueuer, logger svc1log.Logger) *MultipartHandler {
	return &MultipartHandler{svc: svc, logger: logger}
}

// ServeHTTP implements http.Handler.
func (h *MultipartHandler) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		writeError(rw, signalapi.WrapWithInvalidSignalMessage(
			werror.Error("method not allowed"),
			"method", req.Method,
		))
		return
	}
	ctx := req.Context()
	mr, err := req.MultipartReader()
	if err != nil {
		writeError(rw, signalapi.WrapWithInvalidSignalMessage(
			werror.Wrap(err, "expected multipart/form-data"),
			"contentType", req.Header.Get("Content-Type"),
		))
		return
	}

	meta, parts, err := splitMetadata(ctx, mr)
	if err != nil {
		writeError(rw, err)
		return
	}

	result, err := h.svc.Enqueue(ctx, meta, parts)
	if err != nil {
		writeError(rw, mapToConjureError(err))
		return
	}
	writeAccepted(ctx, rw, result.ID)
}

// splitMetadata reads the leading "metadata" part (must be first per
// docs/attachments.md "Order matters"), decodes it into a CreateMessage, and
// returns a streaming AttachmentParts iterator over the remaining parts.
func splitMetadata(ctx context.Context, mr *multipart.Reader) (service.CreateMessage, service.AttachmentParts, error) {
	first, err := mr.NextPart()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return service.CreateMessage{}, nil, signalapi.WrapWithInvalidSignalMessage(
				werror.ErrorWithContextParams(ctx, "missing metadata part"),
				"metadata", "missing",
			)
		}
		return service.CreateMessage{}, nil, signalapi.WrapWithInvalidSignalMessage(
			werror.WrapWithContextParams(ctx, err, "read first part"),
			"metadata", err.Error(),
		)
	}
	if first.FormName() != "metadata" {
		return service.CreateMessage{}, nil, signalapi.WrapWithInvalidSignalMessage(
			werror.ErrorWithContextParams(ctx, "metadata must be the first part",
				werror.SafeParam("got", first.FormName())),
			"metadata", "must be first part",
		)
	}
	var req signalapi.CreateSignalMessageRequest
	if err := codecs.JSON.Decode(first, &req); err != nil {
		_ = first.Close()
		return service.CreateMessage{}, nil, signalapi.WrapWithInvalidSignalMessage(
			werror.WrapWithContextParams(ctx, err, "decode metadata"),
			"metadata", "invalid JSON",
		)
	}
	_ = first.Close()

	return toCreateMessage(req), &multipartAttachments{r: mr}, nil
}

func toCreateMessage(in signalapi.CreateSignalMessageRequest) service.CreateMessage {
	return service.CreateMessage{
		ExternalID:        in.ExternalId,
		IdempotencyKey:    in.IdempotencyKey,
		Recipient:         in.Recipient,
		GroupID:           in.GroupId,
		SenderPhoneNumber: in.SenderPhoneNumber,
		Content:           in.Content,
		QuoteResultID:     in.QuoteResultId,
		TimeoutSeconds:    in.TimeoutSeconds,
		MaxAttempts:       in.MaxAttempts,
	}
}

// multipartAttachments adapts mime/multipart.Reader to AttachmentParts. Each
// Next call returns the next "attachments" part; any other part name fails
// the whole upload. The previous part is implicitly closed by the underlying
// reader once Next is called again.
type multipartAttachments struct {
	r       *multipart.Reader
	current *multipart.Part
}

func (a *multipartAttachments) Next(ctx context.Context) (service.AttachmentPart, error) {
	if a.current != nil {
		_ = a.current.Close()
		a.current = nil
	}
	for {
		part, err := a.r.NextPart()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return service.AttachmentPart{}, io.EOF
			}
			return service.AttachmentPart{}, signalapi.WrapWithInvalidSignalMessage(
				werror.WrapWithContextParams(ctx, err, "next attachment part"),
				"attachments", err.Error(),
			)
		}
		switch part.FormName() {
		case "attachments":
			a.current = part
			return service.AttachmentPart{
				Filename:    part.FileName(),
				ContentType: part.Header.Get("Content-Type"),
				Body:        part,
			}, nil
		case "metadata":
			_ = part.Close()
			return service.AttachmentPart{}, signalapi.WrapWithInvalidSignalMessage(
				werror.ErrorWithContextParams(ctx, "duplicate metadata part"),
				"metadata", "duplicate",
			)
		default:
			_ = part.Close()
			return service.AttachmentPart{}, signalapi.WrapWithInvalidSignalMessage(
				werror.ErrorWithContextParams(ctx, "unexpected part name",
					werror.SafeParam("name", part.FormName())),
				"part", part.FormName(),
			)
		}
	}
}

// writeAccepted emits the 202 response in the envelope shape declared in
// docs/rest-api.md. The conjure-generated SignalMessageAccepted type carries
// only the id; the outer envelope is hand-rolled here because Conjure does
// not model wrapping (the IDL types are the inner `data` value).
func writeAccepted(ctx context.Context, rw http.ResponseWriter, id googleuuid.UUID) {
	accepted := signalapi.SignalMessageAccepted{Id: palantiruuid.UUID(id)}
	rw.Header().Set("Content-Type", codecs.JSON.ContentType())
	rw.WriteHeader(http.StatusAccepted)
	if err := codecs.JSON.Encode(rw, accepted); err != nil {
		svc1log.FromContext(ctx).Warn("encode accepted response", svc1log.Stacktrace(err))
	}
}

func writeError(rw http.ResponseWriter, err error) {
	var conj conjerrors.Error
	if errors.As(err, &conj) {
		conjerrors.WriteErrorResponse(rw, conj)
		return
	}
	conjerrors.WriteErrorResponse(rw, conjerrors.NewInternal())
}
