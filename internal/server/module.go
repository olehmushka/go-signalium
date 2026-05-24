// Package server provides the fx-managed witchcraft server. fx owns process
// lifecycle (SIGINT/SIGTERM) and the witchcraft server is started in a
// goroutine via fx.Lifecycle.OnStart, stopped via OnStop. See
// docs/decisions/0005-fx-wrapping-witchcraft.md for the inversion rationale
// and the contingency if WithDisableSigQuitHandler proves insufficient.
//
// /status/{liveness,readiness} are served by witchcraft's built-in management
// endpoints — no custom handler needed for M3. Custom probes (pgx ping, MinIO
// head, signal-cli TCP) attach via WithHealth / WithReadiness in later
// milestones once those clients exist.
package server

import (
	"context"

	werror "github.com/palantir/witchcraft-go-error"
	"github.com/palantir/witchcraft-go-logging/wlog/svclog/svc1log"
	"github.com/palantir/witchcraft-go-server/v3/witchcraft"
	"go.uber.org/fx"

	"github.com/olehmushka/go-signalium/internal/config"
	signalapi "github.com/olehmushka/go-signalium/internal/generated/signalium/api"
	"github.com/olehmushka/go-signalium/internal/handler"
)

// Module wires the witchcraft server. The server is constructed in
// NewWitchcraftServer (an fx provider), and RegisterLifecycle binds its
// Start/Shutdown to fx OnStart/OnStop.
var Module = fx.Module(
	"server",
	fx.Provide(NewWitchcraftServer),
	fx.Invoke(RegisterLifecycle),
)

// Paths captures the on-disk locations of the install + runtime YAML files.
// They are provided by main.go (or test code) so the production defaults
// stay out of this package.
type Paths struct {
	fx.In

	Install string `name:"installConfigPath"`
	Runtime string `name:"runtimeConfigPath"`
}

// NewWitchcraftServer builds a *witchcraft.Server[config.Install, config.Runtime]
// with disabled signal handling (fx owns signals) and an InitFunc that
// registers the conjure-generated routes for SignalMessagesService plus the
// raw multipart upload handler (POST /api/v1/signal-messages) that lives
// outside Conjure.
func NewWitchcraftServer(
	paths Paths,
	signalMessages signalapi.SignalMessagesService,
	multipart *handler.MultipartHandler,
) *witchcraft.Server[config.Install, config.Runtime] {
	return witchcraft.NewServer[config.Install, config.Runtime]().
		WithInstallConfigFromFile(paths.Install).
		WithRuntimeConfigFromFile(paths.Runtime).
		WithSelfSignedCertificate().
		WithDisableSigQuitHandler().
		WithDisableShutdownSignalHandler().
		WithInitFunc(func(_ context.Context, info witchcraft.InitInfo[config.Install, config.Runtime]) (func(), error) {
			// RegisterRoutesSignalMessagesService is the conjure-generated registrar;
			// its signature is fixed by the generator and does not accept ctx.
			if err := signalapi.RegisterRoutesSignalMessagesService(info.Router, signalMessages); err != nil { //nolint:contextcheck // generated registrar has no ctx param
				return nil, werror.Wrap(err, "register SignalMessagesService routes")
			}
			if err := info.Router.Post(handler.MultipartPath, multipart); err != nil {
				return nil, werror.Wrap(err, "register multipart upload route")
			}
			return nil, nil
		})
}

// RegisterLifecycle wires the witchcraft server's Start/Shutdown into fx.
// fx OnStart launches Start() in a goroutine; a Start() error triggers
// fx.Shutdowner.Shutdown(fx.ExitCode(1)) so process exit codes reflect a
// server-startup failure. OnStop invokes srv.Shutdown(ctx) for graceful
// drain.
func RegisterLifecycle(
	lc fx.Lifecycle,
	srv *witchcraft.Server[config.Install, config.Runtime],
	sh fx.Shutdowner,
	logger svc1log.Logger,
) {
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			go func() {
				if err := srv.Start(); err != nil {
					logger.Error("witchcraft server exited", svc1log.Stacktrace(err))
					_ = sh.Shutdown(fx.ExitCode(1))
				}
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			return srv.Shutdown(ctx)
		},
	})
}
