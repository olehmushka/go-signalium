// Package repo is the persistence-adapter layer. It wraps the sqlc-generated
// Queries surface, converting pgtype values to/from the plain Go shapes the
// service and worker work with (domain.SignalMessage, time.Time, uuid.UUID,
// strings). Sqlc-generated code stays untouched in internal/repo/sqlc/.
//
// All ErrNoRows results map to domain.ErrNotFound so handlers can compare
// with errors.Is.
package repo

import (
	"context"
	"errors" //nolint:depguard // stdlib errors.Is is the canonical sentinel check
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	werror "github.com/palantir/witchcraft-go-error"
	"go.uber.org/fx"

	"github.com/olehmushka/go-signalium/internal/domain"
	"github.com/olehmushka/go-signalium/internal/repo/sqlc"
)

// Module wires the repo. It depends only on *sqlc.Queries which db.Module
// already provides.
var Module = fx.Module(
	"repo",
	fx.Provide(
		NewMessages,
		NewInbound,
	),
)

// Messages is the typed wrapper around sqlc.Queries for signal_messages. The
// service and worker depend on small interfaces they own; this is the only
// concrete implementation.
type Messages struct {
	q *sqlc.Queries
}

// NewMessages is the fx provider.
func NewMessages(q *sqlc.Queries) *Messages { return &Messages{q: q} }

// InsertParams captures everything the handler/service needs to insert.
type InsertParams struct {
	ExternalID        string
	IdempotencyKey    *string
	Recipient         *string
	GroupID           *string
	SenderPhoneNumber string
	Content           string
	Attachments       domain.Attachments
	MaxAttempts       int32
	TimeoutAt         *time.Time
	CorrelationID     string
}

// Insert persists a new row in PENDING state and returns the materialised
// domain row. The DB CHECK constraint enforces exactly-one-of recipient/group
// so the wrapper does not duplicate that validation.
func (r *Messages) Insert(ctx context.Context, p InsertParams) (domain.SignalMessage, error) {
	row, err := r.q.Insert(ctx, sqlc.InsertParams{
		ExternalID:        p.ExternalID,
		IdempotencyKey:    p.IdempotencyKey,
		Recipient:         p.Recipient,
		GroupID:           p.GroupID,
		SenderPhoneNumber: p.SenderPhoneNumber,
		Content:           p.Content,
		Attachments:       p.Attachments,
		MaxAttempts:       p.MaxAttempts,
		TimeoutAt:         tsTz(p.TimeoutAt),
		CorrelationID:     p.CorrelationID,
	})
	if err != nil {
		return domain.SignalMessage{}, werror.WrapWithContextParams(ctx, err, "insert signal message",
			werror.SafeParam("externalId", p.ExternalID))
	}
	return rowToDomain(row), nil
}

// GetByID returns the row for the given UUID; ErrNotFound on miss.
func (r *Messages) GetByID(ctx context.Context, id uuid.UUID) (domain.SignalMessage, error) {
	row, err := r.q.GetByID(ctx, uuidToPg(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.SignalMessage{}, domain.ErrNotFound
		}
		return domain.SignalMessage{}, werror.WrapWithContextParams(ctx, err, "get by id",
			werror.SafeParam("id", id.String()))
	}
	return rowToDomain(row), nil
}

// GetByExternalID returns the row for the caller-supplied external_id.
func (r *Messages) GetByExternalID(ctx context.Context, externalID string) (domain.SignalMessage, error) {
	row, err := r.q.GetByExternalID(ctx, externalID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.SignalMessage{}, domain.ErrNotFound
		}
		return domain.SignalMessage{}, werror.WrapWithContextParams(ctx, err, "get by external id")
	}
	return rowToDomain(row), nil
}

// GetByIdempotencyKey returns the row for the caller-supplied dedup key.
func (r *Messages) GetByIdempotencyKey(ctx context.Context, key string) (domain.SignalMessage, error) {
	row, err := r.q.GetByIdempotencyKey(ctx, &key)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.SignalMessage{}, domain.ErrNotFound
		}
		return domain.SignalMessage{}, werror.WrapWithContextParams(ctx, err, "get by idempotency key")
	}
	return rowToDomain(row), nil
}

