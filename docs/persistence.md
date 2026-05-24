# Persistence

## Schema — `signalium`

```sql
CREATE SCHEMA IF NOT EXISTS signalium;

CREATE TYPE signalium.signal_message_status AS ENUM (
    'PENDING', 'SENDING', 'SENT', 'FAILED', 'PERMANENT_FAILED', 'TIMED_OUT'
);

CREATE TABLE signalium.signal_messages (
    signal_message_id    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    external_id          TEXT NOT NULL UNIQUE,
    idempotency_key      TEXT NULL UNIQUE,
    recipient            TEXT NULL,
    group_id             TEXT NULL,
    sender_phone_number  TEXT NOT NULL,
    content              TEXT NOT NULL,
    attachments          JSONB NOT NULL DEFAULT '[]'::jsonb,
    attempts             INT NOT NULL DEFAULT 0,
    max_attempts         INT NOT NULL DEFAULT 5,
    next_attempt_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    timeout_at           TIMESTAMPTZ NULL,
    status               signalium.signal_message_status NOT NULL DEFAULT 'PENDING',
    last_error           TEXT NULL,
    result_id            VARCHAR(25) NULL,
    quote_result_id      VARCHAR(25) NULL,
    correlation_id       TEXT NOT NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    modified_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at           TIMESTAMPTZ NULL,
    CONSTRAINT exactly_one_target CHECK (
        (recipient IS NOT NULL)::int + (group_id IS NOT NULL)::int = 1
    )
);

CREATE INDEX idx_signal_messages_status_nextattempt
    ON signalium.signal_messages (status, next_attempt_at)
    WHERE deleted_at IS NULL;

CREATE INDEX idx_signal_messages_attachments_gin
    ON signalium.signal_messages USING GIN (attachments);
```

`signalium.inbound_signal_messages` (M6) captures received messages:

```sql
CREATE TABLE signalium.inbound_signal_messages (
    inbound_message_id  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source              TEXT NOT NULL,           -- sender phone or uuid
    source_uuid         TEXT NULL,
    source_timestamp    BIGINT NOT NULL,         -- signal-cli envelope timestamp (ms)
    group_id            TEXT NULL,
    content             TEXT NULL,
    attachments         JSONB NOT NULL DEFAULT '[]'::jsonb,
    raw                 JSONB NOT NULL,          -- full envelope JSON, for forensics
    received_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at          TIMESTAMPTZ NULL,
    UNIQUE (source, source_timestamp)
);

CREATE INDEX idx_inbound_received_at
    ON signalium.inbound_signal_messages (received_at DESC)
    WHERE deleted_at IS NULL;
```

`UNIQUE (source, source_timestamp)` + `INSERT ... ON CONFLICT DO NOTHING` turns signal-cli's reconnect-driven re-deliveries into idempotent no-ops. Attachment bytes stay on signal-cli's disk; the `attachments` array stores `{id, contentType, size}` metadata only. See [`inbound-listening.md`](./inbound-listening.md).

### Field notes

- `external_id` is caller-supplied — caller's message id. Required, unique.
- `idempotency_key` is optional. If supplied, repeated POSTs with the same key return the original row.
- `recipient` xor `group_id` — never both, never neither. Enforced by `exactly_one_target` CHECK.
- `sender_phone_number` is stamped by the server from install.yml; column kept on the row for audit/forensics.
- `attachments` is `JSONB` holding `[{bucket, filename, mimeType?, size?}, ...]`.
- `correlation_id` is a trace id; populated from `X-Trace-Id` header if present, otherwise generated.
- `result_id` is signal-cli's message timestamp on success. Used to dedupe its async event stream.
- `next_attempt_at` doubles as a **lease** when `status='SENDING'`: it's set to `now() + leaseDuration` on claim; another worker re-claims if the lease expires before the original worker finishes. See [`worker.md`](./worker.md).

## Status state machine

```
                  POST                                                           
                   │                                                             
                   ▼                                                             
              ┌─────────┐         claim          ┌─────────┐    SUCCESS    ┌─────┐
              │ PENDING │ ──────────────────────▶│ SENDING │ ─────────────▶│SENT │
              └─────────┘                        └────┬────┘               └─────┘
                   ▲                                  │ FAIL                     
                   │ resend (PATCH .../resend)        ▼                          
                   └──────────────────────┬─────┬─────────┐                      
                                          │     │ FAILED  │── attempts >= max ──▶ PERMANENT_FAILED
                                          │     └─────────┘                      
                                          │                                      
                                          └─────── lease expired ─────────┐      
                                                                          ▼      
                                                                       PENDING   

  timeout_at reached at any point → TIMED_OUT (terminal unless resent)            
```

