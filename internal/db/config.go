// Package db provides the Postgres connection pool, the sqlc Queries
// constructor, and the advisory-locked migration runner. The fx.Module wires
// these so that the pool is created → migrations apply → Queries is provided,
// and on shutdown the pool is closed cleanly.
//
// Typed install.yml parsing arrives in M3; for M2 the config is sourced from
// environment variables with defaults matching var/conf/install.yml.
package db

import (
	"os"
	"strconv"
	"time"

	werror "github.com/palantir/witchcraft-go-error"
)

// Config captures the database section of install.yml. Field names mirror
// the yaml structure documented in docs/config.md.
type Config struct {
	URL                       string
	MaxConns                  int32
	MinConns                  int32
	MaxConnLifetime           time.Duration
	ConnectTimeout            time.Duration
	EnableMigrationsAutoRun   bool
	MigrationsAdvisoryLockKey int64
}

// loadConfig reads the Config from the SIGNALIUM_DB_* environment variables.
// Defaults match the values in var/conf/install.yml so a developer can run
// `make run` against the docker-compose stack without setting anything.
func loadConfig() (Config, error) {
	cfg := Config{
		URL:                       envOr("SIGNALIUM_DB_URL", "postgres://myuser:mypassword@localhost:5432/mydb?sslmode=disable"),
		MaxConns:                  int32(envInt("SIGNALIUM_DB_MAX_CONNS", 10)),
		MinConns:                  int32(envInt("SIGNALIUM_DB_MIN_CONNS", 2)),
		MaxConnLifetime:           envDuration("SIGNALIUM_DB_MAX_CONN_LIFETIME", time.Hour),
		ConnectTimeout:            envDuration("SIGNALIUM_DB_CONNECT_TIMEOUT", 5*time.Second),
		EnableMigrationsAutoRun:   envBool("SIGNALIUM_DB_AUTO_MIGRATE", true),
		MigrationsAdvisoryLockKey: int64(envInt("SIGNALIUM_DB_ADVISORY_LOCK_KEY", 4242)),
	}
	if cfg.URL == "" {
		return Config{}, werror.Error("database url is empty",
			werror.SafeParam("envVar", "SIGNALIUM_DB_URL"))
	}
	return cfg, nil
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
