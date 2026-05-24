# 0005 — fx owns `main`, witchcraft is a fx-managed component

## Status
Accepted, with documented contingency.

## Context
Two frameworks both want to own program lifecycle:

- **uber-go/fx** — the user's chosen DI container. fx.New(...).Run() blocks `main`, installs signal handlers, drives `fx.Lifecycle.OnStart` / `OnStop` for each provided component.
- **palantir/witchcraft-go-server** — Palantir's HTTP framework. `witchcraft.NewServer().Start()` is designed to block `main`, install its own signal handlers, load config, run an `InitFunc` after config but before traffic, and shut down gracefully on SIGTERM.

Naive integration produces double signal handling and unclear lifecycle ownership.

## Decision
fx owns `main` and process lifecycle. Witchcraft is constructed inside an fx provider and started/stopped via fx.Lifecycle hooks. Witchcraft's signal handling is disabled with `WithDisableSigQuitHandler()` so fx is the sole owner of SIGINT/SIGTERM.

Sketch:

```go
// internal/app/server/server.go
func NewWitchcraftServer(handlers Handlers, installPath InstallConfigPath) (*witchcraft.Server, error) {
    return witchcraft.NewServer().
        WithInstallConfigType(config.Install{}).
        WithRuntimeConfigType(config.Runtime{}).
        WithInstallConfigFromFile(string(installPath)).
        WithDisableSigQuitHandler().
        WithInitFunc(func(ctx context.Context, info witchcraft.InitInfo) (func(), error) {
            if err := signalapi.RegisterRoutesSignalMessagesService(info.Router, handlers.SignalMessages); err != nil {
                return nil, werror.Wrap(err, "register routes")
            }
            if err := info.Router.Post("/api/v1/signal-messages", handlers.MultipartUpload); err != nil {
                return nil, werror.Wrap(err, "register multipart route")
            }
            return func() { /* cleanup */ }, nil
        }), nil
}

// internal/app/server/lifecycle.go
func RegisterLifecycle(lc fx.Lifecycle, srv *witchcraft.Server, sh fx.Shutdowner) {
    errCh := make(chan error, 1)
    lc.Append(fx.Hook{
        OnStart: func(_ context.Context) error {
            go func() {
                if err := srv.Start(); err != nil {
                    errCh <- err
                    _ = sh.Shutdown(fx.ExitCode(1))
                }
            }()
            return nil
        },
        OnStop: func(ctx context.Context) error { return srv.Shutdown(ctx) },
    })
}
```

## Consequences
- **Single signal owner.** fx catches SIGINT/SIGTERM and drives OnStop → witchcraft graceful shutdown.
- **Two loggers exist concurrently.** Boot-time logs (before witchcraft starts) use a fx-provided `svc1log.Logger`. Request-scoped logs use `svc1log.FromContext(ctx)` populated by witchcraft middleware. They are different instances; do not try to share state.
- **Metrics registry plumbing is awkward.** Witchcraft constructs the registry inside `InitInfo`. If a fx-graph service needs the registry, the `InitFunc` closes over a holder (struct with mutex, or a `chan` filled once on `InitFunc` entry) so fx-graph services can pull the registry after the server has started. Acceptable but ugly.
- **Refreshable runtime config** is constructed independently at the fx layer from the same `var/conf/runtime.yml` witchcraft is reading. Yes, a duplicate parse; the alternative (exposing witchcraft's refreshable into fx) is worse.

## Contingency
If `WithDisableSigQuitHandler` proves insufficient to fully suppress witchcraft's own signal handling (we have not yet observed this in this project, but the API surface has historically been fluid), invert: witchcraft owns `main`, and a small fx container is constructed synchronously inside `InitFunc`:

```go
witchcraft.NewServer().WithInitFunc(func(ctx context.Context, info witchcraft.InitInfo) (func(), error) {
    app := fx.New( /* providers + an Invoke that registers routes via info.Router */ )
    if err := app.Start(ctx); err != nil { return nil, err }
    return func() { _ = app.Stop(context.Background()) }, nil
}).Start()
```

This loses fx's top-level signal handling but is otherwise functional. Documented here so the next session knows the escape hatch without re-deriving it.

## Alternatives considered
- **Don't use fx at all.** Witchcraft has its own "extensions" pattern. The user explicitly chose fx; not relitigated.
- **Don't use witchcraft, use community stack** (chi + zap + ...) — see [0004](./0004-witchcraft-full-framework.md).
