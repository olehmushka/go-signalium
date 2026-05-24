# 0007 — Atlas (ariga.io) for database migrations

## Status
Accepted.

## Context
The service owns its Postgres schema and must apply DDL changes safely across rolling deploys. The realistic migration runners for Go:

1. **golang-migrate/migrate.** Community standard. Plain `.sql` files. CLI + Go library. No native linting or diffing.
2. **pressly/goose.** Plain SQL plus optional Go-functions-as-migrations.
3. **ariga/atlas.** Declarative HCL **or** versioned SQL. Built-in schema diffing, lint rules, drift detection, awareness of Postgres-specific objects (CHECK constraints, partial indexes, ENUMs).

## Decision
Use Atlas in **versioned SQL mode** (not declarative HCL). Migrations live in `migrations/YYYYMMDDHHMMSS_<slug>.sql` with an `atlas.sum` checksum file committed alongside.

Auto-run at boot is gated by `database.enableMigrationsAutoRun` (install.yml). When enabled, an fx.Invoke between the pgx pool provider and the repo provider:

1. Acquires `pg_advisory_lock(database.migrationsAdvisoryLockKey)`.
2. Applies pending migrations from an `embed.FS` view of `migrations/`.
3. Releases the lock.

## Consequences
- **Plain SQL** keeps the mental model unsurprising. Reviewers read DDL, not HCL.
- **`atlas migrate lint`** in CI catches destructive changes (e.g., dropping a NOT NULL without DEFAULT, narrowing an integer type) before merge.
- **Embedded migrations** mean the binary is self-contained for production; no separate `migrations/` directory to ship into the container image.
- **Advisory-lock-guarded auto-run** prevents two replicas from racing during a rolling deploy. Without the lock, the first replica's `atlas migrate apply` and the second replica's would interleave and possibly leave the schema in a half-applied state.
- **Schema declarative-vs-versioned is reversible.** Atlas supports both; if the team later prefers declarative HCL, the migration is a one-time `atlas migrate diff` from versioned SQL into HCL.

## Alternatives considered
- **golang-migrate.** Lacks lint / diff / drift detection. Atlas wins on operational tooling.
- **goose with Go-function migrations.** Useful for migrations that need code (e.g., backfilling derived data). All current migrations are pure DDL; no need.
- **No auto-run; require manual `make migrate-up` before deploy.** Safer on paper but adds a manual step that gets skipped under deploy pressure. Auto-run + advisory lock + CI lint is the better operational stance.

## Revisit if
- A migration genuinely needs to backfill data programmatically — at that point, evaluate whether a one-off job (separate from boot-time migrations) or switching to goose's Go-functions is the better fit.
- The `atlas` binary becomes an installation friction for contributors. Falling back to `golang-migrate` is a mechanical conversion.