`status='SENDING'` rows whose lease expired (i.e., `next_attempt_at < now()`) are reclaimable — the claim query treats them identically to PENDING.

## sqlc

`go-signalium/sqlc.yaml` (v2):

```yaml
version: "2"
sql:
  - engine: postgresql
    schema: migrations
    queries: internal/repo/queries
    gen:
      go:
        package: sqlc
        sql_package: pgx/v5
        out: internal/repo/sqlc
        emit_interface: true
        emit_pointers_for_null_types: true
        overrides:
          - column: "signalium.signal_messages.attachments"
            go_type:
              import: "github.com/olehmushka/go-signalium/internal/domain"
              type:   "Attachments"
```

`internal/domain.Attachments` is a `[]Attachment` that implements `sql.Scanner` (JSON-unmarshal from `[]byte`) and `driver.Valuer` (JSON-marshal). This gives the generated sqlc API a typed return without leaking `[]byte` to the service layer.

### Generated queries (M2)

```sql
-- name: Insert :one
INSERT INTO signalium.signal_messages (...) VALUES (...) RETURNING *;

-- name: GetByID :one
SELECT * FROM signalium.signal_messages WHERE signal_message_id = $1 AND deleted_at IS NULL;

-- name: GetByExternalID :one
SELECT * FROM signalium.signal_messages WHERE external_id = $1 AND deleted_at IS NULL;

-- name: GetByIdempotencyKey :one
SELECT * FROM signalium.signal_messages WHERE idempotency_key = $1;

-- name: ClaimPending :one
WITH claimed AS (
  SELECT signal_message_id FROM signalium.signal_messages
  WHERE deleted_at IS NULL
    AND (status = 'PENDING' OR (status = 'SENDING' AND next_attempt_at < now()))
    AND next_attempt_at <= now()
  ORDER BY next_attempt_at
  FOR UPDATE SKIP LOCKED
  LIMIT 1
)
UPDATE signalium.signal_messages m
   SET status = 'SENDING',
       attempts = attempts + 1,
       next_attempt_at = now() + ($1::interval),  -- lease duration
       modified_at = now()
  FROM claimed
 WHERE m.signal_message_id = claimed.signal_message_id
RETURNING m.*;

-- name: MarkSent :exec
UPDATE signalium.signal_messages
   SET status = 'SENT', result_id = $2, modified_at = now()
 WHERE signal_message_id = $1;

-- name: MarkFailed :exec
UPDATE signalium.signal_messages
   SET status = (CASE WHEN attempts >= max_attempts THEN 'PERMANENT_FAILED'::signalium.signal_message_status ELSE 'FAILED'::signalium.signal_message_status END),
       last_error = $2,
       next_attempt_at = $3,
       modified_at = now()
 WHERE signal_message_id = $1;

-- name: Resend :exec
UPDATE signalium.signal_messages
   SET status = 'PENDING', attempts = 0, next_attempt_at = now(), last_error = NULL, modified_at = now()
 WHERE signal_message_id = $1 AND status IN ('FAILED','TIMED_OUT','PERMANENT_FAILED');
```

The claim query treats expired-lease `SENDING` rows identically to PENDING — that's the lease + crash safety mechanism.

## pgx pool configuration

- The Postgres ENUM type `signalium.signal_message_status` is in the `signalium` schema; without an explicit search\_path on the connection, pgx's lazy type registration falls back to a text scan. The override is benign but the explicit search\_path makes the binding deterministic.
- `pool.MaxConns`, `pool.MinConns`, `pool.MaxConnLifetime`, etc. come from install.yml.
- Pool is fx-provided; lifecycle hook closes it on `OnStop`.

## Atlas migrations

- Versioned `.sql` migrations in `go-signalium/migrations/`, named `YYYYMMDDHHMMSS_<slug>.sql`. Checksum file `atlas.sum` is committed.
- Embedded into the binary via `embed.FS` (`go-signalium/migrations/migrations.go` exports `var FS embed.FS`).
- Auto-run controlled by `database.enableMigrationsAutoRun` (install.yml). When enabled, an fx.Invoke between the pool provider and the repo provider:
  1. Acquires a Postgres advisory lock (`SELECT pg_advisory_lock(<constant>)`).
  2. Runs `atlas migrate apply` from the embedded FS.
  3. Releases the advisory lock.
- The advisory lock prevents two replicas from running migrations simultaneously during a rolling deploy.

Makefile:

```
make migrate-new name=add_foo    # atlas migrate new add_foo --dir file://migrations
make migrate-up url=$(DB_URL)    # atlas migrate apply --dir file://migrations --url $(DB_URL)
make migrate-hash                # atlas migrate hash --dir file://migrations
make migrate-lint                # atlas migrate lint --dir file://migrations --dev-url <docker postgres>
```

`make migrate-lint` runs in CI.
