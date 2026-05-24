# 0002 — Polling over webhooks for terminal status

## Status
Accepted.

## Context
A send is accepted asynchronously: the REST handler returns `202` immediately and the outbox worker delivers later (see [0001](./0001-rest-as-inbound-trigger.md)). Callers therefore need a way to learn the eventual outcome. The realistic options:

1. **HTTP webhooks.** Caller registers a `callbackUrl` on send; server POSTs `sent` / `failed` events to it.
2. **Polling.** Caller hits `GET /api/v1/signal-messages/{id}` until `status` is terminal.
3. **Server-sent events / WebSockets.** Server holds a long-lived connection per caller and streams transitions.

## Decision
Polling. The server exposes `GET /api/v1/signal-messages/{id}` returning the row's current state. Callers loop on their own cadence until `status ∈ {SENT, PERMANENT_FAILED, TIMED_OUT}`.

## Consequences
- **Simplest possible caller integration** — one extra GET, no callback infrastructure to stand up, no retry-on-callback-failure semantics, no auth-on-the-receiver-side problem.
- **Slightly higher request volume against go-signalium.** Mitigated by the small read cost of `GET /{id}` (single-row primary-key fetch on an already-warm pool).
- **Callers that want push semantics build a thin polling sidecar** themselves. Explicit choice not to bake this in.
- **The status state machine becomes part of the public contract.** Adding new terminal states is a breaking change. Documented in [`persistence.md`](../persistence.md#status-state-machine).

## Alternatives considered
- **Webhooks** require building reliable POST-with-retry, signature validation on the receiver, dead-letter handling on go-signalium's side when callers' endpoints are flapping, and a story for caller-controlled retry exhaustion. Deferred complexity for unclear benefit at current scale.
- **Server-sent events / WebSocket subscription** introduce a long-lived connection model the service doesn't otherwise need.

## Revisit if
A caller appears with hard low-latency notification requirements (sub-second) and a tolerance for the operational cost of webhooks. At that point, an opt-in `callbackUrl` field on the send payload is the right addition; polling stays as the default.
