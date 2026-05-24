# 0003 — Multipart upload for attachments, staged to local MinIO

## Status
Accepted.

## Context
Attachments must outlive the HTTP request that submits them, because acceptance ([0001](./0001-rest-as-inbound-trigger.md)) and delivery are decoupled — the outbox worker may run minutes or hours after the request returned `202`. So bytes have to land in durable storage somewhere.

The realistic ergonomics models:

- **Inline multipart.** Caller `POST`s `multipart/form-data` with the message JSON and attachment file parts in a single request.
- **Two-step with presigned PUT.** Caller asks for a presigned URL, PUTs the bytes itself, then sends a JSON message that references `{bucket, key}`.
- **Base64 in a JSON body.** Caller encodes bytes into the JSON request body.

## Decision
go-signalium accepts attachments inline in the same multipart request that submits the send. Each `attachments` part is streamed straight to MinIO via `req.MultipartReader()` and `minio.PutObject(..., -1)`. The row stores `{bucket, filename}` refs in an `attachments` JSONB column.

## Consequences
- **One round trip from the caller's perspective.** Protocol is `POST multipart` and you're done.
- **True streaming** — no buffering of attachment bytes in memory. Suitable for the rare large attachment.
- **The service owns the bucket** (`signalium-attachments`); callers do not need MinIO/S3 access at all.
- **Conjure doesn't model multipart**, so this one endpoint bypasses the IDL — see [0008](./0008-conjure-bypass-for-multipart.md).
- **Boundary-condition handling** (partial upload failure, content-type validation, size limits) lives in `internal/handler/multipart.go`. Documented in [`attachments.md`](../attachments.md).

## Alternatives considered
- **Two-step with presigned PUT.** Less work for the service, more for every caller. Every caller must hold MinIO/S3 credentials or fetch a presigned URL per send. The simplification of go-signalium owning the storage wins.
- **Base64 in the JSON body.** Bloats request size by ~33% and forces the whole body to be parsed before any work begins. Rejected.
- **Sidecar upload microservice.** Overengineering for a feature already trivial in `minio-go`.

## Boundary
This decision is about *upload*. *Download* (the outbox worker fetching from MinIO before calling `signal-cli send`) is unchanged from the obvious model — see [`worker.md`](../worker.md#dispatch).
