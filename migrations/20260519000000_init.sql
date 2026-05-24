-- Initial schema for the outbound Signal delivery service.
--
-- Schema lives under `signalium`.
-- The Postgres ENUM type signalium.signal_message_status is bound on the
-- Go side via internal/domain.SignalMessageStatus (Scanner/Valuer).

CREATE SCHEMA IF NOT EXISTS signalium;

CREATE TYPE signalium.signal_message_status AS ENUM (
    'PENDING',
    'SENDING',
    'SENT',
    'FAILED',
    'PERMANENT_FAILED',
    'TIMED_OUT'
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

-- Drives the outbox worker claim query.
CREATE INDEX idx_signal_messages_status_nextattempt
    ON signalium.signal_messages (status, next_attempt_at)
    WHERE deleted_at IS NULL;

-- Supports JSONB containment queries against attachments.
CREATE INDEX idx_signal_messages_attachments_gin
    ON signalium.signal_messages USING GIN (attachments);
