# 0011 — Send delivery semantics: at-least-once, with a bounded-retry mitigation

## Status
Accepted.

## Context
The outbox dispatch does three things in sequence: claim a row (`SENDING`), send it
via signal-cli, then `MarkSent`. signal-cli accepts the send before we record it,
so there is a classic gap: if `MarkSent` fails after a successful send, the row
stays `SENDING`, its lease eventually expires, another tick re-claims it, and the
message is **sent a second time**.

Signal has no application-level dedup token on `send`, so we cannot make the daemon
reject a duplicate. The delivery contract is therefore fundamentally
**at-least-once**, and the design question is how small we can make the duplicate
window without over-engineering.

## Decision
Take the cheap, schema-free mitigation now and document the principled fix as the
next step rather than building it speculatively.

**Now — bounded `MarkSent` retry.** `worker.markSent` retries the SENT transition a
few times with a short backoff before giving up. This absorbs the common cause of
the gap — a transient DB/pool hiccup right after a successful send — so a duplicate
only remains possible if the process *crashes* between the send and a committed
`MarkSent`. No schema change, no new failure modes.

**Documented as the principled fix (not yet implemented).** To close the crash
window, make re-claim idempotent:

1. Persist the signal-cli `result_id` while the row is still `SENDING` (a separate,
   earlier write than the `SENDING → SENT` flip).
2. Surface `result_id` in `ClaimPending`.
3. In `dispatch`, if a re-claimed row already has a `result_id`, treat the send as
   already done — go straight to `MarkSent` and skip the re-send.

That turns the second delivery into a no-op for any row that got as far as recording
its result, eliminating the duplicate for everything except a crash *between* the
send and the result_id write — a strictly smaller window than today.

## Consequences
- **Honest contract.** Consumers must treat delivery as at-least-once and dedupe on
  `idempotencyKey` / `result_id` if they need exactly-once *effects*. Stated here and
  in the API docs rather than implied.
- **The common duplicate is gone now.** Transient DB blips no longer leak a
  duplicate; only a process crash in a narrow window can.
- **The next step is scoped, not hand-wavy.** The idempotent-reclaim design above is
  ready to implement when the crash window justifies the extra write and the
  `ClaimPending` change.

## Alternatives considered
- **Daemon-side dedup token.** Cleanest in theory; signal-cli's `send` does not
  support it, so it is not available.
- **Two-phase commit across Postgres and signal-cli.** No shared transaction
  coordinator exists between them; not worth building for a messaging side effect.
- **Implement idempotent re-claim immediately.** Reasonable, but it adds a write to
  the hot path and a claim-query change for a window that the bounded retry already
  shrinks to crash-only. Deferred until measured duplicates justify it.

## Revisit if
- Observed duplicates (or a consumer that cannot dedupe) make the crash window
  matter — implement the idempotent-reclaim fix described above.
- signal-cli gains a client-supplied request/dedup id — then prefer pushing dedup to
  the daemon.
