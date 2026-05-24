package signal_test

import (
	"io"

	"github.com/palantir/witchcraft-go-logging/wlog"
	"github.com/palantir/witchcraft-go-logging/wlog/svclog/svc1log"

	// wlog needs a backend registered for svc1log.New to return a real logger.
	// Tests use a discard sink so they don't pollute -v output.
	_ "github.com/palantir/witchcraft-go-logging/wlog-zap"
)

func testLogger() svc1log.Logger {
	return svc1log.New(io.Discard, wlog.InfoLevel, svc1log.Origin("test"))
}
