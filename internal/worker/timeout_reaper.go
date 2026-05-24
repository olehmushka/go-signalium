package worker

import (
	"context"
	"sync"
	"time"

	werror "github.com/palantir/witchcraft-go-error"
	"github.com/palantir/witchcraft-go-logging/wlog/svclog/svc1log"
	"go.uber.org/fx"

	"github.com/olehmushka/go-signalium/internal/config"
	appmetrics "github.com/olehmushka/go-signalium/internal/metrics"
	"github.com/olehmushka/go-signalium/internal/repo"
)

// TimeoutReaperModule wires the cron job that completes the message state
// machine: a row carries an optional caller-supplied timeout_at, and once it
// passes the row must become TIMED_OUT rather than keep retrying until it
// exhausts max_attempts. The outbox claim query already stops handing out
// overdue rows; this reaper terminalises them. Defined as its own fx.Module to
// mirror CleanupModule and keep worker.go focused on the outbox loop.
//
// The reaper also samples the backlog gauges on each tick, so a single goroutine
// owns both the timeout sweep and the periodic metric sample.
// The worker.Metrics adapter is intentionally NOT provided here — worker.Module
// already binds *metrics.Outbox to worker.Metrics, and fx shares providers across
// modules, so re-providing it would be a duplicate. NewTimeoutReaper consumes that
// single binding.
var TimeoutReaperModule = fx.Module(
	"worker-timeout-reaper",
	fx.Provide(
		func(r *repo.Messages) TimeoutReaperRepo { return r },
		NewTimeoutReaper,
	),
	fx.Invoke(RegisterTimeoutReaperLifecycle),
)

// timeoutReaperInterval is the fixed sweep cadence. Matches the tmp-cleanup job
// so operators see one consistent "every minute" cron rhythm until the schedule
// parser lands.
const timeoutReaperInterval = time.Minute

// TimeoutReaperRepo is the slice of repo.Messages the reaper needs.
type TimeoutReaperRepo interface {
	MarkTimedOut(ctx context.Context) (int64, error)
	BacklogStats(ctx context.Context) (depth int, oldest time.Duration, err error)
}

// TimeoutReaper terminalises overdue rows and samples backlog gauges on a fixed
// tick. One goroutine, fx-managed; the loop body is best-effort and logs rather
// than aborting on a transient DB error.
type TimeoutReaper struct {
	repo    TimeoutReaperRepo
	metrics Metrics
	cfg     config.TimeoutReaperConfig
	logger  svc1log.Logger

	stopCancel context.CancelFunc
	wg         sync.WaitGroup
}

// NewTimeoutReaper is the fx provider.
func NewTimeoutReaper(r TimeoutReaperRepo, m Metrics, install config.Install, logger svc1log.Logger) *TimeoutReaper {
	return &TimeoutReaper{
		repo:    r,
		metrics: m,
		cfg:     install.Cron.TimeoutReaper,
		logger:  logger,
	}
}

// Enabled reports whether the reaper loop will run.
func (t *TimeoutReaper) Enabled() bool { return t.cfg.Enabled }

// Start launches the reaper goroutine. Returns immediately; a no-op when disabled.
func (t *TimeoutReaper) Start(ctx context.Context) {
	if !t.Enabled() {
		t.logger.Info("timeout reaper disabled")
		return
	}
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	t.stopCancel = cancel
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		t.run(loopCtx)
	}()
	t.logger.Info("timeout reaper started",
		svc1log.SafeParam("interval", timeoutReaperInterval.String()))
}

// Stop cancels the loop and waits for the goroutine to drain.
func (t *TimeoutReaper) Stop(ctx context.Context) error {
	if t.stopCancel == nil {
		return nil
	}
	t.stopCancel()
	done := make(chan struct{})
	go func() { t.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return werror.WrapWithContextParams(ctx, ctx.Err(), "timeout reaper stop: drain timeout")
	}
}

func (t *TimeoutReaper) run(ctx context.Context) {
	tk := time.NewTicker(timeoutReaperInterval)
	defer tk.Stop()
	// Run once on boot so rows that expired while the process was down do not
	// wait a full interval before being terminalised.
	t.reap(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			t.reap(ctx)
		}
	}
}

// reap flips overdue rows to TIMED_OUT and republishes the backlog gauges. Both
// steps are independent and best-effort: a failure in one is logged and does not
// skip the other.
func (t *TimeoutReaper) reap(ctx context.Context) {
	if n, err := t.repo.MarkTimedOut(ctx); err != nil {
		t.logger.Warn("timeout reaper sweep failed", svc1log.Stacktrace(err))
	} else if n > 0 {
		t.metrics.IncTerminalN(appmetrics.StatusTimedOut, n)
		t.logger.Info("timeout reaper sweep", svc1log.SafeParam("timedOut", n))
	}

	if depth, oldest, err := t.repo.BacklogStats(ctx); err != nil {
		t.logger.Warn("backlog sample failed", svc1log.Stacktrace(err))
	} else {
		t.metrics.SetBacklog(depth, oldest)
	}
}

// RegisterTimeoutReaperLifecycle binds Start/Stop into fx.
func RegisterTimeoutReaperLifecycle(lc fx.Lifecycle, t *TimeoutReaper) {
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			t.Start(ctx)
			return nil
		},
		OnStop: func(ctx context.Context) error {
			return t.Stop(ctx)
		},
	})
}