// ClaimPending atomically transitions one eligible row from PENDING (or
// expired-lease SENDING) to SENDING, incrementing attempts and writing a new
// next_attempt_at. Returns ErrNotFound when no row is available.
func (r *Messages) ClaimPending(ctx context.Context, lease time.Duration) (domain.SignalMessage, error) {
	row, err := r.q.ClaimPending(ctx, intervalFromDuration(lease))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.SignalMessage{}, domain.ErrNotFound
		}
		return domain.SignalMessage{}, werror.WrapWithContextParams(ctx, err, "claim pending")
	}
	return rowToDomain(row), nil
}

// MarkSent transitions a row to SENT and stamps the signal-cli result id.
func (r *Messages) MarkSent(ctx context.Context, id uuid.UUID, resultID string) error {
	if err := r.q.MarkSent(ctx, sqlc.MarkSentParams{
		ResultID: &resultID,
		ID:       uuidToPg(id),
	}); err != nil {
		return werror.WrapWithContextParams(ctx, err, "mark sent",
			werror.SafeParam("id", id.String()))
	}
	return nil
}

// MarkFailed records the latest error, schedules the next attempt, and lets
// the SQL CASE flip status to PERMANENT_FAILED when attempts >= max_attempts.
func (r *Messages) MarkFailed(ctx context.Context, id uuid.UUID, lastErr string, nextAttemptAt time.Time) error {
	if err := r.q.MarkFailed(ctx, sqlc.MarkFailedParams{
		LastError:     &lastErr,
		NextAttemptAt: tsTz(&nextAttemptAt),
		ID:            uuidToPg(id),
	}); err != nil {
		return werror.WrapWithContextParams(ctx, err, "mark failed",
			werror.SafeParam("id", id.String()))
	}
	return nil
}

// Resend resets a terminal-failed row back to PENDING for another attempt.
// Returns nil when no row matches the terminal-status guard — the handler
// re-reads the row after calling Resend and surfaces the (unchanged) state.
func (r *Messages) Resend(ctx context.Context, id uuid.UUID) error {
	if err := r.q.Resend(ctx, uuidToPg(id)); err != nil {
		return werror.WrapWithContextParams(ctx, err, "resend",
			werror.SafeParam("id", id.String()))
	}
	return nil
}

// UpdateStatus rewrites the status column for the given id and returns the
// updated row. ErrNotFound when the id does not exist (or is soft-deleted).
func (r *Messages) UpdateStatus(ctx context.Context, id uuid.UUID, status domain.SignalMessageStatus) (domain.SignalMessage, error) {
	row, err := r.q.UpdateStatus(ctx, sqlc.UpdateStatusParams{
		Status: status,
		ID:     uuidToPg(id),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.SignalMessage{}, domain.ErrNotFound
		}
		return domain.SignalMessage{}, werror.WrapWithContextParams(ctx, err, "update status",
			werror.SafeParam("id", id.String()))
	}
	return rowToDomain(row), nil
}

// ListFilters mirrors the REST query params for GET /api/v1/signal-messages.
// Zero-value fields skip the corresponding WHERE clause; Limit/Offset default
// to 50/0 at the handler edge so the repo layer stays pure data-access.
type ListFilters struct {
	Q             *string
	Status        *domain.SignalMessageStatus
	Attempts      *int32
	CreatedAtFrom *time.Time
	CreatedAtTo   *time.Time
	Limit         int32
	Offset        int32
}

// List returns a page of rows ordered by created_at desc. Filter rules are
// per ListFilters; the wrapper translates them into sqlc.ListParams.
func (r *Messages) List(ctx context.Context, f ListFilters) ([]domain.SignalMessage, error) {
	rows, err := r.q.List(ctx, sqlc.ListParams{
		Q:             f.Q,
		StatusFilter:  domainStatusToSQLC(f.Status),
		Attempts:      f.Attempts,
		CreatedAtFrom: tsTz(f.CreatedAtFrom),
		CreatedAtTo:   tsTz(f.CreatedAtTo),
		OffsetCount:   f.Offset,
		LimitCount:    f.Limit,
	})
	if err != nil {
		return nil, werror.WrapWithContextParams(ctx, err, "list signal messages")
	}
	out := make([]domain.SignalMessage, 0, len(rows))
	for _, row := range rows {
		out = append(out, rowToDomain(row))
	}
	return out, nil
}

