// Command go-signalium boots the outbound Signal delivery service.
//
// M5 milestone: every Conjure-modelled endpoint now has a real body. The
// operational read/update/resend handlers run against the existing repo,
// stats fold the new SQL aggregates, GetInstanceInfo serves the install
// config, and the groups proxy goes out over the signal-cli HTTP/JSON-RPC
// daemon. fx still wires the same modules as M4 — only handler bodies and
// the signal HTTP client are new.
package main

import (
	"context"
	"os"

	"github.com/palantir/witchcraft-go-logging/wlog"
	wlogzap "github.com/palantir/witchcraft-go-logging/wlog-zap"
	"github.com/palantir/witchcraft-go-logging/wlog/svclog/svc1log"
	"go.uber.org/fx"

	"github.com/olehmushka/go-signalium/internal/config"
	"github.com/olehmushka/go-signalium/internal/db"
	"github.com/olehmushka/go-signalium/internal/handler"
	appmetrics "github.com/olehmushka/go-signalium/internal/metrics"
	"github.com/olehmushka/go-signalium/internal/repo"
	"github.com/olehmushka/go-signalium/internal/server"
	"github.com/olehmushka/go-signalium/internal/service"
	"github.com/olehmushka/go-signalium/internal/signal"
	"github.com/olehmushka/go-signalium/internal/storage"
	"github.com/olehmushka/go-signalium/internal/worker"
)

const packageName = "github.com/olehmushka/go-signalium"

func main() {
	wlog.SetDefaultLoggerProvider(wlogzap.LoggerProvider())

	fx.New(
		fx.Provide(newBootLogger),
		fx.Supply(
			fx.Annotate("var/conf/install.yml", fx.ResultTags(`name:"installConfigPath"`)),
			fx.Annotate("var/conf/runtime.yml", fx.ResultTags(`name:"runtimeConfigPath"`)),
		),
		config.Module,
		appmetrics.Module,
		db.Module,
		repo.Module,
		storage.Module,
		signal.Module,
		service.Module,
		handler.Module,
		worker.Module,
		worker.CleanupModule,
		worker.TimeoutReaperModule,
		server.Module,
		fx.Invoke(registerHello),
	).Run()
}

func newBootLogger() svc1log.Logger {
	return svc1log.New(os.Stdout, wlog.InfoLevel, svc1log.Origin(packageName))
}

func registerHello(lc fx.Lifecycle, logger svc1log.Logger) {
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			logger.Info(
				"starting app",
				svc1log.SafeParam("module", packageName),
			)
			return nil
		},
		OnStop: func(_ context.Context) error {
			logger.Info("shutting down app")
			return nil
		},
	})
}
