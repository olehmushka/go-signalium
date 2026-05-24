package worker

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	werror "github.com/palantir/witchcraft-go-error"
	"github.com/palantir/witchcraft-go-logging/wlog/svclog/svc1log"
	"go.uber.org/fx"

	"github.com/olehmushka/go-signalium/internal/config"
)

// CleanupModule wires the periodic tmp-attachment sweeper. Defined as a
// separate fx.Module so the worker package can stay focused on the outbox loop;
// fx.Module just keeps the constructors discoverable.
//
// The sweeper honours `cron.cleanupOldFiles.enabled` + `directories` + `fileTtl`
// and runs on a fixed one-minute tick. The `schedule` cron expression is not yet
// honoured — it is parsed and reserved for a future cron-expression scheduler;
// the default ("0 * * * * *") already matches the fixed cadence.
var CleanupModule = fx.Module(
	"worker-cleanup",
	fx.Provide(NewCleanup),
	fx.Invoke(RegisterCleanupLifecycle),
)

// cleanupInterval is the fixed sweep cadence. Matches the "0 * * * * *"
// default in install.yml (every minute on the zero-second mark) closely
// enough that operators don't need to know which scheduler is running.
const cleanupInterval = time.Minute

// Cleanup is the tmp-attachment sweeper. One goroutine, fx-managed; the loop
// walks every configured directory and removes files older than `fileTtl`.
type Cleanup struct {
	cfg    config.CleanupOldFilesConfig
	logger svc1log.Logger
	clock  func() time.Time

	stopCancel context.CancelFunc
	wg         sync.WaitGroup
}

// NewCleanup is the fx provider.
func NewCleanup(install config.Install, logger svc1log.Logger) *Cleanup {
	return &Cleanup{
		cfg:    install.Cron.CleanupOldFiles,
		logger: logger,
		clock:  time.Now,
	}
}

// Enabled reports whether the sweeper will run.
func (c *Cleanup) Enabled() bool {
	return c.cfg.Enabled && len(c.cfg.Directories) > 0 && c.cfg.FileTTL > 0
}

// Start launches the sweeper goroutine. Returns immediately.
func (c *Cleanup) Start(ctx context.Context) {
	if !c.Enabled() {
		c.logger.Info("tmp cleanup disabled",
			svc1log.SafeParam("enabled", c.cfg.Enabled),
			svc1log.SafeParam("directories", c.cfg.Directories),
			svc1log.SafeParam("fileTtl", c.cfg.FileTTL.String()))
		return
	}
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	c.stopCancel = cancel
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.run(loopCtx)
	}()
	c.logger.Info("tmp cleanup started",
		svc1log.SafeParam("interval", cleanupInterval.String()),
		svc1log.SafeParam("fileTtl", c.cfg.FileTTL.String()),
		svc1log.SafeParam("directories", c.cfg.Directories))
}

// Stop cancels the loop and waits for the goroutine to exit.
func (c *Cleanup) Stop(ctx context.Context) error {
	if c.stopCancel == nil {
		return nil
	}
	c.stopCancel()
	done := make(chan struct{})
	go func() { c.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return werror.WrapWithContextParams(ctx, ctx.Err(), "cleanup stop: drain timeout")
	}
}

func (c *Cleanup) run(ctx context.Context) {
	t := time.NewTicker(cleanupInterval)
	defer t.Stop()
	// Run once on boot so a long-running pre-existing tmp pile doesn't wait a
	// full interval before being swept.
	c.sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.sweep(ctx)
		}
	}
}

// sweep walks each configured directory once. Errors per-file are logged and
// do not abort the rest of the walk — a single permission-denied file should
// not stop the sweeper from cleaning the rest.
func (c *Cleanup) sweep(ctx context.Context) {
	cutoff := c.clock().Add(-c.cfg.FileTTL)
	var removed int
	for _, dir := range c.cfg.Directories {
		removed += c.sweepDir(ctx, dir, cutoff)
	}
	if removed > 0 {
		c.logger.Info("tmp cleanup sweep",
			svc1log.SafeParam("removed", removed),
			svc1log.SafeParam("cutoff", cutoff.UTC().Format(time.RFC3339)))
	}
}

func (c *Cleanup) sweepDir(ctx context.Context, dir string, cutoff time.Time) int {
	var removed int
	walkErr := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			c.logger.Warn("tmp cleanup walk error",
				svc1log.SafeParam("path", path),
				svc1log.Stacktrace(err))
			return nil
		}
		if d.IsDir() || path == dir {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			c.logger.Warn("tmp cleanup stat error",
				svc1log.SafeParam("path", path),
				svc1log.Stacktrace(err))
			return nil
		}
		if !info.ModTime().Before(cutoff) {
			return nil
		}
		if err := os.Remove(path); err != nil {
			if !os.IsNotExist(err) {
				c.logger.Warn("tmp cleanup remove failed",
					svc1log.SafeParam("path", path),
					svc1log.Stacktrace(err))
			}
			return nil
		}
		removed++
		return nil
	})
	if walkErr != nil && !os.IsNotExist(walkErr) {
		c.logger.Warn("tmp cleanup walk root failed",
			svc1log.SafeParam("dir", dir),
			svc1log.Stacktrace(walkErr))
	}
	// Best-effort: prune empty per-message subdirectories so the walk root
	// stays tidy. We only remove direct children that are now empty.
	pruneEmptyChildren(dir, c.logger)
	return removed
}

// pruneEmptyChildren removes immediate empty subdirectories of root. Useful
// because the worker organises downloads as <tmpRoot>/<messageID>/<file>; once
// the files inside age out we want the wrapper directory gone too.
func pruneEmptyChildren(root string, logger svc1log.Logger) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sub := filepath.Join(root, e.Name())
		children, err := os.ReadDir(sub)
		if err != nil || len(children) > 0 {
			continue
		}
		if err := os.Remove(sub); err != nil && !os.IsNotExist(err) {
			logger.Debug("tmp cleanup prune empty dir failed",
				svc1log.SafeParam("dir", sub),
				svc1log.Stacktrace(err))
		}
	}
}

// RegisterCleanupLifecycle binds Cleanup.Start/Stop into fx.
func RegisterCleanupLifecycle(lc fx.Lifecycle, c *Cleanup) {
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			c.Start(ctx)
			return nil
		},
		OnStop: func(ctx context.Context) error {
			return c.Stop(ctx)
		},
	})
}
