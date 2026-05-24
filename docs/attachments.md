# Attachments

go-signalium accepts attachments inline in the `POST /api/v1/signal-messages` multipart request, stages them to local MinIO, and downloads them again at send time onto the signal-cli daemon's local disk.

## Why MinIO at all?

Because the outbox + worker pattern decouples acceptance from delivery, the bytes must outlive the HTTP request. Three storage options were considered:

1. **MinIO (chosen).** Survives restarts, accessible by all replicas, S3-compatible (easy to swap to AWS S3 later).
2. **Local FS on the API node.** Tied to one host; doesn't work when worker and API run on different replicas.
3. **DB BLOBs.** Postgres handles binary fine but balloons the WAL and complicates replication for what is essentially media-tier data.

[decisions/0003](./decisions/0003-multipart-attachments.md) records the call.

## Upload — request lifecycle

```
POST /api/v1/signal-messages              MinIO
       │                                    │
       │ multipart/form-data                │
       ▼                                    │
   ┌──────────────────────────────┐         │
   │ multipart handler            │         │
   │   reader := req.MultipartReader()      │
   │   for each part:             │         │
   │     if part.FormName=="metadata":      │
   │       json.Decode → metadata │         │
   │     elif part.FormName=="attachments": │
   │       key := id + "/" + filename       │
   │       PutObject(part, -1) ────────────▶│  bucket: signalium-attachments
   │       refs = append(refs, {bucket, key})
   │   insert row(metadata, refs) │
   │   201 / 202                  │         │
   └──────────────────────────────┘         │
```

Important properties:

- **`req.MultipartReader()` not `req.ParseMultipartForm`.** Parsing buffers the entire body in memory or a temp file before handing it back. The streaming reader passes one part at a time — go-signalium never holds an attachment in memory.
- **`PutObject(..., -1, opts)`.** Negative size tells minio-go to use its multipart upload code path internally, which streams in 5 MB chunks. Fine for arbitrarily large attachments.
- **Part-name convention.** `metadata` is the JSON envelope (exactly one). `attachments` is repeated (zero or more). Any other part name is a 400.
- **Order matters.** The handler reads parts in stream order. Convention: send `metadata` first so the handler has the message id (a UUID it generates the moment metadata is parsed) before any file part arrives — that id becomes the MinIO key prefix.

## Bucket layout

Single bucket, configured in install.yml:

```yaml
minio:
  endpoint:    "localhost:9000"
  accessKey:   "minioadmin"
  secretKey:   "minioadmin"
  region:      "us-east-1"
  bucket:      "signalium-attachments"   # one bucket, all attachments
  useSSL:      false
```

Keys: `<signal_message_id>/<original-filename>` — predictable, message-scoped, no collisions across messages, easy to clean up by prefix when a message is hard-deleted.

Bucket is created idempotently at boot by the `storage` fx module (`minio.MakeBucket(ctx, bucket, opts)`; treats `BucketAlreadyOwnedByYou` as success).

## Download — outbox worker

Before calling `signal-cli send`, the worker downloads each attachment from MinIO to the local tmp dir:

1. `tmpDir := filepath.Join(install.minio.localTmpDir, messageID)` — e.g., `tmp/<uuid>/`.
2. For each `{bucket, filename}` in the row: `minio.GetObject(...)` → write to `tmpDir/<filename>`.
3. Pass absolute paths to the JSON-RPC `send` params.
4. On `SENT`, queue the tmp dir for deletion (or rely on cron cleanup if `cleanupOnSuccess=false`).

Download has its own retry loop (3 attempts, exponential backoff) — MinIO transient failures shouldn't kill a send attempt directly. On exhausted retries the message rolls into the normal FAILED → backoff path.

## TTL cleanup

A `robfig/cron/v3` job runs on a configurable schedule (`cron.cleanupOldFilesPeriod`, default hourly). It walks the configured directories (default `tmp/`) and deletes files whose mtime is older than `download.fileTtl` (default 10 minutes). This catches stragglers from crashed worker dispatches that never reached the `CleanupLocal` step.

The cron job is fx-lifecycle-managed: starts on `OnStart`, stops on `OnStop` waiting for the current sweep to finish.

## Validation

The handler validates each attachment part before streaming:

- `Content-Type` is read from the part header; rejected if not in a configurable allow-list (default: any).
- A soft size cap per attachment via `MaxBytesReader` on the part body; total request size capped by witchcraft's `maxRequestBodySize` in install.yml.
- Filename is sanitized — only `[A-Za-z0-9._-]` allowed; anything else replaced with `_`. Path traversal sequences (`../`) are rejected outright.

Failures during streaming roll back: any object already PUT to MinIO is deleted (best-effort `RemoveObject`); the row is never inserted; handler returns `AttachmentUploadFailed`.
