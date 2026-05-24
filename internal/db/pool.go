package db

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	werror "github.com/palantir/witchcraft-go-error"
	"github.com/palantir/witchcraft-go-logging/wlog/svclog/svc1log"
	"go.uber.org/fx"
)

// newPool returns an fx-managed *pgxpool.Pool. The connect attempt is bounded
// by Config.ConnectTimeout; the pool is closed on fx OnStop.
func newPool(lc fx.Lifecycle, logger svc1log.Logger, cfg Config) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, werror.Wrap(err, "parse database url")
	}
	poolCfg.MaxConns = cfg.MaxConns
	poolCfg.MinConns = cfg.MinConns
	poolCfg.MaxConnLifetime = cfg.MaxConnLifetime
	poolCfg.ConnConfig.ConnectTimeout = cfg.ConnectTimeout

	ctx, cancel := context.WithTimeout(context.Background(), cfg.ConnectTimeout+5*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, werror.Wrap(err, "create pgx pool")
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, werror.Wrap(err, "ping postgres")
	}
	logger.Info(
		"postgres pool ready",
		svc1log.SafeParam("maxConns", int(cfg.MaxConns)),
		svc1log.SafeParam("minConns", int(cfg.MinConns)),
	)
	lc.Append(fx.Hook{
		OnStop: func(_ context.Context) error {
			pool.Close()
			return nil
		},
	})
	return pool, nil
}
