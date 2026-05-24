# Contributing to go-signalium

Thanks for considering a contribution. This doc captures the workflow conventions and the conventions the codebase upholds. Read it once before your first PR.

## Before you start

- **Read [`docs/architecture.md`](./docs/architecture.md) and [`docs/style.md`](./docs/style.md).** They are short and they explain the rules the linter enforces.
- **Open an issue first** for any non-trivial change — feature, refactor across packages, schema change, new dependency. A one-line "I'd like to add X" is enough to surface objections before you spend the time.
- **For architectural changes, write an ADR** in [`docs/decisions/`](./docs/decisions). See the [ADR README](./docs/decisions/README.md#when-to-write-a-new-adr) for the bar.

Trivial changes (typos, doc tweaks, obvious bug fixes with a regression test) can go straight to a PR.

## Branch naming

| Prefix | Use |
|---|---|
| `feature/<slug>` | New capability or endpoint |
| `bug/<slug>` | Fix for an unintended behaviour |
| `refactor/<slug>` | Internal cleanup; no observable change |
| `test/<slug>` | Adds or restructures tests only |
| `docs/<slug>` | Docs-only |
| `hotfix/<slug>` | Urgent production fix |
| `security/<slug>` | Security-relevant change (see [SECURITY.md](./SECURITY.md)) |

Lowercase kebab-case slugs. Digits allowed.

## Commit messages

- Subject line ≤ 72 chars, imperative mood ("add list endpoint", not "added" / "adds").
- Body explains *why*, not *what* (the diff shows what). Mention the issue or ADR if relevant.
- One logical change per commit. If a PR has more than one, the reviewer can read the commits separately.

## Development workflow

```bash
# Initial setup
make tidy                       # go mod tidy

# Tight loop
make fmt                        # gofumpt + goimports
make lint                       # golangci-lint (must be clean to merge)
make test                       # hermetic unit tests (-race)

# Before pushing
make build                      # confirm the binary compiles
make integration-test           # spins Postgres testcontainer (~90s)

# Schema / IDL / SQL changes
make migrate-diff NAME=add_foo  # author a new migration
make migrate-hash               # recompute atlas.sum
make sqlc-generate              # if you edited internal/repo/queries/*.sql
make conjure                    # if you edited conjure/*.conjure.yml
```

CI runs `make lint`, `make test`, and a `go build`. Integration tests run locally only; they are not part of CI today (see [`docs/architecture.md`](./docs/architecture.md) and the test file's package comment).

## The merge bar

A PR is ready when:

1. **Lint is clean.** `golangci-lint run` returns 0 issues. No `//nolint` without a `// reason` line (`nolintlint` enforces it).
2. **Tests pass with `-race`.** Add a regression test for any bug fix. New features need both unit tests and at least one integration assertion (or a documented reason why integration coverage is impractical).
3. **Public contract changes are reflected in the IDL.** Anything touching request/response shapes goes through `conjure/go-signalium-api.conjure.yml` first; the generated code follows.
4. **Schema changes go through Atlas.** Hand-editing `internal/repo/sqlc/` or migrating outside `make migrate-diff` is rejected.
5. **Cross-cutting decisions are ADR'd.** If a reviewer would ask "why was this done this way?", the answer should be a link to `docs/decisions/NNNN-...`.
6. **Docs that describe the changed behaviour are updated** in the same PR. Stale docs are worse than missing docs.

## Code conventions

The full list lives in [`docs/style.md`](./docs/style.md). The cheat sheet:

- **Errors:** `werror.Wrap(err, "context", werror.SafeParam("k", v))`. Never stdlib `errors` (except `errors.Is` / `errors.As`), never `fmt.Errorf`.
- **Logging:** `svc1log.FromContext(ctx).Info("...", svc1log.SafeParam(...))`. Never stdlib `log`. Never `fmt.Printf`-style debugging in committed code.
- **Context:** `ctx context.Context` is always the first parameter. Never stored in a struct. Per-attempt timeouts (`context.WithTimeout`) live at the layer that knows the right deadline.
- **Tests:** table-driven with `t.Run`. `testify/require` for hard fails, `testify/assert` for accumulation. `t.Parallel()` on anything stateless.
- **Comments:** explain *why* the code is non-obvious. Don't restate what well-named identifiers already say.
- **No new dependencies without an ADR** explaining what existing dependency was insufficient.

## Testing

- **Hermetic suite (`make test`)** runs in-memory and finishes in ~3 seconds. Every PR keeps it green.
- **Integration suite (`make integration-test`)** spins a real Postgres via testcontainers and exercises the sqlc query layer end-to-end. Run before pushing anything that touches `internal/repo/`, `migrations/`, or the schema overrides in `sqlc.yaml`.
- **Fake signal-cli daemon** in `internal/signal/testfake/` is the only test double for the JSON-RPC protocol. Do not mock the TCP client; use the fake.

## Reporting bugs

Open an issue with the [bug report template](./.github/ISSUE_TEMPLATE/bug_report.md). Include the version, the request/response (redacting PII), relevant log lines, and the smallest reproducer you can manage.

## Reporting security issues

**Do not file a public issue.** Follow the disclosure process in [SECURITY.md](./SECURITY.md).
