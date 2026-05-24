package handler

import (
	"github.com/palantir/pkg/datetime"
	"github.com/palantir/pkg/safelong"
	palantiruuid "github.com/palantir/pkg/uuid"

	"github.com/olehmushka/go-signalium/internal/domain"
	signalapi "github.com/olehmushka/go-signalium/internal/generated/signalium/api"
)

// domainToConjure converts the plain domain row to the conjure-generated
// SignalMessageInfo type returned by the operational endpoints. Attachments
// are eagerly converted; nil slices become an empty (non-nil) list so the
// JSON encoder emits `"attachments": []` rather than null.
func domainToConjure(m domain.SignalMessage) signalapi.SignalMessageInfo {
	info := signalapi.SignalMessageInfo{
		Id:                palantiruuid.UUID(m.ID),
		ExternalId:        m.ExternalID,
		IdempotencyKey:    m.IdempotencyKey,
		Recipient:         m.Recipient,
		GroupId:           m.GroupID,
		SenderPhoneNumber: m.SenderPhoneNumber,
		Content:           m.Content,
		Attachments:       toConjureAttachments(m.Attachments),
		Attempts:          m.Attempts,
		MaxAttempts:       m.MaxAttempts,
		NextAttemptAt:     datetime.DateTime(m.NextAttemptAt),
		Status:            signalapi.New_SignalMessageStatus(signalapi.SignalMessageStatus_Value(m.Status)),
		LastError:         m.LastError,
		ResultId:          m.ResultID,
		QuoteResultId:     m.QuoteResultID,
		CorrelationId:     m.CorrelationID,
		CreatedAt:         datetime.DateTime(m.CreatedAt),
		ModifiedAt:        datetime.DateTime(m.ModifiedAt),
	}
	if m.TimeoutAt != nil {
		t := datetime.DateTime(*m.TimeoutAt)
		info.TimeoutAt = &t
	}
	return info
}

// toConjureAttachments maps the JSONB-backed domain slice to the conjure
// Attachment list. Empty input always returns an empty (non-nil) slice.
func toConjureAttachments(in domain.Attachments) []signalapi.Attachment {
	out := make([]signalapi.Attachment, 0, len(in))
	for _, a := range in {
		converted := signalapi.Attachment{
			Bucket:   a.Bucket,
			Filename: a.Filename,
		}
		if a.MimeType != "" {
			mime := a.MimeType
			converted.MimeType = &mime
		}
		if a.Size > 0 {
			if size, err := safelong.NewSafeLong(a.Size); err == nil {
				converted.Size = &size
			}
		}
		out = append(out, converted)
	}
	return out
}
