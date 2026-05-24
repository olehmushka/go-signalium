// Package worker is the outbox processor. A single fx-managed goroutine
// claims PENDING signal_messages rows, downloads any attachments to local
// disk, hands them to the signal-cli TCP client, and marks the row
// SENT/FAILED based on the outcome. docs/worker.md is the design source.
package worker

import (
	"context"
	"errors" //nolint:depguard // stdlib errors.Is for domain sentinel + ctx comparison
	"math/rand/v2"
	"sync"
	"time"

	"github.com/google/uuid"
	werror "github.com/palantir/witchcraft-go-error"
	"github.com/palantir/witchcraft-go-logging/wlog/svclog/svc1log"
	"go.uber.org/fx"

	"github.com/olehmushka/go-signalium/internal/config"
	"github.com/olehmushka/go-signalium/internal/domain"
	appmetrics "github.com/olehmushka/go-signalium/internal/metrics"
	"github.com/olehmushka/go-signalium/internal/repo"
	"github.com/olehmushka/go-signalium/internal/service"
	"github.com/olehmushka/go-signalium/internal/signal"
	"github.com/olehmushka/go-signalium/internal/storage"
)

// Module wires the worker. OnStart spawns the loop in a background goroutine
// bound to a context that OnStop cancels. OnStop waits up to the witchcraft
// shutdown timeout for the in-flight dispatch to finish; the row's lease
// (next_attempt_at while SENDING) ensures another worker can re-claim if we
// were killed mid-send.
var Module = fx.Module(
	"worker",
	fx.Provide(
		// Adapter providers bind concrete *repo.Messages / *storage.ObjectStore
		// / *signal.TCPClient / *service.SlackNotifier to the consumer-owned
		// Repo / Downloader / Sender / FailureNotifier interfaces. Mirrors
		// the same pattern in internal/service.
		func(r *repo.Messages) Repo { return r },
		func(s *storage.ObjectStore) Downloader { return s },
		func(c *signal.TCPClient) Sender { return c },
		func(n *service.SlackNotifier) FailureNotifier { return n },
		func(o *appmetrics.Outbox) Metrics { return o },
		NewWorker,
	),
	fx.Invoke(RegisterLifecycle),
)

// Metrics is the slice of metrics.Outbox the worker and the timeout reaper emit
// to. Kept as a consumer-owned interface so tests can inject a throwaway emitter.
type Metrics interface {
	ObserveClaim(d time.Duration)
	ObserveSend(d time.Duration, ok bool)
	IncTerminal(status string)
	IncTerminalN(status string, n int64)
	IncRetry()
	SetBacklog(depth int, oldestAge time.Duration)
}

// Repo is the slice of repo.Messages the worker needs.
type Repo interface {
	ClaimPending(ctx context.Context, lease time.Duration) (domain.SignalMessage, error)
	MarkSent(ctx context.Context, id uuid.UUID, resultID string) error
	MarkFailed(ctx context.Context, id uuid.UUID, lastErr string, nextAttemptAt time.Time) error
}

// Downloader is the slice of storage.ObjectStore the worker needs to fetch
// attachments before invoking signal-cli.
type Downloader interface {
	DownloadAll(ctx context.Context, messageID string, refs []storage.ObjectRef) ([]string, error)
	CleanupLocal(ctx context.Context, messageID string)
}

// Sender is the slice of signal.TCPClient the worker uses to dispatch.
type Sender interface {
	Send(ctx context.Context, params any) (signal.SendResult, error)
}

// FailureNotifier is the small slice of service.SlackNotifier the worker
// uses to alert on terminal failure. Enabled() lets the worker skip building
// arguments when the notifier is disabled.
type FailureNotifier interface {
	Enabled() bool
	NotifyPermanentFailure(ctx context.Context, m domain.SignalMessage, cause error)
}

