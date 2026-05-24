package metrics

import (
	metrics "github.com/palantir/pkg/metrics"
	"go.uber.org/fx"
)

// Module provides the metrics registry and the *Outbox emitter to the fx graph.
//
// The registry is sourced from the package-global metrics.DefaultMetricsRegistry
// — the exact object witchcraft constructs its server on and drains on a timer.
// Because it is a global, fx constructors that capture it get the same instance
// regardless of whether they run before or after the witchcraft server starts,
// so the fx-wraps-witchcraft ordering is a non-issue.
var Module = fx.Module(
	"metrics",
	fx.Provide(
		NewRegistry,
		NewOutbox,
	),
)

// NewRegistry returns the process-global root metrics registry that witchcraft
// emits from.
func NewRegistry() metrics.RootRegistry { return metrics.DefaultMetricsRegistry }
