# signal-cli integration

`signal-cli` is the Java daemon (running in the container built by `deploy/Dockerfile.signal-cli`) that holds the Signal account credentials and speaks the Signal protocol. go-signalium connects to it via two transports:

- **TCP 7610 — JSON-RPC 2.0** for outbound sends and the receive-event stream.
- **HTTP 7611 — REST** for read-only queries (`listGroups`, healthcheck).

## Linking the daemon to a Signal account

First boot only. `deploy/entrypoint_signal_cli.sh` prints a QR code; scan it from the linked phone. On subsequent boots, `signal-cli` reuses the credentials from `signal_cli_data` volume.

```bash
docker compose -f deploy/docker-compose.yml up signal-cli
# scan QR
```

`SIGNAL_CLI_TRUST_NEW_IDENTITIES=always` is set in the entrypoint so the daemon does not block on first-contact key changes. Tighten this for production.

## TCP client (internal/signal/tcp_client.go)

**Topology:** one persistent `net.Conn` per process. signal-cli serves all calls on the same socket and emits asynchronous events on the same socket — multiple connections would gain nothing and complicate event demux.

```
┌───────────────────────────────────────────────────────────────┐
│ Client                                                         │
│  ┌──────────────┐                       ┌───────────────────┐  │
│  │ Send(ctx, r) │──── write mutex ────▶ │ net.Conn (writer) │  │
│  │   id := uuid │                       └───────────────────┘  │
│  │   pending[id]= ch                                            │
│  │   wait on ch ◀── reader goroutine demuxes by id ─┐           │
│  └──────────────┘                                    │           │
│                                                      │           │
│   ┌────────────────────────────────────────────────────────────┐ │
│   │ readLoop: bufio.Scanner over conn                          │ │
│   │   for each line:                                           │ │
│   │     decode JSON-RPC frame                                  │ │
│   │     if frame.ID != nil { pending[id] <- frame; delete }    │ │
│   │     else { events <- frame.toEvent() }                     │ │
│   │   on EOF: schedule reconnect with backoff                  │ │
│   └────────────────────────────────────────────────────────────┘ │
└───────────────────────────────────────────────────────────────┘
```

**Demux rule:** signal-cli responses to our requests carry the `id` we set; asynchronous events (incoming messages, receipts) have `id: null` or omit `id`. Branch on `frame.Result != nil || frame.Error != nil`.

**Writes** are serialized with a `sync.Mutex` around `conn.Write`. JSON-RPC frames are small; the mutex is uncontended in practice.

**Reconnect** runs when the read loop sees EOF or an unrecoverable error:
- Close the conn.
- Fail every in-flight `pending[id]` channel with `ErrDisconnected`.
- Backoff (exponential, capped at 30s), then dial again.
- Resume the read loop.

**`tcpIgnoreResults` mode** (runtime config): `Send` returns immediately after writing the frame without registering a pending entry. Use this when the upstream caller does not need per-request acknowledgement — useful for very high write throughput where the cost of tracking a response channel per send is not justified. Default off.

**Fake daemon for tests.** `internal/signal/testfake/` ships a TCP server that speaks the same JSON-RPC dialect. Hermetic tests and the worker e2e suite use it for deterministic runs — no JVM in the test path.

## HTTP client (internal/signal/http_client.go)

Used for endpoints where async-response semantics are wrong (proxied to a synchronous REST handler). Built on `conjure-go-runtime/v2/conjure-go-client/httpclient.Client` — gives retry, circuit breaking, observability metrics for free.

- `GET /api/v1/check` — used by `/status/readiness`.
- `POST /api/v1/rpc` with body `{"method":"listGroups","params":{"account":"+380..."}}` — used by `GET /api/v1/groups`.

## Outbound payload (signal-cli `send`)

JSON-RPC method `send`. Params produced by `internal/signal/message_builder.go`:

```json
{
  "account":     "+380...",      // sender phone, from config
  "message":     "content",
  "attachments": ["/abs/path/photo.jpg", ...],
  "recipient":   ["+380..."],    // OR groupId, not both
  "groupId":     ["abc..."],     // OR recipient, not both
  "quoteTimestamp": 1700000000,  // if quoteResultId set
  "quoteAuthor":    "+380..."    // if quoteResultId set
}
```

`attachments` are LOCAL FILE PATHS — signal-cli reads from disk. So the outbox worker:
1. Reads the row's `attachments` JSONB list.
2. For each `{bucket, filename}`, downloads from MinIO to `tmp/<message-id>/<filename>`.
3. Passes the local paths to the message builder.
4. On `SENT`, deletes the tmp files (or lets the cron cleanup handle them).

## Inbound events

When `signalCli.enableListening=true`, the reader goroutine routes events with no `id` to `Client.Events() <-chan Event`. The `InboundListener` service subscribes and writes rows to `signalium.inbound_signal_messages`. See [`inbound-listening.md`](./inbound-listening.md) for the schema and dedup story.

Event shape (parsed):

```json
{
  "method": "receive",
  "params": {
    "envelope": {
      "source":     "+380...",
      "sourceUuid": "...",
      "timestamp":  1700000000,
      "dataMessage": {
        "message": "text",
        "attachments": [{"id":"...", "contentType":"...", "size":12345}],
        "groupInfo": { "groupId": "..." }
      }
    }
  }
}
```

The listener does NOT echo into the outbound flow. Acknowledging or replying to received messages is out of scope.

## References

- signal-cli JSON-RPC reference: <https://github.com/AsamK/signal-cli/wiki/JSON-RPC-service> (links rot occasionally; verify against the daemon's own help output).
- signal-cli `send` command reference: <https://github.com/AsamK/signal-cli/blob/master/man/signal-cli.1.adoc>.
