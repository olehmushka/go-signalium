//go:build integration

// Integration tests for the repo layer. They boot a Postgres testcontainer,
// apply the embedded migrations, and exercise every sqlc query through the
// typed wrappers in repo.Messages / repo.Inbound. Unit tests (in-memory repo
// in worker/e2e_test.go) cannot catch schema/ENUM/index drift between the
// .sql files and the generated code — these can.
//
// Gated by the `integration` build tag so `go test ./...` stays hermetic and
// fast. Run with `make integration-test` (Docker required).

package repo_test

import (
	"context"
	stdfs "io/fs"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/olehmushka/go-signalium/internal/domain"
	"github.com/olehmushka/go-signalium/internal/repo"
	"github.com/olehmushka/go-signalium/internal/repo/sqlc"
	"github.com/olehmushka/go-signalium/migrations"
)

// containerImage pins the Postgres version. Bump in lockstep with prod.
const containerImage = "postgres:16-alpine"

// setupDB spins one Postgres container per test, applies migrations, and
// returns a pool. t.Cleanup tears the container down.
//
// Per-test (not per-package) isolation is the simplest correct choice: each
// test starts from a clean schema, no ordering coupling, no truncation logic
// to maintain. The container start is ~2s on warm Docker; the full suite
// stays well under a minute.
func setupDB(tb testing.TB) *pgxpool.Pool {
	tb.Helper()
	ctx := context.Background()

	ctr, err := postgres.Run(ctx, containerImage,
		postgres.WithDatabase("signalium_test"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	require.NoError(tb, err, "start postgres container")
	tb.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = ctr.Terminate(shutdownCtx)
	})

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	require.NoError(tb, err, "connection string")

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(tb, err, "open pool")
	tb.Cleanup(pool.Close)

	require.NoError(tb, applyMigrations(ctx, pool), "apply migrations")
	return pool
}

// applyMigrations runs every embedded .sql file in lexicographic order. It
// deliberately skips the atlas.sum verification that production startup does
// — the test cares about schema correctness, not checksum bookkeeping.
func applyMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	entries, err := stdfs.Glob(migrations.FS, "*.sql")
	if err != nil {
		return err
	}
	sort.Strings(entries)
	for _, name := range entries {
		body, err := migrations.FS.ReadFile(name)
		if err != nil {
			return err
		}
		if _, err := pool.Exec(ctx, string(body)); err != nil {
			return err
		}
	}
	return nil
}

func newMessages(tb testing.TB, pool *pgxpool.Pool) *repo.Messages {
	tb.Helper()
	return repo.NewMessages(sqlc.New(pool))
}

func newInbound(tb testing.TB, pool *pgxpool.Pool) *repo.Inbound {
	tb.Helper()
	return repo.NewInbound(sqlc.New(pool))
}

func strPtr(s string) *string { return &s }

func newInsertParams(externalID, recipient string) repo.InsertParams {
	return repo.InsertParams{
		ExternalID:        externalID,
		Recipient:         strPtr(recipient),
		SenderPhoneNumber: "+380000000000",
		Content:           "hello",
		Attachments:       domain.Attachments{},
		MaxAttempts:       3,
		CorrelationID:     uuid.NewString(),
	}
}

// ---- signal_messages ----------------------------------------------------

func TestMessages_InsertAndGet(t *testing.T) {
	pool := setupDB(t)
	r := newMessages(t, pool)
	ctx := t.Context()

	inserted, err := r.Insert(ctx, newInsertParams("ext-1", "+380111111111"))
	require.NoError(t, err)
	assert.Equal(t, "ext-1", inserted.ExternalID)
	assert.Equal(t, domain.StatusPending, inserted.Status)
	assert.NotEqual(t, uuid.Nil, inserted.ID)

	got, err := r.GetByID(ctx, inserted.ID)
	require.NoError(t, err)
	assert.Equal(t, inserted.ID, got.ID)
	assert.Equal(t, inserted.ExternalID, got.ExternalID)
}

