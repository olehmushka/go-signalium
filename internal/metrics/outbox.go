// Package metrics emits the service's custom application metrics onto the same
// registry witchcraft drains to metric.1 logs.
//
// witchcraft-go-server hardwires the process-global metrics.DefaultMetricsRegistry
// as its root and emits everything registered on it on a timer, so any metric
// registered here surfaces automatically — no scrape endpoint or extra wiring is
// needed. docs/observability.md documents the metric names, tags, and how to read
// them. ADR docs/decisions/0009-observability-metrics.md records the rationale.
package metrics

import (
	"time"

	metrics "github.com/palantir/pkg/metrics"
)

// Metric names. The witchcraft root prefix (product name) is prepended on emit,
// so these read as e.g. "<product>.signalium.outbox.send" in the metric.1 log.
const (
	claimLatency     = "signalium.outbox.claim"    // timer: time to claim one row
	sendLatency      = "signalium.outbox.send"     // timer: signal-cli send roundtrip, tagged by outcome
	terminal         = "signalium.outbox.terminal" // counter: rows reaching a terminal status
	retry            = "signalium.outbox.retry"    // counter: transient failures scheduled for retry
	inboundDropped   = "signalium.outbox.inbound.dropped"
	backlogDepth     = "signalium.outbox.backlog.depth"
	backlogOldestAge = "signalium.outbox.backlog.oldest_age_seconds"
)

// Tag keys.
const (
	tagOutcome = "outcome"
	tagStatus  = "status"
	tagMethod  = "method"
)

// Terminal-status tag values. They mirror the lower-cased domain statuses so a
// dashboard can pivot terminal counts by reason.
const (
	StatusSent            = "sent"
	StatusPermanentFailed = "permanent_failed"
	StatusTimedOut        = "timed_out"
)

// Outbox emits the signalium.outbox.* metric family. It holds the registry and
// registers each metric on demand: the underlying go-metrics registry returns
// the existing handle for a repeated name+tags, so this is cheap and idempotent.
type Outbox struct {
	reg metrics.RootRegistry
}

// NewOutbox builds an Outbox that emits onto the given registry. Production code
// passes metrics.DefaultMetricsRegistry (see Module); tests pass a throwaway
// registry from metrics.NewRootMetricsRegistry().
func NewOutbox(reg metrics.RootRegistry) *Outbox { return &Outbox{reg: reg} }

// ObserveClaim records how long a claim attempt took, whether or not it found a
// row. Together with the poll cadence it shows how saturated the worker is.
func (o *Outbox) ObserveClaim(d time.Duration) {
	o.reg.Timer(claimLatency).Update(d)
}

// ObserveSend records the signal-cli send roundtrip latency, tagged by outcome
// so success and error latencies can be compared.
func (o *Outbox) ObserveSend(d time.Duration, ok bool) {
	outcome := "error"
	if ok {
		outcome = "success"
	}
	o.reg.Timer(sendLatency, metrics.MustNewTag(tagOutcome, outcome)).Update(d)
}

// IncTerminal increments the terminal-status counter by one. status should be
// one of the Status* constants in this package.
func (o *Outbox) IncTerminal(status string) {
	o.IncTerminalN(status, 1)
}

// IncTerminalN increments the terminal-status counter by n. The reaper uses it
// to record a whole sweep of timed-out rows in one call; n <= 0 is a no-op.
func (o *Outbox) IncTerminalN(status string, n int64) {
	if n <= 0 {
		return
	}
	o.reg.Counter(terminal, metrics.MustNewTag(tagStatus, status)).Inc(n)
}

// IncRetry counts a transient failure that was rescheduled (not yet terminal).
func (o *Outbox) IncRetry() {
	o.reg.Counter(retry).Inc(1)
}

// IncInboundDropped counts an inbound signal-cli event dropped because the
// events buffer was full. method comes off the wire, so it is tagged with a
// fallback to stay panic-proof against an unexpected value.
func (o *Outbox) IncInboundDropped(method string) {
	o.reg.Counter(inboundDropped, metrics.NewTagWithFallbackValue(tagMethod, method, "unknown")).Inc(1)
}

// SetBacklog publishes the current outbox backlog: the number of rows still
// awaiting delivery and the age of the oldest such row. The reaper samples these
// on its tick so a single goroutine owns both jobs.
func (o *Outbox) SetBacklog(depth int, oldestAge time.Duration) {
	o.reg.Gauge(backlogDepth).Update(int64(depth))
	o.reg.Gauge(backlogOldestAge).Update(int64(oldestAge.Seconds()))
}
