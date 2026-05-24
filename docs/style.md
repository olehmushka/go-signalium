# Style — Palantir Go conventions for go-signalium

This is not a generic Go style guide. It captures the **specific Palantir Go opinions** that apply to this project, with the rationale and the lint rule that enforces each one.

Baseline: [palantir/go-style-guide](https://github.com/palantir/go-style-guide). The rules below extend or specialize it.

## Errors

- **Use `werror`, not stdlib `errors` or `fmt.Errorf`, in non-test production code.** `werror.Wrap(err, "context")` carries a stack and structured params; bare wrapping loses both. `errors.Is` / `errors.As` still work because werror chains the cause. — Enforced by `depguard` (deny `errors`, deny `github.com/pkg/errors`) + `errorlint` (forbid `%v` for errors).
- **`werror.SafeParam` for values that may appear in logs; `werror.UnsafeParam` for PII or secrets.** wlog renders safe params inline and redacts unsafe ones.
- **Map to `conjure-go-contract/errors` only at the handler boundary** via a single `internal/handler/errors.go::mapToConjureError(err)`. Services and repos never see Conjure error types. — Documented in `docs/rest-api.md`.
- **Define semantic errors as sentinels in `internal/domain/errors.go`** (`var ErrIdempotencyConflict = werror.Error("idempotency conflict")`). Compare with `errors.Is`. Never compare error strings.
- **Log at exactly one place**: the handler edge. Services and repos wrap-and-return; they do not log-and-return.

## Logging

- **Use `wlog` / `svc1log`, never stdlib `log` or external loggers.** — `depguard` denies `log`, `logrus`, `zap` direct imports (zap is the wlog backend, not the API).
- **Get the per-request logger from context**: `svc1log.FromContext(ctx).Info(...)`. Witchcraft populates it with `traceId`, `requestId`, and other request-scoped fields.
- **Log stack traces with `svc1log.Stacktrace(err)`** (renders the werror stack). Do not `fmt.Sprintf("%+v", err)`.

## Context

- **`ctx context.Context` is always the first parameter** of any function that performs I/O or that calls a function that does. — `contextcheck`, `noctx`.
- **Never store ctx in a struct.** Pass it through.
- **Pass the request ctx to every downstream call** (DB query, MinIO PutObject, signal-cli RPC). Witchcraft cancels it on shutdown.
- Set per-attempt timeouts with `context.WithTimeout(ctx, ...)` at the layer that knows the right deadline (the outbox worker, not the handler).

## Packages & naming

- **No underscores in package names; no Hungarian prefixes.** Package `signal`, not `signal_pkg`.
- **One concept per package.** `internal/storage` is MinIO + local FS because both serve the same concept. `internal/signal` is TCP + HTTP because both speak signal-cli.
- **Initialisms are uppercase**: `ID`, `URL`, `JSON`, `HTTP`, `RPC`, `TCP`, `API`, `SQL`, `UUID`, `URI` — configured in `revive`'s `var-naming` rule.
- **Exported identifiers have package-level doc comments.** — `revive: package-comments, exported`.

## Interfaces

- **Define interfaces in the consumer, not the producer** (Palantir's preferred shape). `internal/service/send_message.go` defines what it needs from a `MessageRepo`; `internal/repo` provides a concrete type that happens to satisfy it.
- **Keep interfaces small.** If an interface has more than ~5 methods, it is doing more than one job.
- **No empty marker interfaces** unless documented why (e.g., satisfying a 3rd-party contract).

## Concurrency

- **Goroutines own a teardown signal.** Every long-running goroutine accepts a `ctx context.Context` and exits on `ctx.Done()`. fx.Lifecycle hooks must wait for in-flight goroutines on `OnStop`.
- **Channel direction in signatures.** `func subscribe() <-chan Event` so callers cannot send.
- **No global mutable state.** No `var pool *pgxpool.Pool`. All dependencies flow through fx.

## Testing

- **Table-driven tests.** Use `tests := []struct{ name string; ... }{...}` and `t.Run(tc.name, ...)`.
- **`testify/require` for hard failures, `testify/assert` for accumulating soft assertions.** Mix freely.
- **`t.Parallel()` at the top of any test that has no shared state.** — `tparallel` enforces it.
- **`testcontainers-go` for anything that touches Postgres, MinIO, or signal-cli.** No SQLite, no in-memory minio mock, no faking the schema. The fake signal-cli daemon is the one test double — and it speaks the real TCP JSON-RPC protocol.
- **`testifylint` enabled** to catch `assert.Equal(t, actual, expected)` (wrong argument order) and similar.

## Code organization

- **Hexagonal layering**: `internal/handler` → `internal/service` → `internal/repo` + `internal/signal` + `internal/storage`. Inversion: `service` depends on interfaces it owns; outer layers provide concrete impls.
- **`internal/generated` and `internal/repo/sqlc` are excluded from lint** but committed and reviewed for behavioral changes. Never hand-edit.
- **No business logic in `cmd/` or `internal/app/`.** Those packages exist only to wire fx.

## Formatting

- **`gofumpt`** (stricter than `gofmt`). Set up your editor or run `gofumpt -w .` before committing.
- **No `goimports` group manipulation**; let it sort.

## Dependency policy

`depguard` enforces:

```
Allow:  go stdlib, github.com/palantir/*, github.com/olehmushka/go-signalium/*,
        github.com/jackc/pgx/v5, github.com/minio/minio-go/v7,
        github.com/slack-go/slack, github.com/robfig/cron/v3, go.uber.org/fx,
        github.com/google/uuid, github.com/stretchr/testify (test only),
        github.com/testcontainers/testcontainers-go (test only)
Deny:   errors            -> use werror
        github.com/pkg/errors -> use werror
        log               -> use wlog
        github.com/sirupsen/logrus -> use wlog
```

Adding a new dependency: open an ADR (`docs/decisions/`) explaining why no existing dependency suffices, and update `.golangci.yml`'s allow-list.

## `.golangci.yml` ruleset rationale

The exact `.golangci.yml` lives in `go-signalium/.golangci.yml`. Highlights:

| Linter | What it catches | Why we keep it |
|---|---|---|
| `errcheck` | Discarded errors | Baseline; required. |
| `errorlint` | `fmt.Errorf("%v", err)`, `err == sentinel` | Forces correct wrapping + `errors.Is`. |
| `depguard` | Banned imports | See above. |
| `exhaustive` | Missing `switch` cases on enums | The `SignalMessageStatus` enum is the prime offender. |
| `contextcheck` | Functions that ignore ctx and call ctx-needing ones | High-value in the signal-cli loop where ctx threading is non-obvious. |
| `noctx` | `http.NewRequest` without ctx | Easy regression. |
| `bodyclose` | HTTP response bodies not closed | Mandatory for any HTTP client code (the signal-cli HTTP client). |
| `rowserrcheck`, `sqlclosecheck` | Postgres rows leaks | Cheap insurance over sqlc. |
| `gofumpt` | Formatting | One canonical layout. |
| `revive` | Naming, exported docs, error strings | Replaces golint; configured with initialisms list. |
| `gocritic` | Diagnostic/perf/style | Catches subtle anti-patterns. |
| `tparallel`, `testifylint` | Test hygiene | |
| `nolintlint` | `//nolint` directives without reason | Forces an explanation for every suppression. |

Generated dirs (`internal/generated`, `internal/repo/sqlc`) are excluded from lint via `issues.exclude-dirs`.
