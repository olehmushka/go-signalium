# Changelog

All notable changes to go-signalium are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project aims to follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html) once a `1.0.0` release exists. Until then, all releases are `0.x.y` and any release can carry breaking changes — see the "Breaking" section of each entry.

## [Unreleased]

### Added
- **Application metrics.** A `signalium.outbox.*` metric family (send/claim timers, terminal-status and retry counters, backlog depth/age gauges, dropped-inbound-event counter) registered on the same `metrics.DefaultMetricsRegistry` witchcraft drains to `metric.1` logs — no scrape endpoint. New `internal/metrics` fx seam; consumer-owned `Metrics` interfaces in the worker and signal-cli client. See `docs/observability.md` and ADR 0009.
- **Timeout enforcement.** Overdue messages now become `TIMED_OUT` automatically: `ClaimPending` excludes rows past their `timeout_at`, and a new fx-managed cron (`internal/worker/timeout_reaper.go`, `cron.timeoutReaper.enabled`) terminalises them on a one-minute tick and samples the backlog gauges. Closes the gap where a stale message was retried to `PERMANENT_FAILED` instead of `TIMED_OUT`. New sqlc queries `MarkTimedOut` + `BacklogStats`; integration tests cover the sweep, the claim exclusion, and backlog counting. See ADR 0010.
- **Supply-chain CI.** CodeQL (push/PR + weekly cron, `security-and-quality` query pack), govulncheck (push/PR + daily cron, fails on known CVEs), `actions/dependency-review-action` on PRs (fails on high-severity findings or copyleft licenses), and a Dependabot config that groups minor + patch updates for `gomod` and `github-actions`.
- **Coverage.** CI now runs `go test -coverprofile=cover.out -covermode=atomic` and uploads to Codecov (tokenless, public-repo); README carries a coverage badge alongside Go Report Card and release badges. `make cover` / `make cover-html` for local use.
- **Release pipeline.** `goreleaser` builds Linux + macOS, amd64 + arm64 archives on `v*` tags; SBOMs via `syft` per-archive; checksums signed keylessly with `cosign` (Sigstore Fulcio + GitHub OIDC). `make release-snapshot` for a local dry run.
- **Benchmarks.** Hot-path benchmarks (`make bench`) for the multipart admission path (metadata-only + 1KB/64KB/1MB attachments) and the sentinel→Conjure error mapper. Integration-tagged benchmark (`make bench-integration`) for the `ClaimPending` outbox claim roundtrip.
- **Fuzz testing.** `FuzzMultipart` covers the multipart admission boundary with ten seeds (valid, malformed JSON, mis-ordered parts, duplicate metadata, non-UTF-8 filenames, truncated bodies). A dedicated `.github/workflows/fuzz.yml` runs the target for 30s on PRs that touch `internal/handler/**`, with corpus caching.

### Fixed
- **Duplicate-send window narrowed.** The outbox now retries the `MarkSent` transition (bounded) so a transient DB blip after a successful signal-cli send no longer leaks a duplicate on lease re-claim; only a process crash in the send-to-commit window can. Delivery semantics (at-least-once) and the principled idempotent-reclaim fix are documented in ADR 0011.
- **Docs/code drift on `TIMED_OUT`.** `docs/worker.md` described a timeout sweep that was never implemented; the behaviour now exists and the doc matches it.
- **Dead reconnect state removed.** Dropped the always-true `first` flag in the signal-cli `dialLoop` and clarified the `dialOnce` re-arm invariant.
- **Multipart admission no longer leaks 500s on malformed input.** Three sites in `internal/handler/multipart.go` (duplicate metadata part, unexpected part name, underlying parser error during attachment iteration) previously returned plain `werror` values which `mapToConjureError` then wrapped as `InternalServiceError` → HTTP 500. They now return `InvalidSignalMessage` → HTTP 400. Discovered by `FuzzMultipart`; covered by a saved seed at `internal/handler/testdata/fuzz/FuzzMultipart/030c2aef2658c784`.

