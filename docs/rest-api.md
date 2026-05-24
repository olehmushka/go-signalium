# REST API

## Endpoints

| Method | Path | Body | Returns | Notes |
|---|---|---|---|---|
| GET | `/status/liveness` | — | `{status:"ok"}` | Witchcraft built-in. `/api/healthz` aliases to this. |
| GET | `/status/readiness` | — | `{status:"ready"}` or 503 | Probes pgx pool ping, MinIO bucket head, signal-cli TCP daemon. `/api/readyz` aliases. |
| GET | `/api/v1/info` | — | `InstanceInfo` | `{name, senderPhoneNumber}` — both from install.yml. |
| GET | `/api/v1/groups` | — | `[]SignalGroupInfo` | Proxied to `signal-cli` HTTP daemon (`/api/v1/rpc` with `listGroups`). |
| **POST** | **`/api/v1/signal-messages`** | **multipart/form-data** | `202 SignalMessageAccepted` | See "Send" below. |
| GET | `/api/v1/signal-messages` | — | `SignalMessageList` | Query params: `q`, `limit`, `offset`, `createdAtFrom`, `createdAtTo`, `status`, `attempts`. |
| GET | `/api/v1/signal-messages/{id}` | — | `SignalMessageInfo` | Polling endpoint — callers loop until `status` is terminal (`SENT` / `PERMANENT_FAILED` / `TIMED_OUT`). |
| PUT | `/api/v1/signal-messages/{id}` | `UpdateRequest` | `SignalMessageInfo` | Operational: update status field (e.g., manual `PERMANENT_FAILED` override). PUT (not PATCH) because Conjure restricts methods to GET/POST/PUT/DELETE. |
| POST | `/api/v1/signal-messages/{id}/resend` | — | `SignalMessageInfo` | Resets `status=PENDING`, `attempts=0`, `next_attempt_at=now()` if currently FAILED/TIMED_OUT. POST (not PATCH) for the same Conjure-method reason. |
| GET | `/api/v1/signal-messages-stats` | — | `Stats` | Aggregated counts + per-day time series. |

## Send (POST /api/v1/signal-messages)

**Request — `multipart/form-data`**

```
--boundary
Content-Disposition: form-data; name="metadata"
Content-Type: application/json

{
  "externalId": "caller-msg-uuid",
  "idempotencyKey": "optional-dedup-key",
  "recipient": "+380...",          // OR groupId, not both
  "groupId": "abcdef...",          // OR recipient, not both
  "content": "message body",
  "quoteResultId": "1700000000",   // optional, reply-to
  "timeoutSeconds": 60,            // optional
  "maxAttempts": 5                 // optional, default 5
}
--boundary
Content-Disposition: form-data; name="attachments"; filename="photo.jpg"
Content-Type: image/jpeg

<binary>
--boundary--
```

**Response — `202 Accepted`**

```json
{
  "status": "success",
  "statusCode": 202,
  "meta": { "timestamp": "...", "requestId": "...", "version": "1" },
  "error": null,
  "data": { "id": "<uuid>" }
}
```

**Semantics:**

1. Multipart is parsed with `req.MultipartReader()` (streaming, no full-body buffer).
2. `metadata` JSON is decoded; validation rules: exactly one of `recipient` / `groupId` (echoes the DB CHECK constraint), non-empty `content`, `externalId` ≤ 255 chars.
3. Each `attachments` part is streamed straight to MinIO via `PutObject(ctx, bucket, key, partReader, -1, opts)` where `key = signal_message_id + "/" + filename` and `bucket` defaults to install.yml's `minio.bucket`.
4. `senderPhoneNumber` is omitted from the request; the server stamps the configured value. (If the request DOES include it and it does not match the configured value, the server returns `409 SenderMismatch`.)
5. `idempotencyKey`: if a row with the same key exists, the response returns its `signal_message_id` without inserting a new row or re-uploading attachments.
6. Otherwise a `PENDING` row is inserted; the outbox worker picks it up on the next tick.

## Response envelope

Defined in the Conjure IDL (`go-signalium/conjure/go-signalium-api.conjure.yml`). All JSON endpoints share:

```json
{
  "status":     "success | fail | error",
  "statusCode": 200,
  "meta": {
    "timestamp": "ISO-8601 UTC",
    "requestId": "string",
    "version":   "string"
  },
  "error":      null | { "code": "...", "name": "...", "message": "...", "parameters": {...} },
  "data":       <T> | null
}
```

`/status/{liveness,readiness}` are the exception — they return the bare witchcraft body for compatibility with k8s probes.

## Errors

Conjure errors declared in the IDL under `errors:`:

| Name | HTTP | When |
|---|---|---|
| `IdempotencyConflict` | 409 | `idempotencyKey` already used with a different request body. |
| `InvalidArgument` | 400 | Validation failure (bad sender, both recipient+groupId, empty content, etc.). |
| `NotFound` | 404 | GET/PATCH `/{id}` against a non-existent row. |
| `SenderMismatch` | 409 | Request `senderPhoneNumber` does not match server config. |
| `AttachmentUploadFailed` | 502 | MinIO write failed during streaming. |
| `SignalCliUnavailable` | 503 | TCP daemon unreachable when caller hits a synchronous endpoint (groups proxy). |
| `Internal` | 500 | Anything unmapped; server logs full werror stack but does not leak details. |

Mapping `error → conjure error` lives in `internal/handler/errors.go::mapToConjureError`. Handlers always return either a typed conjure error or `Internal()`.

## Conjure workflow

1. Edit `go-signalium/conjure/go-signalium-api.conjure.yml`.
2. `make conjure` → two-step pipeline: `conjure compile` (Java tool, YAML → IR JSON) then `conjure-go --server` (IR → Go in `internal/generated/`).
3. Implement the new handler in `go-signalium/internal/handler/`.
4. Wire it up in the witchcraft `InitFunc` via the generated `RegisterRoutes<Service>` call.

Conjure does not model HTTP PATCH; PUT or POST are used for the equivalent semantics on this service.

**Multipart exception.** `POST /api/v1/signal-messages` is **not** modelled in Conjure (Conjure does not support multipart). It is registered as a raw `wrouter.RouteHandler` in the same `InitFunc`. The `metadata` part still uses a Conjure-defined struct for typed decoding. Documented in [decisions/0008](./decisions/0008-conjure-bypass-for-multipart.md).
