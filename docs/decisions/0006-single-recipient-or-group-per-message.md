# 0006 — One recipient OR one group per message

## Status
Accepted.

## Context
signal-cli's underlying `send` RPC accepts arrays — multiple recipients and multiple groups in a single call. A naive contract would expose that directly:

```jsonc
{
  "recipients": ["+380...", "+380..."],   // one or more phone numbers
  "groupIds":   ["abc...", "def..."]      // zero or more group ids
}
```

The array form looks general but pulls in real complexity:

- **Partial success semantics.** "Sent to 2 of 3 recipients" needs its own status terms, retry rules, and per-target error tracking. The state machine grows by an order of magnitude.
- **Storage shape.** Either denormalize per-target rows (multiplies row count) or carry per-target status in JSONB (loses indexability).
- **Retry semantics.** Per-target backoff, per-target attempts counters, idempotency keyed how?

In our observed use cases, every send fans out to **exactly one destination per logical message**. The array model carries the cost of complexity for a code path no caller exercises.

## Decision
Reduce to a single recipient OR a single group per message. Enforce at the DB level with a CHECK constraint:

```sql
CONSTRAINT exactly_one_target CHECK (
    (recipient IS NOT NULL)::int + (group_id IS NOT NULL)::int = 1
)
```

Columns: `recipient TEXT NULL` and `group_id TEXT NULL`. The Conjure request type mirrors this with `recipient` and `groupId` fields, validated as a one-of at the IDL layer.

## Consequences
- **No partial-success ambiguity.** A message is fully sent, fully failed, fully pending, or timed-out — never "sent to 2 of 3 recipients." Retry semantics collapse to a single path.
- **No JSON storage for repeatable phone numbers.** `recipient` is just `TEXT`. Trivial primary-key-style lookups; no GIN.
- **Schema CHECK catches caller mistakes** at the boundary, not deep in business logic. The integration test [`TestMessages_RecipientXorGroupConstraint`](../../internal/repo/integration_test.go) pins this.
- **Callers that need fan-out** call `POST /api/v1/signal-messages` once per destination. Attachments are uploaded once per call; callers that care about dedup pre-upload to MinIO out-of-band and rely on `idempotencyKey` at the message level.

## Alternatives considered
- **Keep arrays.** No real caller needs them; complexity tax for hypothetical use.
- **One recipient OR one group, but no CHECK constraint.** Application-level validation works until the day someone bypasses it via a direct SQL insert. The CHECK is cheap.
- **`destination_type` + `destination_id` columns.** Single nullable value column, an enum for the type. More normalized but loses type clarity — `recipient` and `group_id` make queries self-documenting.

## Boundary
Multiple **attachments** per message remain supported — the simplification is destination-side only.

## Revisit if
A caller appears with sustained fan-out patterns where per-destination POSTs are a real bottleneck. At that point, a batch endpoint accepting an array of complete `CreateSignalMessage` requests (each still one-recipient-OR-one-group) is the natural shape — not re-introducing arrays into one message.