func TestMessages_GetByID_NotFound(t *testing.T) {
	pool := setupDB(t)
	r := newMessages(t, pool)

	_, err := r.GetByID(t.Context(), uuid.New())
	require.ErrorIs(t, err, domain.ErrNotFound)
}

func TestMessages_GetByExternalID(t *testing.T) {
	pool := setupDB(t)
	r := newMessages(t, pool)
	ctx := t.Context()

	_, err := r.Insert(ctx, newInsertParams("ext-42", "+380111111111"))
	require.NoError(t, err)

	got, err := r.GetByExternalID(ctx, "ext-42")
	require.NoError(t, err)
	assert.Equal(t, "ext-42", got.ExternalID)

	_, err = r.GetByExternalID(ctx, "missing")
	require.ErrorIs(t, err, domain.ErrNotFound)
}

func TestMessages_GetByIdempotencyKey(t *testing.T) {
	pool := setupDB(t)
	r := newMessages(t, pool)
	ctx := t.Context()

	p := newInsertParams("ext-idem", "+380111111111")
	p.IdempotencyKey = strPtr("idem-key-1")
	_, err := r.Insert(ctx, p)
	require.NoError(t, err)

	got, err := r.GetByIdempotencyKey(ctx, "idem-key-1")
	require.NoError(t, err)
	assert.Equal(t, "ext-idem", got.ExternalID)

	_, err = r.GetByIdempotencyKey(ctx, "missing-key")
	require.ErrorIs(t, err, domain.ErrNotFound)
}

func TestMessages_ClaimPending_ClaimsAndReturnsEmptyAfter(t *testing.T) {
	pool := setupDB(t)
	r := newMessages(t, pool)
	ctx := t.Context()

	inserted, err := r.Insert(ctx, newInsertParams("ext-claim", "+380111111111"))
	require.NoError(t, err)

	claimed, err := r.ClaimPending(ctx, 30*time.Second)
	require.NoError(t, err)
	assert.Equal(t, inserted.ID, claimed.ID)
	assert.Equal(t, domain.StatusSending, claimed.Status)
	assert.Equal(t, 1, claimed.Attempts)
	assert.True(t, claimed.NextAttemptAt.After(time.Now()), "lease should push next_attempt_at into future")

	// Same call again: no rows eligible, so we should see ErrNotFound.
	_, err = r.ClaimPending(ctx, 30*time.Second)
	require.ErrorIs(t, err, domain.ErrNotFound)
}

func TestMessages_ClaimPending_ReclaimsExpiredLease(t *testing.T) {
	pool := setupDB(t)
	r := newMessages(t, pool)
	ctx := t.Context()

	inserted, err := r.Insert(ctx, newInsertParams("ext-relock", "+380111111111"))
	require.NoError(t, err)

	// Claim with a 1ms lease so it expires immediately.
	first, err := r.ClaimPending(ctx, time.Millisecond)
	require.NoError(t, err)
	require.Equal(t, inserted.ID, first.ID)

	// Give the lease a moment to fall into the past.
	time.Sleep(50 * time.Millisecond)

	second, err := r.ClaimPending(ctx, 30*time.Second)
	require.NoError(t, err, "expired-lease SENDING row should be re-claimable")
	assert.Equal(t, inserted.ID, second.ID)
	assert.Equal(t, 2, second.Attempts, "claim must bump attempts on re-claim")
}

func TestMessages_MarkSent(t *testing.T) {
	pool := setupDB(t)
	r := newMessages(t, pool)
	ctx := t.Context()

	inserted, err := r.Insert(ctx, newInsertParams("ext-sent", "+380111111111"))
	require.NoError(t, err)
	_, err = r.ClaimPending(ctx, 30*time.Second)
	require.NoError(t, err)

	require.NoError(t, r.MarkSent(ctx, inserted.ID, "1700000001"))

	got, err := r.GetByID(ctx, inserted.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusSent, got.Status)
	require.NotNil(t, got.ResultID)
	assert.Equal(t, "1700000001", *got.ResultID)
}

