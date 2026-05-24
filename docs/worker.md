# Outbox worker

The worker is a single fx-managed goroutine that drives `signalium.signal_messages` rows through their state machine. It is the heart of the delivery pipeline — the REST handler only accepts and persists.

## Loop

```go
func (w *Worker) loop(ctx context.Context) {
    t := time.NewTicker(jitter(w.pollInterval))
    defer t.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-t.C:
            if err := w.tick(ctx); err != nil {
                svc1log.FromContext(ctx).Warn("outbox tick failed", svc1log.Stacktrace(err))
            }
        }
    }
}

func (w *Worker) tick(ctx context.Context) error {
    msg, err := w.repo.ClaimPending(ctx, w.leaseDuration)
    if errors.Is(err, pgx.ErrNoRows) { return nil }
    if err != nil { return werror.Wrap(err, "claim") }
    w.dispatch(ctx, msg)
    return nil
}
```

`ClaimPending` is the load-bearing query — see [`persistence.md`](./persistence.md#generated-queries-m2). It atomically:

1. Finds one row that is `PENDING`, OR `SENDING` with an expired lease (`next_attempt_at < now()`).
2. Sets `status='SENDING'`, increments `attempts`, and writes a fresh `next_attempt_at = now() + leaseDuration`.
3. Returns the row.

The `FOR UPDATE SKIP LOCKED` clause lets multiple workers (multiple replicas) run concurrently without claiming the same row.

## Lease — why and how

The lease is the `next_attempt_at` value while `status='SENDING'`. It's set on claim and acts as a "this worker has until time T to finish, or another worker can re-claim it."

This handles:

- **Ungraceful crash.** Worker SIGKILLed mid-send: row stays in SENDING but with `next_attempt_at` in the past after `leaseDuration`. Next tick on any replica picks it up.
- **Network partition.** Worker partitioned from DB but still talking to signal-cli: same recovery via lease.

The lease does **not** handle graceful shutdown — that's done by cancelling the worker context in `OnStop` and waiting for the current send to finish (bounded by the witchcraft shutdown timeout). The lease is the "everything went sideways" backstop.

`leaseDuration` should be ≥ longest plausible send time (file downloads + signal-cli round trip + buffer). Default 5 minutes. Configured in install.yml.

## Dispatch

```go
func (w *Worker) dispatch(ctx context.Context, m domain.SignalMessage) {
    sendCtx, cancel := context.WithTimeout(ctx, w.perAttemptTimeout)
    defer cancel()

    paths, err := w.attachments.Download(sendCtx, m)
    if err != nil { w.fail(sendCtx, m, err); return }

    result, err := w.signal.Send(sendCtx, w.builder.For(m, paths))
    if err != nil { w.fail(sendCtx, m, err); return }

    if err := w.repo.MarkSent(sendCtx, m.ID, result.ResultID); err != nil {
        svc1log.FromContext(ctx).Error("mark sent failed", svc1log.Stacktrace(err))
        return
    }
    w.attachments.CleanupLocal(m.ID) // best-effort
}

func (w *Worker) fail(ctx context.Context, m domain.SignalMessage, err error) {
    next := time.Now().Add(backoff(m.Attempts))
    if mappable := w.slack(); mappable.Enabled() && permanent(m) {
        _ = mappable.NotifyFailure(ctx, m, err)
    }
    _ = w.repo.MarkFailed(ctx, m.ID, err.Error(), next)
}
```

## Backoff

Exponential backoff with full jitter; bounded by a configurable ceiling:

```
delay = min(maxDelay, baseDelay * 2^(attempts-1))
sleep = uniform_random(0, delay)   # full jitter
baseDelay = 5s     # runtime-configurable
maxDelay  = 1h     # runtime-configurable
```

Full jitter (rather than `delay + jitter(0, delay/4)`) prevents thundering herds on a multi-replica deploy after a daemon outage — every worker resolves on a different random offset rather than clustering near a fixed point.

Implementation: [`internal/service/backoff.go`](../internal/service/backoff.go). Verified by [`internal/service/backoff_test.go`](../internal/service/backoff_test.go) including the cap clamp.

## Permanent failure

When `MarkFailed` is called with `attempts >= max_attempts`, the SQL CASE sets `status='PERMANENT_FAILED'`. The next worker tick will NOT pick the row up (status filter doesn't match). Only an operator action (`POST .../resend`) revives it.

Permanent failure triggers a Slack notification when enabled. Transient failures do not.

## TIMED_OUT

If `timeout_at` is set on the row and `now() > timeout_at`, the row is marked `TIMED_OUT` regardless of attempt count. This handles "the message is no longer fresh enough to be useful" scenarios.

The timeout sweep is a separate query, run on the same ticker as `ClaimPending`:

```sql
UPDATE signalium.signal_messages
   SET status = 'TIMED_OUT', modified_at = now()
 WHERE deleted_at IS NULL
   AND status IN ('PENDING', 'FAILED')
   AND timeout_at IS NOT NULL
   AND timeout_at < now();
```

## Cooperation with witchcraft shutdown

```go
// internal/app/worker/module.go
func RegisterLifecycle(lc fx.Lifecycle, w *service.Worker) {
    workerCtx, cancel := context.WithCancel(context.Background())
    lc.Append(fx.Hook{
        OnStart: func(_ context.Context) error {
            go w.Run(workerCtx) // blocks until ctx cancelled
            return nil
        },
        OnStop: func(stopCtx context.Context) error {
            cancel()
            return w.Wait(stopCtx) // returns when goroutine exits or stopCtx expires
        },
    })
}
```

`OnStop` cancels the loop ctx and waits up to the witchcraft shutdown timeout. In-flight dispatch sees `sendCtx.Done()` and the signal-cli client cancels the pending request; the row stays in `SENDING` and the lease handles re-claim.

## Concurrency

Single-message-at-a-time per worker by default — `ClaimPending` returns one row. To increase throughput, set `worker.concurrency > 1` in install.yml: the worker fans out via a bounded `errgroup.Group` that calls `ClaimPending` and `dispatch` concurrently up to N. Each goroutine has its own claim, so `SKIP LOCKED` keeps them from racing.
