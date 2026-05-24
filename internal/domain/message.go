package domain

import (
	"time"

	"github.com/google/uuid"
)

// SignalMessage is the plain Go view of a row in signalium.signal_messages.
// It is the lingua franca between the repo, service, handler, and worker
// layers — nothing in those packages imports sqlc-generated pgtype values.
type SignalMessage struct {
	ID                uuid.UUID
	ExternalID        string
	IdempotencyKey    *string
	Recipient         *string
	GroupID           *string
	SenderPhoneNumber string
	Content           string
	Attachments       Attachments
	Attempts          int
	MaxAttempts       int
	NextAttemptAt     time.Time
	TimeoutAt         *time.Time
	Status            SignalMessageStatus
	LastError         *string
	ResultID          *string
	QuoteResultID     *string
	CorrelationID     string
	CreatedAt         time.Time
	ModifiedAt        time.Time
}
