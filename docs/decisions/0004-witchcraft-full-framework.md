# 0004 — Adopt Palantir Witchcraft at the full-framework tier

## Status
Accepted.

## Context
Palantir publishes a layered Go stack:

1. **Style guide only** (`palantir/go-style-guide`) + community libs.
2. **Palantir libraries** (werror, wlog, refreshable, conjure-go-runtime) on top of an idiomatic server you build yourself.
3. **Witchcraft full framework** (`palantir/witchcraft-go-server` + `conjure-go` + everything above). The framework owns server bootstrap, structured logging, metrics, health checks, refreshable config wiring, and IDL-driven type/handler generation.

The project commits to Palantir-style Go end-to-end, so tier 1 (style only) was rejected upfront. The real choice was tier 2 vs tier 3.

## Decision
Use witchcraft-go-server (v3) as the HTTP server, conjure-go IDL for type/handler generation, wlog for logging, werror for errors, `pkg/refreshable` for runtime config, `pkg/metrics` for instrumentation.

## Consequences
- **Less code to write.** Health/readiness, request logging, request-scoped logger injection, metrics, panic-recovery, request ID propagation — all provided.
- **More toolchain.** `conjure-go` is a code generator with its own version-pinned binary; CI must run it and check for drift.
- **One framework-level decision to deal with**: witchcraft normally owns `main`. We invert that to put fx on top — see [0005](./0005-fx-wrapping-witchcraft.md).
- **One protocol limitation**: Conjure doesn't model multipart — see [0008](./0008-conjure-bypass-for-multipart.md).
- **OpenAPI/Swagger**: not used directly. If external consumers need OpenAPI, generate it from the Conjure IDL via conjure-to-openapi tooling.

## Alternatives considered
- **Tier 2 (Palantir libs only).** Lighter dependency surface, more boilerplate. The wins from witchcraft's built-in health/readiness/logging are real and reduce the docs surface this project has to own.
- **Tier 1 (style guide only).** Loses the Palantir error/log/metric conventions that the rest of the codebase will be written in. False economy.

## Revisit if
The conjure regeneration step proves a chronic source of friction (e.g., generated code keeps drifting in CI), in which case dropping to tier 2 with hand-written `wrouter.RouteHandler`s is a viable retreat.
