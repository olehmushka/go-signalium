# 0001 — REST as the inbound trigger

## Status
Accepted.

## Context
An outbound-message service needs an inbound interface. The realistic candidates are:

- **HTTP REST.** Universally available client tooling, no broker to operate, contract lives in one schema.
- **Message broker (RabbitMQ / Kafka / NATS).** Decouples producer from consumer at the cost of running another piece of infrastructure and forcing every caller to integrate with it.
- **gRPC.** Strong typing, streaming support; less widely deployed than REST in our caller environment.

Callers in our environment are first-party services with intermittent send volume, not a high-throughput event stream. They already speak HTTP for everything else.

## Decision
go-signalium accepts outbound sends via `POST /api/v1/signal-messages`. The single source of truth for the request/response contract is the Conjure IDL at [`conjure/go-signalium-api.conjure.yml`](../../conjure/go-signalium-api.conjure.yml). No broker is part of the deployment.

## Consequences
- **Caller cost:** an HTTP client and a URL. Narrowest possible integration surface.
- **Operational cost:** one Postgres, one MinIO, one `signal-cli` daemon. No broker to monitor, patch, or rebalance.
- **Schema discipline:** the Conjure IDL is the single source of truth for request/response shapes, with the multipart bypass documented in [0008](./0008-conjure-bypass-for-multipart.md).
- **Callers learn delivery status by polling** — see [0002](./0002-polling-over-webhooks.md).
- **High-volume burst handling** sits entirely on the outbox: the REST handler is a thin acceptor that returns `202` once the row is persisted. Throughput is bounded by Postgres, not by HTTP semantics.

## Alternatives considered
- **Broker as the trigger.** Couples every caller to broker availability and operational know-how; doubles the contract surface (REST for reads + JSON-Schema for messages).
- **gRPC.** Worth revisiting if a caller appears with sustained throughput needs where HTTP/1.1 overhead is measurable. Not the case today.

## Revisit if
- A caller emerges with sustained-thousand-RPS send patterns and the per-request HTTP overhead becomes the bottleneck.
- A coordinated cross-service workflow appears for which an event bus is the right primitive — in that case, the bus sits in front of go-signalium, not in place of its REST.
