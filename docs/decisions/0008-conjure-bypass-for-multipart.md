# 0008 — Bypass Conjure for the multipart upload endpoint

## Status
Accepted.

## Context
[0004](./0004-witchcraft-full-framework.md) commits the service to Conjure IDL for HTTP types and handlers. Conjure's body model is JSON-first; it supports an opaque `binary` body type for octet-stream payloads but does not model `multipart/form-data` with mixed JSON + file parts.

`POST /api/v1/signal-messages` ([0003](./0003-multipart-attachments.md)) is exactly that mixed shape: a `metadata` JSON part plus zero or more `attachments` file parts.

## Decision
Register `POST /api/v1/signal-messages` as a raw `wrouter.RouteHandler` inside the same witchcraft `InitFunc` that registers the Conjure-generated handlers. The `metadata` JSON part is decoded into a Conjure-defined struct (`CreateSignalMessageRequest`) so the schema for the JSON portion remains IDL-sourced; only the *transport* is hand-rolled for this one route.

```go
// Inside InitFunc:
if err := signalapi.RegisterRoutesSignalMessagesService(info.Router, handlers.SignalMessages); err != nil {
    return nil, err
}
if err := info.Router.Post("/api/v1/signal-messages", handlers.MultipartUpload); err != nil {
    return nil, err
}
```

The multipart handler:

1. Calls `req.MultipartReader()` (streaming).
2. Reads the `metadata` part first; decodes into the Conjure type.
3. For each subsequent `attachments` part, streams to MinIO via `PutObject(..., -1)`.
4. Persists the row.
5. Returns `202` with the new id.

## Consequences
- **One route is "outside" Conjure.** This endpoint does not appear in conjure-generated OpenAPI exports. Documented in [`rest-api.md`](../rest-api.md#send-post-apiv1signal-messages); a sibling section in any externally-published spec must mention it.
- **The metadata JSON shape is still IDL-sourced.** Schema discipline is preserved for 99% of the payload by surface area.
- **Hand-rolled error responses** for this route — the multipart handler builds the envelope manually (or uses a shared `respond.JSON(rw, ...)` helper). Cannot rely on Conjure's automatic error rendering.
- **Lint enforcement** that no second route bypasses Conjure: a `grep -r "info.Router.Post\|info.Router.Get" internal/` in CI must return exactly the multipart-upload line. Mechanical, cheap, and stops drift.

## Alternatives considered
- **Declare the endpoint as `binary` in Conjure and parse multipart manually inside the generated handler.** Possible but the handler signature treats the body as opaque `io.ReadCloser`; you end up doing the same multipart parsing inside a handler that pretends to be typed. Worst of both.
- **Two-step upload protocol** (presigned PUT → JSON POST). Rejected in [0003](./0003-multipart-attachments.md) on caller-ergonomics grounds.
- **Encode attachments as base64 inside the JSON body.** Rejected — bloats request size by ~33% and buffers in memory.

## Revisit if
Conjure adds native multipart support (the spec has been discussed in upstream issues but is not implemented). At that point, fold this endpoint back into the IDL.