// Worker drives one signal_messages row at a time through the state machine.
// Concurrency >1 is supported via the install config; each goroutine has its
// own ClaimPending so SKIP LOCKED keeps them from racing.
type Worker struct {
	repo     Repo
	store    Downloader
	signal   Sender
	notifier FailureNotifier
	metrics  Metrics
	cfg      config.WorkerConfig
	account  string
	logger   svc1log.Logger

	stopCancel context.CancelFunc
	done       chan struct{}
}

// NewWorker is the fx provider. Constructor parameters are the small,
// consumer-owned interfaces; concrete production types are bound to them by
// adapter providers in Module so tests can inject in-memory fakes directly.
func NewWorker(
	r Repo,
	store Downloader,
	sender Sender,
	notifier FailureNotifier,
	m Metrics,
	install config.Install,
	logger svc1log.Logger,
) *Worker {
	return &Worker{
		repo:     r,
		store:    store,
		signal:   sender,
		notifier: notifier,
		metrics:  m,
		cfg:      install.Worker,
		account:  install.SignalCli.SenderPhoneNumber,
		logger:   logger,
		done:     make(chan struct{}),
	}
}

// RegisterLifecycle ties Worker.Run to fx Start/Stop.
func RegisterLifecycle(lc fx.Lifecycle, w *Worker) {
	lc.Append(fx.Hook{
		OnStart: func(startCtx context.Context) error {
			// The worker loop must outlive fx's OnStart ctx (witchcraft cancels
			// that as soon as startup completes), but it should still inherit
			// any logger/tracing values attached to it — use WithoutCancel so
			// values propagate and a fresh cancel scope is added on top.
			ctx, cancel := context.WithCancel(context.WithoutCancel(startCtx))
			w.stopCancel = cancel
			go w.Run(ctx)
			return nil
		},
		OnStop: func(ctx context.Context) error {
			if w.stopCancel != nil {
				w.stopCancel()
			}
			select {
			case <-w.done:
				return nil
			case <-ctx.Done():
				return werror.WrapWithContextParams(ctx, ctx.Err(), "worker stop: drain timeout")
			}
		},
	})
}

// Run is the loop. Concurrency=1 dispatches inline; >1 fans out via a fixed
// worker-pool goroutine pool.
func (w *Worker) Run(ctx context.Context) {
	defer close(w.done)
	poll := w.cfg.PollInterval
	if poll <= 0 {
		poll = time.Second
	}
	if w.cfg.Concurrency <= 1 {
		w.runSingle(ctx, poll)
		return
	}
	w.runConcurrent(ctx, poll, w.cfg.Concurrency)
}

func (w *Worker) runSingle(ctx context.Context, poll time.Duration) {
	t := time.NewTicker(jitter(poll))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !w.tick(ctx) {
				continue
			}
			// Successful claim — try again immediately to drain a backlog
			// before falling back to the ticker cadence.
			for w.tick(ctx) {
				if ctx.Err() != nil {
					return
				}
			}
		}
	}
}

func (w *Worker) runConcurrent(ctx context.Context, poll time.Duration, n int) {
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.runSingle(ctx, poll)
		}()
	}
	wg.Wait()
}

// tick attempts to claim one row and dispatch it. Returns true iff a row
// was claimed (so the caller can hot-loop until the table drains).
func (w *Worker) tick(ctx context.Context) bool {
	start := time.Now()
	msg, err := w.repo.ClaimPending(ctx, w.cfg.LeaseDuration)
	w.metrics.ObserveClaim(time.Since(start))
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return false
		}
		w.logger.Warn("claim pending failed", svc1log.Stacktrace(err))
		return false
	}
	w.dispatch(ctx, msg)
	return true
}