func TestMessages_MarkFailed_FailedWhenUnderMax(t *testing.T) {
	pool := setupDB(t)
	r := newMessages(t, pool)
	ctx := t.Context()

	p := newInsertParams("ext-fail-soft", "+380111111111")
	p.MaxAttempts = 3
	inserted, err := r.Insert(ctx, p)
	require.NoError(t, err)

	_, err = r.ClaimPending(ctx, 30*time.Second)
	require.NoError(t, err)
	next := time.Now().UTC().Add(time.Minute)
	require.NoError(t, r.MarkFailed(ctx, inserted.ID, "boom-1", next))

	got, err := r.GetByID(ctx, inserted.ID)
	require.NoError(t, err)
	// attempts=1 < max=3, so the CASE branch picks FAILED, not PERMANENT_FAILED.
	assert.Equal(t, domain.StatusFailed, got.Status)
	assert.Equal(t, 1, got.Attempts)
	require.NotNil(t, got.LastError)
	assert.Equal(t, "boom-1", *got.LastError)
}

func TestMessages_MarkFailed_PromotesToPermanentAtMax(t *testing.T) {
	pool := setupDB(t)
	r := newMessages(t, pool)
	ctx := t.Context()

	// MaxAttempts=1 makes the first failure terminal: claim bumps attempts to
	// 1, MarkFailed sees attempts >= max, CASE picks PERMANENT_FAILED. This
	// exercises the terminal branch without needing the lease-expiry rollover
	// dance the worker uses to grow attempts past 1.
	p := newInsertParams("ext-fail-terminal", "+380111111111")
	p.MaxAttempts = 1
	inserted, err := r.Insert(ctx, p)
	require.NoError(t, err)

	_, err = r.ClaimPending(ctx, 30*time.Second)
	require.NoError(t, err)
	require.NoError(t, r.MarkFailed(ctx, inserted.ID, "boom-final", time.Now().UTC().Add(time.Minute)))

	got, err := r.GetByID(ctx, inserted.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusPermanentFailed, got.Status)
	assert.Equal(t, 1, got.Attempts)
}

func TestMessages_ClaimPending_GrowsAttemptsAcrossLeaseExpiries(t *testing.T) {
	pool := setupDB(t)
	r := newMessages(t, pool)
	ctx := t.Context()

	// The production "attempts > 1" path: row stays SENDING because the worker
	// crashed mid-dispatch; the lease expires; another worker re-claims and
	// the claim bumps attempts. Loop that twice to grow attempts to 3.
	inserted, err := r.Insert(ctx, newInsertParams("ext-relock-grow", "+380111111111"))
	require.NoError(t, err)

	for i := 1; i <= 3; i++ {
		claimed, err := r.ClaimPending(ctx, time.Millisecond)
		require.NoError(t, err)
		assert.Equal(t, inserted.ID, claimed.ID)
		assert.Equal(t, i, claimed.Attempts, "attempt counter should climb on each re-claim")
		// Wait for the 1ms lease to fall into the past before the next claim.
		time.Sleep(20 * time.Millisecond)
	}
}

func TestMessages_Resend_RestoresPendingFromTerminal(t *testing.T) {
	pool := setupDB(t)
	r := newMessages(t, pool)
	ctx := t.Context()

	inserted, err := r.Insert(ctx, newInsertParams("ext-resend", "+380111111111"))
	require.NoError(t, err)
	_, err = r.ClaimPending(ctx, 30*time.Second)
	require.NoError(t, err)
	require.NoError(t, r.MarkFailed(ctx, inserted.ID, "boom", time.Now().Add(time.Hour)))

	got, err := r.GetByID(ctx, inserted.ID)
	require.NoError(t, err)
	require.Equal(t, domain.StatusFailed, got.Status)

	require.NoError(t, r.Resend(ctx, inserted.ID))
	got, err = r.GetByID(ctx, inserted.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusPending, got.Status)
	assert.Equal(t, 0, got.Attempts, "Resend must reset attempt counter")
	assert.Nil(t, got.LastError, "Resend must clear last_error")
}

