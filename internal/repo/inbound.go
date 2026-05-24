package repo

import (
	"context"

	werror "github.com/palantir/witchcraft-go-error"

	"github.com/olehmushka/go-signalium/internal/repo/sqlc"
)

// Inbound is the typed wrapper around sqlc.Queries for inbound_signal_messages.
// Only the write path is exposed — the listener is fire-and-forget, downstream
// consumers read directly via SQL (docs/inbound-listening.md "Why no API").
type Inbound struct {
	q *sqlc.Queries
}

// NewInbound is the fx provider.
func NewInbound(q *sqlc.Queries) *Inbound { return &Inbound{q: q} }

// InsertInboundParams is the typed input for one captured envelope. JSONB
// fields arrive as raw bytes — the listener marshals into JSON before
// calling Insert.
type InsertInboundParams struct {
	Source          string
	SourceUUID      *string
	SourceTimestamp int64
	GroupID         *string
	Content         *string
	Attachments     []byte
	Raw             []byte
}

// Insert writes one envelope with ON CONFLICT DO NOTHING semantics. Returns
// true iff a row was actually inserted (i.e. it was not a duplicate redelivery).
func (i *Inbound) Insert(ctx context.Context, p InsertInboundParams) (bool, error) {
	rows, err := i.q.InsertInbound(ctx, sqlc.InsertInboundParams{
		Source:          p.Source,
		SourceUuid:      p.SourceUUID,
		SourceTimestamp: p.SourceTimestamp,
		GroupID:         p.GroupID,
		Content:         p.Content,
		Attachments:     p.Attachments,
		Raw:             p.Raw,
	})
	if err != nil {
		return false, werror.WrapWithContextParams(ctx, err, "insert inbound message",
			werror.SafeParam("source", p.Source),
			werror.SafeParam("sourceTimestamp", p.SourceTimestamp))
	}
	return rows > 0, nil
}
