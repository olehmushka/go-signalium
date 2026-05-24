package db

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/palantir/witchcraft-go-logging/wlog/svclog/svc1log"
	"go.uber.org/fx"

	"github.com/olehmushka/go-signalium/internal/repo/sqlc"
)

// Module wires the database layer. Order matters: the pool is provided first,
// migrations are applied via fx.Invoke, and only then is the sqlc Queries
// surface available to downstream modules.
//
//	db.Module pulls in:
//	  - Config        (from env, defaults match install.yml)
//	  - *pgxpool.Pool (connect on startup, close on shutdown)
//	  - migrations    (advisory-locked auto-run, gated by Config.Enable...)
//	  - *sqlc.Queries (constructed from the pool)
var Module = fx.Module(
	"db",
	fx.Provide(
		loadConfig,
		newPool,
		newQueries,
	),
	fx.Invoke(func(lc fx.Lifecycle, pool *pgxpool.Pool, logger svc1log.Logger, cfg Config) {
		lc.Append(fx.Hook{
			OnStart: func(ctx context.Context) error {
				return runMigrations(ctx, pool, logger, cfg)
			},
		})
	}),
)

// newQueries constructs the sqlc Queries surface backed by the connection pool.
// The pool itself satisfies sqlc.DBTX (Exec / Query / QueryRow).
func newQueries(pool *pgxpool.Pool) *sqlc.Queries {
	return sqlc.New(pool)
}
