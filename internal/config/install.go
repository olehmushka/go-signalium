// Package config defines the typed Install and Runtime configuration that
// witchcraft parses out of var/conf/install.yml and var/conf/runtime.yml.
//
// Install embeds witchcraft's base Install (server ports, TLS, product
// metadata) and adds service-specific blocks: database, MinIO, signal-cli,
// outbox worker, cron. Runtime embeds witchcraft's base Runtime (loggers,
// audit, service discovery) and adds blocks the refreshable surface needs
// (worker pollInterval/maxAttempts, signalCli.enableListening, inbound ignore
// rules, Slack).
//
// docs/config.md explains which keys live where and which can be hot-reloaded.
// The yaml tags on the witchcraft-owned section follow witchcraft conventions
// (kebab-case under `server:`); the rest of this file's tags are camelCase to
// match the existing var/conf/install.yml shape.
package config

import (
	"time"

	wconfig "github.com/palantir/witchcraft-go-server/v3/config"
)

// Install is the typed install configuration. It satisfies
// wconfig.BaseInstallConfig via the embedded wconfig.Install and adds the
// service-specific subsections (database, MinIO, signal-cli, worker, cron).
type Install struct {
	wconfig.Install `yaml:",inline"`

	Database  DatabaseConfig  `yaml:"database"`
	MinIO     MinIOConfig     `yaml:"minio"`
	SignalCli SignalCliConfig `yaml:"signalCli"`
	Worker    WorkerConfig    `yaml:"worker"`
	Cron      CronConfig      `yaml:"cron"`
}

// BaseInstallConfig satisfies the witchcraft interface and lets generic code
// retrieve the embedded base struct without type assertions.
func (i Install) BaseInstallConfig() wconfig.Install { return i.Install }

// DatabaseConfig is the `database:` subtree. Fields mirror docs/config.md.
type DatabaseConfig struct {
	URL                       string       `yaml:"url"`
	Pool                      DatabasePool `yaml:"pool"`
	EnableMigrationsAutoRun   bool         `yaml:"enableMigrationsAutoRun"`
	MigrationsAdvisoryLockKey int64        `yaml:"migrationsAdvisoryLockKey"`
}

// DatabasePool maps the `database.pool:` subtree.
type DatabasePool struct {
	MaxConns        int32         `yaml:"maxConns"`
	MinConns        int32         `yaml:"minConns"`
	MaxConnLifetime time.Duration `yaml:"maxConnLifetime"`
	ConnectTimeout  time.Duration `yaml:"connectTimeout"`
}

// MinIOConfig maps the `minio:` subtree.
type MinIOConfig struct {
	Endpoint    string `yaml:"endpoint"`
	AccessKey   string `yaml:"accessKey"`
	SecretKey   string `yaml:"secretKey"`
	Region      string `yaml:"region"`
	Bucket      string `yaml:"bucket"`
	UseSSL      bool   `yaml:"useSSL"`
	LocalTmpDir string `yaml:"localTmpDir"`
}

// SignalCliConfig maps the `signalCli:` subtree.
type SignalCliConfig struct {
	TCP               SignalCliTCP  `yaml:"tcp"`
	HTTP              SignalCliHTTP `yaml:"http"`
	SenderPhoneNumber string        `yaml:"senderPhoneNumber"`
	EnableListening   bool          `yaml:"enableListening"`
}

// SignalCliTCP maps the `signalCli.tcp:` subtree.
type SignalCliTCP struct {
	Host              string        `yaml:"host"`
	Port              int           `yaml:"port"`
	WaitResultTimeout time.Duration `yaml:"waitResultTimeout"`
	IgnoreResults     bool          `yaml:"ignoreResults"`
}

// SignalCliHTTP maps the `signalCli.http:` subtree.
type SignalCliHTTP struct {
	Host       string        `yaml:"host"`
	Port       int           `yaml:"port"`
	Timeout    time.Duration `yaml:"timeout"`
	MaxRetries int           `yaml:"maxRetries"`
}

// WorkerConfig maps the install-time `worker:` subtree. PollInterval and
// MaxAttempts live additionally in the runtime config (refreshable); the
// install copy is the boot-time seed.
type WorkerConfig struct {
	PollInterval      time.Duration `yaml:"pollInterval"`
	LeaseDuration     time.Duration `yaml:"leaseDuration"`
	PerAttemptTimeout time.Duration `yaml:"perAttemptTimeout"`
	Concurrency       int           `yaml:"concurrency"`
	BaseBackoff       time.Duration `yaml:"baseBackoff"`
	MaxBackoff        time.Duration `yaml:"maxBackoff"`
}

// CronConfig maps the `cron:` subtree.
type CronConfig struct {
	CleanupOldFiles CleanupOldFilesConfig `yaml:"cleanupOldFiles"`
}

// CleanupOldFilesConfig configures the tmp-cleanup cron job (M6).
type CleanupOldFilesConfig struct {
	Enabled     bool          `yaml:"enabled"`
	Schedule    string        `yaml:"schedule"`
	Directories []string      `yaml:"directories"`
	FileTTL     time.Duration `yaml:"fileTtl"`
}
