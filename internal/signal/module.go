// Package signal contains the two clients that bridge go-signalium and the
// signal-cli Java daemon: a persistent TCP/JSON-RPC client (Send + event
// stream) and an HTTP client (read-only proxies). docs/signal-cli.md describes
// the protocol surface and reconnect semantics.
package signal

import (
	"context"

	"github.com/palantir/witchcraft-go-logging/wlog/svclog/svc1log"
	"go.uber.org/fx"

	"github.com/olehmushka/go-signalium/internal/config"
)

// Module wires the TCP client (send) and the HTTP client (read-only proxies
// like groups listing — M5). The TCP client connects on OnStart so a failed
// startup is visible immediately, and is closed on OnStop. The HTTP client is
// stateless: it builds a fresh request per call, so no lifecycle hook needed.
var Module = fx.Module(
	"signal",
	fx.Provide(
		NewTCPClient,
		NewHTTPClient,
	),
	fx.Invoke(func(lc fx.Lifecycle, c *TCPClient, logger svc1log.Logger, cfg config.Install) {
		lc.Append(fx.Hook{
			OnStart: func(ctx context.Context) error {
				c.Start(ctx)
				logger.Info("signal-cli tcp client started",
					svc1log.SafeParam("host", cfg.SignalCli.TCP.Host),
					svc1log.SafeParam("port", cfg.SignalCli.TCP.Port))
				return nil
			},
			OnStop: func(ctx context.Context) error {
				return c.Close(ctx)
			},
		})
	}),
)