func (w *Worker) dispatch(ctx context.Context, msg domain.SignalMessage) {
	sendCtx, cancel := context.WithTimeout(ctx, w.cfg.PerAttemptTimeout)
	defer cancel()

	refs := toRefs(msg.Attachments)
	paths, err := w.store.DownloadAll(sendCtx, msg.ID.String(), refs)
	if err != nil {
		w.fail(sendCtx, msg, werror.Wrap(err, "download attachments"))
		return
	}
	defer w.store.CleanupLocal(sendCtx, msg.ID.String())

	params, err := signal.BuildSendParams(w.account, msg, paths)
	if err != nil {
		w.fail(sendCtx, msg, err)
		return
	}
	sendStart := time.Now()
	res, err := w.signal.Send(sendCtx, params)
	w.metrics.ObserveSend(time.Since(sendStart), err == nil)
	if err != nil {
		w.fail(sendCtx, msg, err)
		return
	}
	if err := w.markSent(sendCtx, msg.ID, res.ResultID()); err != nil {
		w.logger.Error("mark sent failed",
			svc1log.SafeParam("id", msg.ID.String()),
			svc1log.Stacktrace(err))
		// Row stays in SENDING; the lease expires and a future tick re-claims it.
		// signal-cli already accepted the send, so the re-claim re-sends — a
		// duplicate. markSent's bounded retry shrinks this window to a crash
		// between a successful send and a committed MarkSent; the principled
		// crash-proof fix (idempotent re-claim keyed on result_id) is recorded
		// in docs/decisions/0011-exactly-once-send.md.
		return
	}
	w.metrics.IncTerminal(appmetrics.StatusSent)
}

// markSent retries the SENT transition a few times before giving up. The send
// has already succeeded at signal-cli, so a transient DB blip here would
// otherwise leak a duplicate on the next lease re-claim; a short retry absorbs
// the common case (brief connection/pool hiccup) without a schema change.
func (w *Worker) markSent(ctx context.Context, id uuid.UUID, resultID string) error {
	const attempts = 3
	var err error
	for i := 0; i < attempts; i++ {
		if err = w.repo.MarkSent(ctx, id, resultID); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return err
		}
		if i < attempts-1 {
			if !sleepCtx(ctx, time.Duration(i+1)*50*time.Millisecond) {
				return err
			}
		}
	}
	return err
}

// sleepCtx waits for d or until ctx is cancelled. Returns false if cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (w *Worker) fail(ctx context.Context, msg domain.SignalMessage, cause error) {
	next := time.Now().UTC().Add(service.Backoff(msg.Attempts, w.cfg.BaseBackoff, w.cfg.MaxBackoff))
	if err := w.repo.MarkFailed(ctx, msg.ID, cause.Error(), next); err != nil {
		w.logger.Error("mark failed failed",
			svc1log.SafeParam("id", msg.ID.String()),
			svc1log.Stacktrace(err))
		return
	}
	w.logger.Warn("signal-message attempt failed",
		svc1log.SafeParam("id", msg.ID.String()),
		svc1log.SafeParam("attempts", msg.Attempts),
		svc1log.SafeParam("nextAttemptAt", next.Format(time.RFC3339)),
		svc1log.Stacktrace(cause))
	// MarkFailed's CASE flips status to PERMANENT_FAILED at this attempt count;
	// mirror the predicate here for both the metric and the Slack alert. Attempts
	// on msg is the pre-attempt value, so the row's attempts after MarkFailed ==
	// msg.Attempts.
	if msg.Attempts >= msg.MaxAttempts {
		w.metrics.IncTerminal(appmetrics.StatusPermanentFailed)
		if w.notifier != nil && w.notifier.Enabled() {
			w.notifier.NotifyPermanentFailure(ctx, msg, cause)
		}
	} else {
		w.metrics.IncRetry()
	}
}

func toRefs(att domain.Attachments) []storage.ObjectRef {
	if len(att) == 0 {
		return nil
	}
	refs := make([]storage.ObjectRef, len(att))
	for i, a := range att {
		refs[i] = storage.ObjectRef{Bucket: a.Bucket, Key: a.Filename}
	}
	return refs
}

// jitter spreads worker start so multiple replicas don't all hit the DB at the
// same moment. Returns d±25%.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	delta := time.Duration(rand.Int64N(int64(d) / 2)) //nolint:gosec // worker jitter, not crypto
	return d - d/4 + delta
}