## [0.1.0] — 2026-05-24

First tagged release. The service is feature-complete for its initial scope (durable outbound Signal messages with attachments, optional inbound capture, Slack alerting on permanent failure) and the supporting tooling, tests, and docs have stabilised enough to draw a line.

### Added
- **Outbound send pipeline.** REST `POST /api/v1/signal-messages` accepts multipart (`metadata` JSON + `attachments`), persists a `PENDING` outbox row, returns `202`, and lets a background worker drive delivery via `signal-cli` JSON-RPC. Retries use exponential backoff + jitter; permanent failures are terminal.
- **Outbox worker.** Claim query uses `FOR UPDATE SKIP LOCKED` with a per-row lease so multiple workers can run safely.
- **Idempotency.** `idempotencyKey` on submission short-circuits to the existing message id on retry.
- **Inbound capture.** When `signalCli.enableListening=true`, received Signal events are deduplicated and persisted to `signalium.inbound_signal_messages`.
- **Operational endpoints.** List / Get / Update / Resend, stats, groups proxy.
- **Slack alerting.** Permanent failures fan out to a configured Slack webhook.
- **Persistence.** Postgres schema under the `signalium` schema, `pgx/v5` pool, sqlc-generated query layer, Atlas versioned migrations with advisory-locked auto-apply at boot.
- **Storage.** MinIO / S3 client with bucket bootstrap; attachments stream from request body to object storage without buffering full payloads in memory.
- **Framework wiring.** `uber-go/fx` owns the process lifecycle and wraps a `witchcraft-go-server` instance; Conjure IDL is the single source of truth for types, errors, and handler signatures.
- **Tests.** Hermetic unit suite (`make test`, `-race`) plus a testcontainers integration suite (`make integration-test`).
- **Docs.** `docs/architecture.md`, `docs/persistence.md`, `docs/rest-api.md`, `docs/signal-cli.md`, `docs/attachments.md`, `docs/worker.md`, `docs/inbound-listening.md`, `docs/config.md`, `docs/style.md`, and eight ADRs under `docs/decisions/`.
- **Repo hygiene.** `README.md`, `CONTRIBUTING.md`, `CODEOWNERS`, `SECURITY.md`, Apache-2.0 `LICENSE`, `.github/` issue + PR templates, GitHub Actions CI (`go vet`, `golangci-lint`, `go test -race`), README sequence diagram of the full request lifecycle.

### Changed
- Documentation moved into the repo and rewritten as self-contained — no cross-references to external services.

### Fixed
- `var/log/` runtime artefacts are no longer tracked; `.gitignore` updated to exclude them.

## [0.0.0-dev]

Pre-release milestone-by-milestone build-out (see `docs/architecture.md` for the milestone table).

### Highlights
- **M1 — Bootstrap.** `cmd/go-signalium`, fx skeleton, Makefile, `.golangci.yml`, Atlas/sqlc/Conjure toolchain wiring.
- **M2 — Persistence.** `signalium.signal_messages` schema, sqlc query layer, pgx pool, advisory-locked auto-migration.
- **M3 — Witchcraft + Conjure.** IDL → generated handlers; `/status/{liveness,readiness}` live.
- **M4 — Send pipeline.** Multipart upload handler, signal-cli TCP client (persistent connection, demux, reconnect), outbox worker (claim / lease / backoff).
- **M5 — Operational endpoints.** List/Get/Update/Resend, stats, groups proxy.
- **M6 — Inbound + Slack + cron.** Inbound listener + `signalium.inbound_signal_messages` table, Slack notifier, tmp cleanup cron.
- **M7 — Tests, lint, docs.** Hermetic unit tests for backoff/demux/multipart/error-mapping, testcontainers integration suite, complete docs, lint clean.

[Unreleased]: https://github.com/olehmushka/go-signalium/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/olehmushka/go-signalium/releases/tag/v0.1.0
[0.0.0-dev]: https://github.com/olehmushka/go-signalium/releases/tag/v0.0.0-dev
