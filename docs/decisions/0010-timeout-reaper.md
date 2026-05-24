# 0010 — Timeout enforcement via a reaper cron + claim exclusion

## Status
Accepted.

## Context
A message can carry a caller-supplied deadline (`timeoutSeconds` → `timeout_at`),
and `TIMED_OUT` is a defined terminal status. But nothing enforced it: the column
was written on insert and never read. An overdue message kept being retried until
it exhausted `max_attempts` and landed in `PERMANENT_FAILED` — the wrong terminal
state, reached late, after burning send attempts on work the caller had already
given up on. `docs/worker.md` even described a timeout sweep that did not exist in
the code.

The state machine was therefore incomplete: `TIMED_OUT` was reachable only by an
operator manually `PUT`-ing the status.

## Decision
Enforce timeouts in two complementary places, neither of which requires a schema
change (the `timeout_at` column already exists):

1. **Exclude overdue rows from the claim.** `ClaimPending` gains
   `AND (timeout_at IS NULL OR timeout_at > now())`, so the worker stops handing
   itself past-deadline messages the instant they expire — no wasted send.
2. **Terminalise overdue rows with a dedicated reaper.**
   [`internal/worker/timeout_reaper.go`](../../internal/worker/timeout_reaper.go)
   is an fx-managed cron — its own `fx.Module` mirroring the tmp-cleanup job
   ([0007](./0007-atlas-for-migrations.md)-era cron style) — that runs once on boot
   and on a fixed one-minute tick. Each tick runs a single set-based statement:

   ```sql
   UPDATE signalium.signal_messages
      SET status = 'TIMED_OUT', modified_at = now()
    WHERE deleted_at IS NULL
      AND status IN ('PENDING', 'SENDING', 'FAILED')
      AND timeout_at IS NOT NULL
      AND timeout_at <= now();
   ```

   `SENDING` is included so a message whose deadline passes while in flight is
   terminalised on the next sweep instead of being re-claimed after its lease
   expires. The reaper also samples the backlog gauges, so one goroutine owns both
   the sweep and the periodic metric sample.

Gated by `cron.timeoutReaper.enabled`. The terminal sweep increments
`signalium.outbox.terminal{status=timed_out}` by the rows-affected count.

## Consequences
- **The state machine is complete.** `TIMED_OUT` is now reached automatically and
  promptly, not only by operator action.
- **No wasted work.** The claim exclusion means an expired message is never sent,
  even in the window before the reaper's next tick.
- **Set-based, idempotent, cheap.** One `UPDATE` per tick; a tick with nothing
  overdue affects zero rows and returns immediately. Safe to run on every replica —
  the statement is naturally idempotent.
- **Consistent operability.** Same fixed-interval cron rhythm as the tmp-cleanup
  job, so operators reason about one cadence.

## Alternatives considered
- **Fold the sweep into the outbox `tick`.** Couples two concerns (claiming vs.
  terminalising) and runs the sweep at the high-frequency poll cadence for no
  benefit; the reaper's one-minute cadence is plenty for a freshness deadline.
- **Per-row scheduled job / timer.** Far more moving parts than a periodic
  set-based sweep, with no upside for this workload.
- **Only exclude in the claim, no reaper.** Overdue rows would linger in `PENDING`
  forever, polluting backlog metrics and `GET` queries. The terminal transition is
  the point.

## Revisit if
- Deadlines get tight enough (sub-minute) that a one-minute reaper cadence is too
  coarse — then either shorten the interval or move enforcement into the claim
  result handling.
- The table grows large enough that the unindexed `timeout_at` predicate scans hurt;
  add a partial index `(timeout_at) WHERE deleted_at IS NULL AND status IN (...)`.
