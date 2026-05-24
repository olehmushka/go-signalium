# 0009 — Application metrics on the witchcraft registry

## Status
Accepted.

## Context
The service ran with zero custom metrics: witchcraft emitted its built-in server
metrics, but nothing reported on the outbox itself — queue depth, send latency,
retry/terminal rates, dropped inbound events. For a durable-delivery service that
is exactly the data an operator needs to answer "is it keeping up?" and "why are
messages failing?". We needed a metrics surface without bolting on a second
telemetry stack.

The constraint that shapes the design: `witchcraft-go-server/v3` hardwires the
package-global `metrics.DefaultMetricsRegistry` (`github.com/palantir/pkg/metrics`)
as its root registry — there is no `WithMetricsRegistry(...)` builder option — and
its emit goroutine drains that registry to `metric.1` logs on a timer. Because fx
owns `main` and witchcraft is a managed component (see [0005](./0005-fx-wrapping-witchcraft.md)),
we cannot fish a registry out of the constructed server without a startup-ordering
hazard.

## Decision
Register the service's metrics directly on `metrics.DefaultMetricsRegistry` — the
same global witchcraft emits from — via a small fx seam:

- [`internal/metrics`](../../internal/metrics) provides the registry
  (`func NewRegistry() metrics.RootRegistry { return metrics.DefaultMetricsRegistry }`)
  and an `*Outbox` emitter holding the metric name/tag constants.
- The worker, the timeout reaper, and the signal-cli TCP client depend on small
  **consumer-owned interfaces** (`worker.Metrics`, `signal.Metrics`) that `*Outbox`
  satisfies, bound by adapter providers in each module — the same dependency-
  inversion pattern already used for repo/storage/sender.

Metric names live under `signalium.outbox.*`; the full catalogue is in
[`docs/observability.md`](../observability.md).

Because the registry is a process global, fx constructors capture the exact object
witchcraft later emits from regardless of construction order, so the
fx-wraps-witchcraft inversion is a non-issue. No new third-party dependency is
introduced — `github.com/palantir/*` is already on the depguard allowlist and
`pkg/metrics`/`go-metrics` were already transitive deps.

## Consequences
- **No second telemetry path.** Custom metrics ride witchcraft's existing emit
  pipeline; an operator who can read `metric.1` logs already sees them.
- **Trivially testable.** Tests construct an `Outbox` over a throwaway
  `metrics.NewRootMetricsRegistry()`, so the real emission path runs in unit tests
  without asserting on global state.
- **Small interfaces keep the seam honest.** The worker depends on `ObserveSend`,
  not on a metrics library.
- **Default timer aggregate blocklist applies.** witchcraft suppresses
  `p50/mean/min` and the rolling rates for timers; dashboards use `count/max/p99/p999`.
  Documented so it does not surprise.

## Alternatives considered
- **A separate Prometheus registry + `/metrics` endpoint.** Adds a dependency and a
  second emit path for no benefit here; witchcraft's registry already feeds the log
  pipeline the rest of the service uses.
- **Threading a registry object through fx from the server provider.** Not possible
  cleanly — witchcraft does not expose its registry, and constructing one ourselves
  would mean witchcraft emits from a *different* registry than our code registers on.
- **`metrics.FromContext(ctx)` child registries per request.** Right for
  request-scoped handler metrics; the outbox work is background (no request ctx), so
  the root registry is the correct home.

## Revisit if
- We adopt an external metrics backend (Prometheus/OTel). At that point add an
  exporter that reads the same registry rather than re-instrumenting call sites.
- Per-tenant or per-recipient cardinality becomes interesting — re-evaluate the tag
  set against cardinality limits before adding high-cardinality tags.
