# Configuration

go-signalium uses witchcraft's two-file convention:

- **`var/conf/install.yml`** — boot-time. Values that require a restart to change: ports, DB URL, signal-cli host, sender phone.
- **`var/conf/runtime.yml`** — refreshable. Values witchcraft watches via file mtime and pushes through `palantir/pkg/refreshable` to subscribers: log levels, feature flags, the Slack toggle, the inbound ignore list, etc.

Both files are typed: `internal/config/install.go` defines `Install`, `internal/config/runtime.go` defines `Runtime`. Witchcraft validates and parses them via `WithInstallConfigType` / `WithRuntimeConfigType`.

## `var/conf/install.yml`

The `server:` block uses kebab-case keys because witchcraft owns it (Install embeds `witchcraft-go-server/v3/config.Install`). Service-specific subsections below use camelCase to match the typed struct in `internal/config`.

```yaml
product-name:    go-signalium
product-version: 0.0.0-dev

# Witchcraft server (kebab-case keys; embeds wconfig.Install.Server)
server:
  address:         ""
  port:            8083
  management-port: 8084              # readiness/liveness/metrics (witchcraft default)
  context-path:    "/"
  cert-file:       ""
  key-file:        ""
  client-ca-files: []

# Logging
logging:
  level:              "info"             # debug | info | warn | error
  json:               true               # structured JSON output

# Database
database:
  url:                "postgres://myuser:mypassword@localhost:5432/mydb?sslmode=disable"
  pool:
    maxConns:         10
    minConns:         2
    maxConnLifetime:  "1h"
    connectTimeout:   "5s"
  enableMigrationsAutoRun: true
  migrationsAdvisoryLockKey: 4242        # arbitrary i64 used with pg_advisory_lock

# MinIO
minio:
  endpoint:           "localhost:9000"
  accessKey:          "minioadmin"
  secretKey:          "minioadmin"
  region:             "us-east-1"
  bucket:             "signalium-attachments"
  useSSL:             false
  localTmpDir:        "./tmp"            # download staging for outbox

# signal-cli
signalCli:
  tcp:
    host:             "127.0.0.1"
    port:             7610
    waitResultTimeout: "10s"
    ignoreResults:    false              # if true, fire-and-forget; treats every send as success
  http:
    host:             "127.0.0.1"
    port:             7611
    timeout:          "10s"
    maxRetries:       5
  senderPhoneNumber:  "+380XXXXXXXXX"    # this process's bound account
  enableListening:    false              # if true, capture inbound events to DB

# Outbox worker
worker:
  pollInterval:       "1s"
  leaseDuration:      "5m"
  perAttemptTimeout:  "60s"
  concurrency:        1
  baseBackoff:        "5s"
  maxBackoff:         "1h"

# Cron jobs
cron:
  cleanupOldFiles:
    enabled:          true
    schedule:         "0 * * * * *"      # advisory; the sweeper runs on a fixed 1-minute tick
    directories:      ["./tmp"]
    fileTtl:          "10m"
  timeoutReaper:
    enabled:          true               # flip overdue (timeout_at <= now) non-terminal rows to TIMED_OUT
    schedule:         "0 * * * * *"      # advisory; the reaper runs on a fixed 1-minute tick
```

> The `schedule` fields are parsed but not yet honoured — both cron jobs run on a
> fixed one-minute tick. The default expression matches that cadence. See
> [`worker.md`](./worker.md) and [decisions/0010](./decisions/0010-timeout-reaper.md).

## `var/conf/runtime.yml`

```yaml
# All values here are watched and pushed to refreshable subscribers.

logging:
  level:              "info"             # live log-level changes
  
worker:
  pollInterval:       "1s"               # tweak claim cadence at runtime
  maxAttempts:        5                  # default; rows store their own override

signalCli:
  enableListening:    false

inbound:
  ignore:
    typing:           true
    receipt:          true
    syncMessage:      true
    callMessage:      true

slack:
  enabled:            false
  webhookUrl:         ""                 # treat as a secret; consider mounting from a Secret instead
  channelIds:         []                 # for slack-go SDK based notifier
  notifyOn:
    permanentFailure: true
    attachmentError:  true
```

## Refreshable boundary

**Refreshable** (re-read on each use):
- log level
- `worker.pollInterval`, `worker.maxAttempts`
- `signalCli.enableListening`
- `inbound.ignore.*`
- `slack.*`

**Not refreshable** (require restart):
- ports, TLS paths, `server.maxRequestBodySize`
- DB URL, pool settings
- MinIO endpoint/creds (changing creds in flight breaks the SDK's connection cache)
- signal-cli host/port (reconnect logic doesn't observe config changes)
- `senderPhoneNumber` (one-process-one-sender invariant)
- worker `concurrency` (semaphore resize is non-trivial; restart to scale)

[decisions/0005](./decisions/0005-fx-wrapping-witchcraft.md) covers refreshable interaction with the fx graph.

## Env var overrides

Witchcraft supports overriding config keys via env vars (`SERVER_PORT=9090` overrides `server.port`). Useful for k8s + Helm. The mapping is automatic: upper-snake-case, dot-to-underscore. Documented separately by witchcraft.

## Secrets

Secrets (DB password, MinIO creds, Slack webhook) belong in mounted files, not in the YAML. Witchcraft supports per-field `{file:/path/to/secret}` references via its config loader. For development, in-line plaintext is acceptable; for prod, use the file-reference form.