// Count returns the total number of rows matching the same filters as List.
// Handlers call it alongside List to populate SignalMessageList.Total.
func (r *Messages) Count(ctx context.Context, f ListFilters) (int, error) {
	n, err := r.q.Count(ctx, sqlc.CountParams{
		Q:             f.Q,
		StatusFilter:  domainStatusToSQLC(f.Status),
		Attempts:      f.Attempts,
		CreatedAtFrom: tsTz(f.CreatedAtFrom),
		CreatedAtTo:   tsTz(f.CreatedAtTo),
	})
	if err != nil {
		return 0, werror.WrapWithContextParams(ctx, err, "count signal messages")
	}
	return int(n), nil
}

// StatusCount is one row of the per-status histogram returned by StatsCounts.
type StatusCount struct {
	Status domain.SignalMessageStatus
	Count  int
}

// StatsCounts returns the COUNT(*) per status across the entire (non-deleted)
// table. The handler folds these into SignalMessageStats.Counts.
func (r *Messages) StatsCounts(ctx context.Context) ([]StatusCount, error) {
	rows, err := r.q.StatsCounts(ctx)
	if err != nil {
		return nil, werror.WrapWithContextParams(ctx, err, "stats counts")
	}
	out := make([]StatusCount, 0, len(rows))
	for _, row := range rows {
		out = append(out, StatusCount{Status: row.Status, Count: int(row.Counted)})
	}
	return out, nil
}

// DayCount is one bucket in the per-day time series. Date is a YYYY-MM-DD
// string keyed on UTC date_trunc('day', created_at).
type DayCount struct {
	Date   string
	Sent   int
	Failed int
}

// StatsPerDay returns a 30-day rolling window grouped by UTC day.
func (r *Messages) StatsPerDay(ctx context.Context) ([]DayCount, error) {
	rows, err := r.q.StatsPerDay(ctx)
	if err != nil {
		return nil, werror.WrapWithContextParams(ctx, err, "stats per-day")
	}
	out := make([]DayCount, 0, len(rows))
	for _, row := range rows {
		out = append(out, DayCount{Date: row.Day, Sent: int(row.Sent), Failed: int(row.Failed)})
	}
	return out, nil
}

// domainStatusToSQLC bridges the domain status pointer used in ListFilters
// to the sqlc-generated newtype that the narg parameter expects. Both are
// `type X string` under the hood but Go requires an explicit conversion.
func domainStatusToSQLC(s *domain.SignalMessageStatus) *sqlc.SignaliumSignalMessageStatus {
	if s == nil {
		return nil
	}
	v := sqlc.SignaliumSignalMessageStatus(*s)
	return &v
}

func rowToDomain(row *sqlc.SignaliumSignalMessage) domain.SignalMessage {
	return domain.SignalMessage{
		ID:                pgToUUID(row.ID),
		ExternalID:        row.ExternalID,
		IdempotencyKey:    row.IdempotencyKey,
		Recipient:         row.Recipient,
		GroupID:           row.GroupID,
		SenderPhoneNumber: row.SenderPhoneNumber,
		Content:           row.Content,
		Attachments:       row.Attachments,
		Attempts:          int(row.Attempts),
		MaxAttempts:       int(row.MaxAttempts),
		NextAttemptAt:     pgToTime(row.NextAttemptAt),
		TimeoutAt:         pgToTimePtr(row.TimeoutAt),
		Status:            row.Status,
		LastError:         row.LastError,
		ResultID:          row.ResultID,
		QuoteResultID:     row.QuoteResultID,
		CorrelationID:     row.CorrelationID,
		CreatedAt:         pgToTime(row.CreatedAt),
		ModifiedAt:        pgToTime(row.ModifiedAt),
	}
}

func uuidToPg(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

func pgToUUID(v pgtype.UUID) uuid.UUID {
	if !v.Valid {
		return uuid.Nil
	}
	return uuid.UUID(v.Bytes)
}

func tsTz(t *time.Time) pgtype.Timestamptz {
	if t == nil || t.IsZero() {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

func pgToTime(v pgtype.Timestamptz) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return v.Time
}

func pgToTimePtr(v pgtype.Timestamptz) *time.Time {
	if !v.Valid {
		return nil
	}
	t := v.Time
	return &t
}

func intervalFromDuration(d time.Duration) pgtype.Interval {
	return pgtype.Interval{Microseconds: d.Microseconds(), Valid: true}
}