func TestMessages_Resend_NoopOnNonTerminal(t *testing.T) {
	pool := setupDB(t)
	r := newMessages(t, pool)
	ctx := t.Context()

	inserted, err := r.Insert(ctx, newInsertParams("ext-resend-noop", "+380111111111"))
	require.NoError(t, err)

	// PENDING is not in (FAILED, TIMED_OUT, PERMANENT_FAILED) → Resend should
	// be a silent no-op (the row is already runnable).
	require.NoError(t, r.Resend(ctx, inserted.ID))

	got, err := r.GetByID(ctx, inserted.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusPending, got.Status)
	assert.Equal(t, 0, got.Attempts)
}

func TestMessages_RecipientXorGroupConstraint(t *testing.T) {
	pool := setupDB(t)
	r := newMessages(t, pool)
	ctx := t.Context()

	// Neither recipient nor group_id → CHECK violation.
	p := newInsertParams("ext-neither", "")
	p.Recipient = nil
	_, err := r.Insert(ctx, p)
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "exactly_one_target")

	// Both → also a violation.
	p2 := newInsertParams("ext-both", "+380111111111")
	p2.GroupID = strPtr("group-abc")
	_, err = r.Insert(ctx, p2)
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "exactly_one_target")
}

func TestMessages_StatsCounts(t *testing.T) {
	pool := setupDB(t)
	r := newMessages(t, pool)
	ctx := t.Context()

	// Drive one message all the way to SENT first (claim, then MarkSent the
	// claimed row), then insert a fresh PENDING row that the claim hasn't
	// touched. Doing it in this order avoids the trap where ClaimPending picks
	// whichever row has the earliest next_attempt_at — we know there's only
	// one candidate when we call it.
	toSend, err := r.Insert(ctx, newInsertParams("ext-stats-1", "+380111111111"))
	require.NoError(t, err)
	claimed, err := r.ClaimPending(ctx, 30*time.Second)
	require.NoError(t, err)
	require.Equal(t, toSend.ID, claimed.ID)
	require.NoError(t, r.MarkSent(ctx, claimed.ID, "1700000001"))

	_, err = r.Insert(ctx, newInsertParams("ext-stats-2", "+380222222222"))
	require.NoError(t, err)

	counts, err := r.StatsCounts(ctx)
	require.NoError(t, err)

	got := map[domain.SignalMessageStatus]int{}
	for _, c := range counts {
		got[c.Status] = c.Count
	}
	assert.Equal(t, 1, got[domain.StatusPending])
	assert.Equal(t, 1, got[domain.StatusSent])
}

// ---- inbound_signal_messages -------------------------------------------

func TestInbound_Insert_DedupesViaConflict(t *testing.T) {
	pool := setupDB(t)
	in := newInbound(t, pool)
	ctx := t.Context()

	p := repo.InsertInboundParams{
		Source:          "+380111111111",
		SourceTimestamp: 1700000000000,
		Content:         strPtr("hi"),
		Attachments:     []byte("[]"),
		Raw:             []byte(`{"envelope":{"timestamp":1700000000000}}`),
	}

	wrote, err := in.Insert(ctx, p)
	require.NoError(t, err)
	assert.True(t, wrote, "first insert should write a row")

	wrote, err = in.Insert(ctx, p)
	require.NoError(t, err)
	assert.False(t, wrote, "duplicate (source, source_timestamp) should be ignored via ON CONFLICT DO NOTHING")
}

func TestInbound_Insert_DistinctTimestampsWrite(t *testing.T) {
	pool := setupDB(t)
	in := newInbound(t, pool)
	ctx := t.Context()

	for i := int64(0); i < 3; i++ {
		wrote, err := in.Insert(ctx, repo.InsertInboundParams{
			Source:          "+380111111111",
			SourceTimestamp: 1700000000000 + i,
			Content:         strPtr("msg"),
			Attachments:     []byte("[]"),
			Raw:             []byte(`{}`),
		})
		require.NoError(t, err)
		assert.True(t, wrote)
	}
}
