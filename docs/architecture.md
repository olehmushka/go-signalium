# Architecture

## System sketch

```
                              ┌──────────────────────────────────────────────┐
                              │                  go-signalium                │
                              │                                              │
   POST /api/v1/signal-       │  ┌──────────┐   ┌────────┐   ┌────────────┐  │
   messages  (multipart) ────▶│  │ multipart│──▶│ Send   │──▶│ Postgres   │  │
                              │  │ handler  │   │Service │   │ signalium. │  │
                              │  └────┬─────┘   └────────┘   │ signal_msg │  │
                              │       │ stream                └────────────┘  │
                              │       ▼                            ▲          │
                              │  ┌──────────┐                       │ claim   │
                              │  │  MinIO   │           ┌───────────┴───┐    │
                              │  │ (local)  │           │ Outbox Worker │    │
                              │  └──────────┘           │  (goroutine)  │    │
                              │       ▲                 └───────┬───────┘    │
                              │       │ download                │            │
                              │       │                         ▼            │
                              │  ┌────┴──────────┐    ┌──────────────────┐   │
                              │  │ signal-cli    │◀───│ signal-cli       │   │
                              │  │ TCP 7610      │    │ TCP client       │   │
                              │  │ HTTP 7611     │    │ (JSON-RPC)       │   │
                              │  └───────────────┘    └────────┬─────────┘   │
                              │       inbound events           │             │
                              │  ┌─────────────────────────────┘             │
                              │  ▼                                           │
                              │ Inbound Listener ─▶ Postgres                 │
                              └──────────────────────────────────────────────┘
```

The service is structured around two halves connected through Postgres:

- The **accept path** (REST handler → `SendMessageService` → Postgres) persists work and returns `202` immediately.
- The **delivery path** (outbox worker → `signal-cli` → `MarkSent` / `MarkFailed`) drains the outbox asynchronously.

Decoupling acceptance from delivery is what lets the service absorb signal-cli outages, restart cleanly, and scale horizontally without a coordinator.

## fx graph

fx owns `main` and process lifecycle. Witchcraft is a fx-managed component, not the program entry point.

```
fx.New(
    config.Module,        // install.yml + runtime.yml -> typed config
    db.Module,            // pgx pool, sqlc Queries, advisory-lock migration runner
    storage.Module,       // MinIO client + bucket bootstrap
    signal.Module,        // signal-cli TCP + HTTP clients
    slack.Module,         // optional notifier (refreshable enabled flag)
    service.Module,       // Send / Resend / Sender / ResultConsumer / InboundListener / Stats / Groups
    handler.Module,       // conjure handler impls + raw multipart handler
    server.Module,        // witchcraft.Server provider + lifecycle hook
    worker.Module,        // outbox worker + cron lifecycle hooks
).Run()
```

**Witchcraft inversion.** Witchcraft normally owns `main`. To put fx on top:

- Construct `*witchcraft.Server` inside an fx provider with `WithDisableSigQuitHandler()` so witchcraft does not install its own SIGINT/SIGTERM handler.
- Register routes in the `WithInitFunc(...)` closure — both conjure-generated handlers (`signalapi.RegisterRoutesSignalMessagesService(info.Router, impl)`) and one raw `wrouter.RouteHandler` for the multipart upload (`info.Router.Post("/api/v1/signal-messages", multipartHandler)`).
- fx.Invoke wires `OnStart` → `go srv.Start()` (with an error channel that triggers `fx.Shutdowner.Shutdown(fx.ExitCode(1))` on fatal error) and `OnStop` → `srv.Shutdown(ctx)`.
- Boot-time logs use a fx-provided `svc1log.Logger`; request-scoped logs use `svc1log.FromContext(ctx)` populated by witchcraft middleware. They are different instances by design — see [decisions/0005](./decisions/0005-fx-wrapping-witchcraft.md).

If signal-handling proves problematic, the fallback topology (witchcraft owns `main`, a small fx container constructed inside `InitFunc`) is documented as the contingency.

## Request lifecycle: POST /api/v1/signal-messages

1. Client posts `multipart/form-data` with a `metadata` JSON part and zero or more `attachments` file parts.
2. The raw `wrouter.RouteHandler` calls `req.MultipartReader()` for true streaming.
3. JSON metadata is decoded into a conjure-generated request type (the schema is shared with the JSON endpoints).
4. Each file part is streamed via `minio.Client.PutObject(...)` with `objectSize=-1` (triggers MinIO multipart upload internally).
5. `SendMessageService.Enqueue(ctx, req, attachmentRefs)`:
   - Server stamps `senderPhoneNumber` from configured value if request omitted it; rejects if request provides a different value.
   - Checks `idempotency_key` — if a row with the same key exists, returns its `id` without inserting.
   - Inserts a `PENDING` row with the attachment refs in the `attachments` JSONB.
6. Handler returns `202 Accepted` with the new `signal_message_id`.

## Send pipeline: Outbox worker

A single fx-managed goroutine runs the loop:

1. **Claim** — `BEGIN; SELECT ... FROM signalium.signal_messages WHERE status='PENDING' AND next_attempt_at <= now() ORDER BY next_attempt_at FOR UPDATE SKIP LOCKED LIMIT N; UPDATE ... SET status='SENDING', attempts=attempts+1, next_attempt_at=now()+lease, modified_at=now(); COMMIT;` — the lease handles ungraceful crashes; another worker re-claims after lease expiry.
2. **Send** — `signal/tcp_client.go` issues a JSON-RPC `send` to signal-cli. If `tcpIgnoreResults=true` (fire-and-forget mode), success is assumed immediately. Otherwise wait on the per-request response channel up to a deadline.
3. **Correlate** — On `SUCCESS` result, `ResultConsumer` updates the row to `SENT` with `result_id` (signal-cli timestamp), deletes any local tmp files.
4. **Retry / fail** — On error, increment `attempts`; compute `next_attempt_at = now() + backoff(attempts)`. When `attempts >= max_attempts`, set `PERMANENT_FAILED` and notify Slack (if enabled).

## Inbound listening (optional)

When `signalCli.enableListening=true`, the signal-cli TCP client publishes received-message events (frames with no `id`) to a subscriber channel. `InboundListener` consumes the channel and writes rows to `signalium.inbound_signal_messages` with `INSERT ... ON CONFLICT DO NOTHING` on `(source, source_timestamp)` so reconnect-driven re-deliveries are idempotent. Attachment bytes stay on the signal-cli volume; only metadata (`{id, contentType, size}`) is recorded. See [`docs/inbound-listening.md`](./inbound-listening.md).

## Failure model

The system has three classes of failure and each gets its own recovery primitive:

- **Worker crash mid-send** — the row stays `SENDING` with a `next_attempt_at` in the future (the lease); when the lease expires another worker (or the restarted same worker) re-claims via the `(status='SENDING' AND next_attempt_at < now())` arm of the claim query.
- **Transient signal-cli error** — the worker calls `MarkFailed`, which writes `next_attempt_at = now() + backoff(attempts)` and flips status to `FAILED`. The worker does not auto-re-claim `FAILED` rows; an operator (or the resend endpoint) advances them.
- **Permanent failure** — when `attempts >= max_attempts`, `MarkFailed`'s CASE sets `PERMANENT_FAILED`. Slack is notified if enabled.

All three are deterministic and recoverable; the worker never enters a state that needs manual database surgery.
