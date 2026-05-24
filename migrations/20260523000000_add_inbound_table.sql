-- Inbound listener storage (M6).
--
-- Captures asynchronous Signal events relayed by the signal-cli daemon when
-- inbound listening is enabled. UNIQUE(source, source_timestamp) plus an
-- ON CONFLICT DO NOTHING insert turns duplicate redeliveries (which signal-cli
-- can emit after reconnect) into idempotent no-ops.
--
-- `raw` carries the entire envelope JSON so forensic queries can reach fields
-- the typed columns don't expose. `attachments` mirrors signal-cli's envelope
-- shape: an array of `{id, contentType, size}` blobs — bytes still live on the
-- daemon's disk; no proactive copy to MinIO. See docs/inbound-listening.md.

CREATE TABLE signalium.inbound_signal_messages (
    inbound_message_id  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source              TEXT NOT NULL,
    source_uuid         TEXT NULL,
    source_timestamp    BIGINT NOT NULL,
    group_id            TEXT NULL,
    content             TEXT NULL,
    attachments         JSONB NOT NULL DEFAULT '[]'::jsonb,
    raw                 JSONB NOT NULL,
    received_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at          TIMESTAMPTZ NULL,
    UNIQUE (source, source_timestamp)
);

CREATE INDEX idx_inbound_received_at
    ON signalium.inbound_signal_messages (received_at DESC)
    WHERE deleted_at IS NULL;
