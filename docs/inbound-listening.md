# Inbound listening

Optional feature, enabled by `signalCli.enableListening=true`. When on, go-signalium captures received Signal messages from the daemon's event stream and persists them.

This is **separate** from the outbound send pipeline — no echo, no reply, no callback. The receiver is purely a sink.

## Flow

```
signal-cli daemon ── TCP 7610 ──▶ tcp_client.go reader goroutine
                                          │ demux: frame.ID == nil
                                          ▼
                                   Client.Events() <-chan Event
                                          │ subscribed
                                          ▼
                                  InboundListener.run(ctx)
                                          │
                                          ▼
                            signalium.inbound_signal_messages
```

`InboundListener` is an fx-managed component with its own goroutine, started on `OnStart`, stopped on `OnStop` by cancelling its context and waiting for the goroutine to drain in-flight events.

## Schema (M6)

```sql
CREATE TABLE signalium.inbound_signal_messages (
    inbound_message_id  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source              TEXT NOT NULL,           -- sender phone or uuid
    source_uuid         TEXT NULL,
    source_timestamp    BIGINT NOT NULL,         -- signal-cli's envelope timestamp (ms)
    group_id            TEXT NULL,               -- non-null if it was a group message
    content             TEXT NULL,               -- nullable; may be receipt/typing/etc.
    attachments         JSONB NOT NULL DEFAULT '[]'::jsonb,
    raw                 JSONB NOT NULL,          -- entire envelope, for forensics
    received_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at          TIMESTAMPTZ NULL,
    UNIQUE (source, source_timestamp)
);

CREATE INDEX idx_inbound_received_at
    ON signalium.inbound_signal_messages (received_at DESC)
    WHERE deleted_at IS NULL;
```

`UNIQUE (source, source_timestamp)` deduplicates — signal-cli sometimes redelivers events on reconnect. The listener uses `INSERT ... ON CONFLICT DO NOTHING`.

## Attachment handling for inbound

Attachments arrive as IDs in the envelope (`{id, contentType, size}`), not as bytes — signal-cli stores them on its own disk. The listener:

1. Records the metadata in `attachments` JSONB (`[{id, contentType, size, signalCliPath}]`).
2. Does NOT proactively copy them anywhere — they live in the `signal_cli_data` Docker volume.
3. If a future consumer needs them in MinIO, a downstream service can fetch via signal-cli's `getAttachment` RPC and PUT to MinIO. Out of scope for the initial cut.

## Filtering

A configurable allow/deny list in runtime.yml lets operators ignore noise (typing indicators, receipts, sync messages from the linked phone itself):

```yaml
inbound:
  enabled: true
  ignore:
    typing:        true
    receipt:       true
    syncMessage:   true    # messages this account sent from another device
    callMessage:   true
```

`InboundListener` checks these flags per event before inserting. The flags are refreshable (`palantir/pkg/refreshable`).

## Why no API for browsing inbound

The initial scope is "capture for audit/forensics" — downstream consumers, if any, query the table directly or via SQL views. Adding `GET /api/v1/inbound-signal-messages` is straightforward later (define in Conjure, generate handler, write SQL) but not required for the first cut.
