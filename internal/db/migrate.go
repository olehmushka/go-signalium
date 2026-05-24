package db

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"hash"
	stdfs "io/fs"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	werror "github.com/palantir/witchcraft-go-error"
	"github.com/palantir/witchcraft-go-logging/wlog/svclog/svc1log"

	"github.com/olehmushka/go-signalium/migrations"
)

// runMigrations is invoked by fx after the pool exists. When auto-run is
// enabled it acquires a Postgres advisory lock, applies every pending
// migration from migrations.FS in lexicographic order, and records each
// application in signalium.schema_migrations.
//
// The advisory lock prevents two replicas racing the same migration during a
// rolling deploy (see docs/persistence.md and decisions/0007-atlas).
//
// Migrations are checksummed against atlas.sum so a tampered .sql file
// fails fast — atlas CLI (`make migrate-hash`) is the source of truth for
// the sum, this runner only verifies.
func runMigrations(ctx context.Context, pool *pgxpool.Pool, logger svc1log.Logger, cfg Config) error {
	if !cfg.EnableMigrationsAutoRun {
		logger.Info("skipping auto-migration", svc1log.SafeParam("reason", "disabled"))
		return nil
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return werror.Wrap(err, "acquire conn for migration")
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", cfg.MigrationsAdvisoryLockKey); err != nil {
		return werror.Wrap(err, "acquire migration advisory lock",
			werror.SafeParam("lockKey", cfg.MigrationsAdvisoryLockKey))
	}
	defer func() {
		if _, unlockErr := conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", cfg.MigrationsAdvisoryLockKey); unlockErr != nil {
			logger.Error(
				"release migration advisory lock failed",
				svc1log.SafeParam("lockKey", cfg.MigrationsAdvisoryLockKey),
				svc1log.Stacktrace(unlockErr),
			)
		}
	}()

	if err := ensureRevisionTable(ctx, conn.Conn()); err != nil {
		return err
	}

	sums, err := readAtlasSum()
	if err != nil {
		return err
	}

	pending, err := pendingMigrations(ctx, conn.Conn(), sums)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		logger.Info("no pending migrations")
		return nil
	}
	logger.Info("applying migrations", svc1log.SafeParam("count", len(pending)))
	for _, m := range pending {
		if err := applyOne(ctx, conn.Conn(), m); err != nil {
			return werror.Wrap(err, "apply migration",
				werror.SafeParam("version", m.version),
				werror.SafeParam("filename", m.filename))
		}
		logger.Info(
			"migration applied",
			svc1log.SafeParam("version", m.version),
			svc1log.SafeParam("filename", m.filename),
		)
	}
	return nil
}

// migrationFile captures a single .sql file pulled out of migrations.FS along
// with its expected atlas.sum hash and parsed body.
type migrationFile struct {
	filename string // e.g. "20260519000000_init.sql"
	version  string // e.g. "20260519000000"
	body     []byte
	expected string // base64 sha256 from atlas.sum
}

func ensureRevisionTable(ctx context.Context, conn *pgx.Conn) error {
	const ddl = `
CREATE SCHEMA IF NOT EXISTS signalium;
CREATE TABLE IF NOT EXISTS signalium.schema_migrations (
    version      TEXT PRIMARY KEY,
    filename     TEXT NOT NULL,
    content_hash TEXT NOT NULL,
    applied_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);`
	if _, err := conn.Exec(ctx, ddl); err != nil {
		return werror.Wrap(err, "ensure schema_migrations table")
	}
	return nil
}

// readAtlasSum parses migrations/atlas.sum into a map[filename]base64Hash.
// The first line is a directory-level checksum (h1:<...>) which we skip;
// subsequent lines are "<filename> h1:<...>".
func readAtlasSum() (map[string]string, error) {
	raw, err := migrations.FS.ReadFile("atlas.sum")
	if err != nil {
		return nil, werror.Wrap(err, "read atlas.sum")
	}
	out := map[string]string{}
	for i, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if i == 0 {
			continue // directory checksum
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, werror.Error("malformed atlas.sum line",
				werror.SafeParam("lineNum", i+1))
		}
		name, hash := fields[0], strings.TrimPrefix(fields[1], "h1:")
		out[name] = hash
	}
	return out, nil
}

func pendingMigrations(ctx context.Context, conn *pgx.Conn, sums map[string]string) ([]migrationFile, error) {
	applied := map[string]struct{}{}
	rows, err := conn.Query(ctx, "SELECT version FROM signalium.schema_migrations")
	if err != nil {
		return nil, werror.Wrap(err, "list applied migrations")
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, werror.Wrap(err, "scan applied migration version")
		}
		applied[v] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, werror.Wrap(err, "iter applied migrations")
	}

	entries, err := stdfs.Glob(migrations.FS, "*.sql")
	if err != nil {
		return nil, werror.Wrap(err, "glob embedded migrations")
	}
	sort.Strings(entries)

	// Atlas computes per-file hashes as a running sha256 across all files in
	// sorted order: for each file, write name then body into a single hasher
	// and snapshot Sum() — every entry is a cumulative checksum, not a
	// standalone sha256(body). See ariga.io/atlas sql/migrate.NewHashFile.
	running := sha256.New()
	pending := make([]migrationFile, 0, len(entries))
	for _, name := range entries {
		version, _, ok := strings.Cut(name, "_")
		if !ok {
			return nil, werror.Error("migration filename missing version prefix",
				werror.SafeParam("filename", name))
		}
		body, err := migrations.FS.ReadFile(name)
		if err != nil {
			return nil, werror.Wrap(err, "read embedded migration",
				werror.SafeParam("filename", name))
		}
		want, ok := sums[name]
		if !ok {
			return nil, werror.Error("migration not present in atlas.sum (run `make migrate-hash`)",
				werror.SafeParam("filename", name))
		}
		got := hashFile(running, name, body)
		if got != want {
			return nil, werror.Error("migration content hash mismatch (atlas.sum stale)",
				werror.SafeParam("filename", name),
				werror.SafeParam("expected", want),
				werror.SafeParam("actual", got))
		}
		if _, done := applied[version]; done {
			continue
		}
		pending = append(pending, migrationFile{
			filename: name,
			version:  version,
			body:     body,
			expected: want,
		})
	}
	return pending, nil
}

func applyOne(ctx context.Context, conn *pgx.Conn, m migrationFile) error {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return werror.Wrap(err, "begin migration tx")
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	if _, err := tx.Exec(ctx, string(m.body)); err != nil {
		return werror.Wrap(err, "exec migration body")
	}
	if _, err := tx.Exec(
		ctx,
		`INSERT INTO signalium.schema_migrations(version, filename, content_hash, applied_at)
		 VALUES ($1, $2, $3, $4)`,
		m.version, m.filename, m.expected, time.Now().UTC(),
	); err != nil {
		return werror.Wrap(err, "record applied migration")
	}
	if err := tx.Commit(ctx); err != nil {
		return werror.Wrap(err, "commit migration tx")
	}
	return nil
}

// hashFile advances the running atlas-style hash for one file and returns
// the snapshot recorded in atlas.sum for it. The caller must invoke this in
// the same sorted order atlas uses (lexicographic filename), since each
// entry is a cumulative sha256 over all prior name||body bytes plus this
// file's. Format in atlas.sum is "<name> h1:<base64(snapshot)>".
func hashFile(h hash.Hash, name string, body []byte) string {
	h.Write([]byte(name))
	h.Write(body)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}
