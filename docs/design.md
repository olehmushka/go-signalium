# Design: a durable Signal sender

This is the narrative companion to the reference docs. It walks through *why*
go-signalium is shaped the way it is — the decisions, the tradeoffs, and the failure
reasoning behind them. The per-topic mechanics live in [`architecture.md`](./architecture.md),
[`worker.md`](./worker.md), [`persistence.md`](./persistence.md), and the
[ADRs](./decisions); each decision below links to the record that pins it down.

## The problem

Sending a Signal message from a backend sounds trivial until you actually depend on
it. The only practical way to talk to Signal is [`signal-cli`](https://github.com/AsamK/signal-cli),
a JVM daemon you drive over JSON-RPC. It can be slow, it restarts, it loses its
connection, and a send can fail for reasons that are either transient (daemon
reconnecting) or permanent (the recipient blocked you). If a request thread calls
signal-cli synchronously, every one of those failure modes becomes the caller's
problem, and a daemon hiccup turns into a wall of 500s.

So the real requirement isn't "send a message." It's: **accept a send request,
guarantee it is eventually delivered or definitively failed, and never lose it across
crashes or daemon outages** — while giving the caller a fast, predictable answer.

That requirement is what shapes everything else.

## The central idea: split acceptance from delivery

The whole design hinges on one move: the request path does **not** send. It validates,
persists the message as a `PENDING` row, and returns `202 Accepted` immediately. A
separate background worker drains those rows to signal-cli. The two halves only ever
meet through Postgres.

This is the [transactional outbox pattern](https://microservices.io/patterns/data/transactional-outbox.html),
and choosing it buys three properties at once:

- **Durability.** A message that returns `202` is committed to Postgres. A process
  crash, a deploy, or a signal-cli outage cannot lose it — it is just a row waiting to
  be claimed.
- **Backpressure isolation.** signal-cli being slow or down never blocks or fails the
  accept path. Work piles up as rows, not as stuck request threads.
- **Horizontal scale without a coordinator.** Because the queue *is* a Postgres table,
  multiple replicas drain it concurrently with no broker and no leader election (more
  on how, below).

The cost is that callers learn the outcome by polling `GET /…/{id}` rather than from
the original response. That tradeoff — polling over webhooks/callbacks — is deliberate
and recorded in [ADR 0002](./decisions/0002-polling-over-webhooks.md): no broker, no
inbound webhook endpoint to secure, no delivery-of-the-delivery-notification problem.
REST as the inbound trigger (rather than a queue consumer) is [ADR 0001](./decisions/0001-rest-as-inbound-trigger.md).

## A message's life

```
POST (multipart) ─► validate ─► stream attachments to MinIO ─► INSERT PENDING ─► 202 {id}
                                                                      │
                            ┌─────────────────────────────────────────┘
                            ▼
   worker: CLAIM (SKIP LOCKED) ─► download attachments ─► signal-cli send ─► MarkSent
                            │                                   │
                            └── on error: backoff + MarkFailed ─┘   (PERMANENT_FAILED at max attempts)
```

1. The client posts `multipart/form-data`: a `metadata` JSON part plus zero or more
   `attachments` file parts. Attachments **stream** straight to MinIO with
   `objectSize=-1` — the service never buffers a whole file in memory.
2. The send service validates, applies idempotency, and inserts one `PENDING` row with
   the attachment references in a JSONB column.
3. The worker claims the row, pulls the attachments down to the daemon's local disk,
   issues the JSON-RPC `send`, and records the result.

One wrinkle worth calling out: multipart streaming is the one place the service
*bypasses* Conjure. Conjure models JSON bodies, not `multipart/form-data`, so the
upload endpoint is a raw `wrouter` handler that shares the Conjure-generated request
*type* for the metadata part — [ADR 0008](./decisions/0008-conjure-bypass-for-multipart.md).
Everything else goes through the generated handlers.

## The worker, in depth

The worker is the heart of the system, and three details make it correct.

**Claiming is a single atomic statement.** `ClaimPending` finds one eligible row and
flips it to `SENDING` in the same `UPDATE … RETURNING`, under `FOR UPDATE SKIP LOCKED`:

```sql
WITH claimed AS (
  SELECT signal_message_id FROM signalium.signal_messages
   WHERE deleted_at IS NULL
     AND (status = 'PENDING' OR (status = 'SENDING' AND next_attempt_at < now()))
     AND next_attempt_at <= now()
     AND (timeout_at IS NULL OR timeout_at > now())
   ORDER BY next_attempt_at
   FOR UPDATE SKIP LOCKED LIMIT 1
)
UPDATE … SET status='SENDING', attempts=attempts+1,
             next_attempt_at = now() + lease  …
```

`SKIP LOCKED` is what makes multi-replica scale free: two workers running this query
at the same instant skip each other's locked rows and each get a *different* message.
No coordinator, no double-claim.

**The lease is the crash backstop.** When a row is claimed, its `next_attempt_at` is
pushed `lease` into the future *while it is `SENDING`*. If the worker finishes, the row
moves to a terminal state and the lease is moot. If the worker is `SIGKILL`ed or
partitioned mid-send, the row sits in `SENDING` until the lease expires — at which
point the `(status='SENDING' AND next_attempt_at < now())` arm of the claim makes it
re-claimable by any replica. The system never needs manual database surgery to recover.

**Retries use exponential backoff with jitter.** A transient failure schedules
`next_attempt_at = now() + backoff(attempts)`; at `attempts >= max_attempts` a SQL
`CASE` flips the row to `PERMANENT_FAILED`. Full jitter (not a fixed offset) keeps a
fleet of replicas from re-converging into a thundering herd after a daemon outage.

## Failure model and delivery semantics

I think this is the most important section, because it's where a design either is
honest or pretends. Four failure classes, each with its own recovery primitive:

| Failure | Recovery |
|---|---|
| Worker crash mid-send | Lease expiry → re-claim by any replica |
| Transient signal-cli error | `MarkFailed` schedules a backed-off retry |
| Permanent failure (`attempts ≥ max`) | Terminal `PERMANENT_FAILED`; operator `resend` revives it |
| Deadline passed (`timeout_at`) | Claim excludes it; the reaper marks it `TIMED_OUT` |

That last row was a genuine gap I closed: `timeout_at` was being written but never
enforced, so a stale message was retried all the way to `PERMANENT_FAILED` instead of
timing out. Now the claim query excludes overdue rows *and* a dedicated reaper cron
terminalises them — [ADR 0010](./decisions/0010-timeout-reaper.md).

**The honest part: delivery is at-least-once, not exactly-once.** The dispatch order is
claim → send → `MarkSent`. signal-cli accepts the send *before* we record it, so if
`MarkSent` fails after a successful send, the lease eventually re-claims the row and
the message is sent twice. There is no shared transaction across Postgres and
signal-cli to make this atomic, and signal-cli's `send` has no client-supplied dedup
token, so the duplicate window cannot simply be wished away.

What we *can* do is bound it. `MarkSent` is wrapped in a short retry, which absorbs the
common cause (a transient DB blip) and shrinks the window to a genuine process crash
between the send and the commit. The principled fix — persist the `result_id` while
still `SENDING` and make re-claim idempotent — is designed out in
[ADR 0011](./decisions/0011-exactly-once-send.md) and deferred until measured
duplicates justify the extra write. Consumers that need exactly-once *effects* dedupe
on `idempotencyKey` / `result_id`; that contract is stated, not implied.

## The platform bet

The service runs on Palantir's open-source Go stack, and the most interesting choice
there is an inversion. [witchcraft-go-server](https://github.com/palantir/witchcraft-go-server)
normally owns `main` and the process lifecycle. Here, [uber-go/fx](https://uber-go.github.io/fx/)
owns `main`, and witchcraft is a single fx-managed component constructed with
`WithDisableSigQuitHandler()` so it doesn't fight fx for signal handling. Routes are
registered inside witchcraft's `InitFunc`, and an `fx.Invoke` wires `srv.Start` /
`srv.Shutdown` into the lifecycle. The reasoning, and the documented contingency if
signal handling ever proves troublesome, are in [ADR 0005](./decisions/0005-fx-wrapping-witchcraft.md).

Why bother? Because fx's constructor graph is the cleanest way to express this system's
dependency structure — pools, clients, the worker, the crons — with start/stop hooks
and graceful drain, and witchcraft brings health/readiness/metrics/structured logging
for free ([ADR 0004](./decisions/0004-witchcraft-full-framework.md)). The rest of the
stack follows the same "lean on typed, generated, single-source-of-truth tooling" tenet:
[Conjure](https://github.com/palantir/conjure) IDL for the API types and errors, sqlc
for typed queries, [Atlas](https://atlasgo.io/) for versioned migrations
([ADR 0007](./decisions/0007-atlas-for-migrations.md)), and `werror`/`wlog` for
stack-bearing errors and structured logs (enforced by depguard in `.golangci.yml`).

A pattern repeats throughout the code: **interfaces are owned by the consumer, not the
producer.** The worker declares the three-method `Repo`, `Downloader`, `Sender` slices
it actually needs; fx adapter providers bind the concrete `*repo.Messages` /
`*storage.ObjectStore` / `*signal.TCPClient` to them. Production wires the real types;
tests inject in-memory fakes against the same small interfaces with no mocking
framework. The metrics seam added the same way — a `Metrics` interface per consumer,
one `*metrics.Outbox` behind all of them.

## Observability

For a queue-shaped service, "is it keeping up?" and "why is it failing?" are the
questions that matter, so the worker, the reaper, and the TCP client emit a
`signalium.outbox.*` metric family — send/claim latency, terminal-status and retry
rates, backlog depth and oldest-age, dropped inbound events.

The neat part is the wiring: witchcraft hardwires the process-global
`metrics.DefaultMetricsRegistry` as its root and drains it to `metric.1` logs on a
timer. Registering on that same global means the custom metrics ride the existing emit
path with **no scrape endpoint and no new dependency** — and the fx-over-witchcraft
inversion becomes a non-issue, because a global is the same object no matter who
constructs it first. Details and the metric catalogue are in
[`observability.md`](./observability.md) and [ADR 0009](./decisions/0009-observability-metrics.md);
a Grafana dashboard ships in [`deploy/grafana`](../deploy/grafana/outbox-dashboard.json).

## Testing strategy

The suite is layered so the fast tests stay fast and the slow tests stay honest:

- **Hermetic unit tests** (`make test`, `-race`) run in ~1s with no Docker — in-memory
  repo/store fakes and a loopback fake signal-cli daemon. The end-to-end smoke
  (`worker/e2e_test.go`) drives a real multipart `POST` through the worker to `SENT`.
- **Integration tests** (`make integration-test`) spin a Postgres testcontainer per
  test and exercise every sqlc query through the real schema — the only way to catch
  ENUM/index/CHECK drift between the `.sql` files and the generated code.
- **Fuzzing** on the multipart admission boundary; a saved crasher seed documents a
  500→400 bug it already found.
- **An fx graph boot check** caught a duplicate-provider bug that no unit test could,
  because the e2e test constructs the worker by hand rather than through fx.

Plus the supply-chain layer in CI: CodeQL, govulncheck, dependency review, and signed
releases with SBOMs.

## Tradeoffs and what's next

Nothing here is free, and the design names its costs:

- **Polling, not push.** Simpler and broker-free, at the cost of caller poll traffic.
  Webhooks remain a clean future extension.
- **At-least-once, not exactly-once.** Bounded today; the crash-window fix is scoped in
  ADR 0011 and gated on whether duplicates are ever actually observed.
- **One recipient or group per message** ([ADR 0006](./decisions/0006-single-recipient-or-group-per-message.md))
  keeps the state machine and the schema honest; fan-out is a caller concern.
- **Fixed-interval crons.** The cleanup and timeout-reaper jobs run on a one-minute
  tick; the `schedule` config field is reserved for a real cron-expression parser.

If I kept investing, the next steps in order would be: close the exactly-once crash
window (ADR 0011), publish a throughput characterization off the existing benchmarks
and metrics, and add fault-injection tests that kill the worker mid-send to prove the
lease path under chaos rather than only by reasoning.

## Where to read more

- [`architecture.md`](./architecture.md) — the fx graph, request lifecycle, failure model.
- [`worker.md`](./worker.md) — claim query, lease, backoff, the timeout reaper.
- [`persistence.md`](./persistence.md) — schema, sqlc layout, Atlas migrations.
- [`observability.md`](./observability.md) — the metric catalogue and how to read it.
- [`decisions/`](./decisions) — every decision above, with the alternatives considered.
