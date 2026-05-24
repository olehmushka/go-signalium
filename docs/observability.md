# Observability

The service emits application metrics through Palantir's metrics registry, the
same one `witchcraft-go-server` already uses for its built-in server metrics.
There is **no separate scrape endpoint and no extra wiring**: witchcraft constructs
its server on the process-global `metrics.DefaultMetricsRegistry`
([`github.com/palantir/pkg/metrics`](https://pkg.go.dev/github.com/palantir/pkg/metrics))
and drains every metric registered on it to `metric.1` log lines on a timer. The
custom metrics here register on that same global, so they ride the same emit path.

The seam is [`internal/metrics`](../internal/metrics): a tiny fx module that
provides the registry (sourced from `metrics.DefaultMetricsRegistry`) and an
`*Outbox` emitter. The worker, the timeout reaper, and the signal-cli TCP client
depend on small consumer-owned interfaces that `*Outbox` satisfies, so unit tests
inject a throwaway registry and the production graph binds the real one. See
[decisions/0009](./decisions/0009-observability-metrics.md) for the rationale.

## Metric catalogue

All names are prefixed with the witchcraft root (the product name) on emit, so they
read as `<product>.signalium.outbox.*` in the logs.

| Metric | Type | Tags | Emitted from | Meaning |
|---|---|---|---|---|
| `signalium.outbox.claim` | Timer | — | `worker.tick` | Latency of one `ClaimPending` round-trip. With the poll cadence, shows how saturated the worker is. |
| `signalium.outbox.send` | Timer | `outcome=success\|error` | `worker.dispatch` | signal-cli send round-trip latency, split by outcome. |
| `signalium.outbox.terminal` | Counter | `status=sent\|permanent_failed\|timed_out` | worker + reaper | Rows reaching a terminal state, by reason. |
| `signalium.outbox.retry` | Counter | — | `worker.fail` | Transient failures rescheduled for a later attempt. |
| `signalium.outbox.backlog.depth` | Gauge | — | reaper tick | Rows still awaiting delivery (`PENDING` + `FAILED`). |
| `signalium.outbox.backlog.oldest_age_seconds` | Gauge | — | reaper tick | Age of the oldest undelivered row — the head-of-line latency. |
| `signalium.outbox.inbound.dropped` | Counter | `method` | `tcp_client.dispatchFrame` | Inbound signal-cli events dropped because the events buffer was full. A non-zero rate means a slow inbound consumer. |

The backlog gauges are sampled by the timeout reaper on its tick (one goroutine
owns both the sweep and the sample), so they update at the reaper cadence rather
than continuously.

## Suggested alerts / dashboards

- **Delivery health:** rate of `terminal{status=permanent_failed}` and
  `terminal{status=timed_out}` vs `terminal{status=sent}`.
- **Backlog building:** `backlog.depth` trending up or `backlog.oldest_age_seconds`
  exceeding your freshness budget → the worker is not keeping up (scale
  `worker.concurrency`, check signal-cli health).
- **Daemon trouble:** `send{outcome=error}` rate, paired with `retry`.
- **Inbound loss:** any `inbound.dropped` → the inbound consumer is too slow or the
  events buffer is undersized.

## Reading the metrics locally

`make run` boots the service with witchcraft's default emit frequency. Trigger
traffic (send a message, force a failure with an unreachable signal-cli, submit a
message with a short `timeoutSeconds`) and watch stdout for `"type":"metric.1"`
lines naming `signalium.outbox.*`.

```bash
make run 2>&1 | grep --line-buffered '"metric.1"' | grep signalium.outbox
```

Two witchcraft behaviours worth knowing:

- **Counters and gauges appear promptly; timers need an observation.** A timer with
  no samples yet emits nothing.
- **Some timer aggregates are blocklisted by default.** witchcraft suppresses
  `p50/mean/min/stddev` and the 1m/5m/15m rates for timers; `count`, `max`, `p99`,
  and `p999` survive. Build dashboards on the surviving keys.

## Visualizing the metrics

A ready-made Grafana dashboard lives at
[`deploy/grafana/outbox-dashboard.json`](../deploy/grafana/outbox-dashboard.json):
three rows — Delivery (terminal-outcome rate, send latency), Backlog (depth, oldest
age), and Worker health (claim latency, retry rate, dropped inbound). The datasource
is a template variable, so it imports cleanly into any Grafana
(*Dashboards → Import → Upload JSON*) and you pick the datasource at load time.

The dashboard targets a **Prometheus** datasource. witchcraft emits to `metric.1`
logs, not to Prometheus, so wiring the two is one deliberate step — pick whichever
fits your platform:

1. **Prometheus exporter on the registry.** Add a small exporter that reads the same
   `metrics.DefaultMetricsRegistry` and serves `/metrics`. This is the path ADR 0009
   flags under "Revisit if" for adopting an external backend; the metric values are
   already there, it is purely an exposition format.
2. **Log-based metrics.** Ship the `metric.1` lines to Loki / a log-metrics pipeline
   and derive series from them.

Either way, the dashboard assumes the conventional mapping from the witchcraft names
to Prometheus: dots become underscores and tags become labels, with timer quantiles
carried on a `quantile` label. For example:

| Witchcraft metric | Prometheus series used by the dashboard |
|---|---|
| `signalium.outbox.terminal{status}` | `signalium_outbox_terminal{status="…"}` |
| `signalium.outbox.send{outcome}` (timer) | `signalium_outbox_send{outcome="…",quantile="0.99"}` |
| `signalium.outbox.backlog.depth` (gauge) | `signalium_outbox_backlog_depth` |
| `signalium.outbox.retry` (counter) | `signalium_outbox_retry` |

If your exporter names things differently, the panel queries are the only thing to
adjust.
