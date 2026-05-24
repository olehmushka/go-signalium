package worker_test

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/palantir/witchcraft-go-logging/wlog"
	"github.com/palantir/witchcraft-go-logging/wlog/svclog/svc1log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	// register zap backend so svc1log.New returns a real logger.
	_ "github.com/palantir/witchcraft-go-logging/wlog-zap"

	"github.com/olehmushka/go-signalium/internal/config"
	"github.com/olehmushka/go-signalium/internal/worker"
)

// fakeReaperRepo records calls so the test can assert the reaper swept.
type fakeReaperRepo struct {
	mu        sync.Mutex
	sweeps    int
	timedOut  int64
	statCalls int
}

func (f *fakeReaperRepo) MarkTimedOut(context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sweeps++
	return f.timedOut, nil
}

func (f *fakeReaperRepo) BacklogStats(context.Context) (int, time.Duration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statCalls++
	return 3, 42 * time.Second, nil
}

func (f *fakeReaperRepo) counts() (sweeps, stats int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sweeps, f.statCalls
}

func TestTimeoutReaper_SweepsAndSamplesOnBoot(t *testing.T) {
	t.Parallel()
	repo := &fakeReaperRepo{timedOut: 2}
	r := worker.NewTimeoutReaper(
		repo,
		testMetrics(),
		config.Install{Cron: config.CronConfig{TimeoutReaper: config.TimeoutReaperConfig{Enabled: true}}},
		svc1log.New(io.Discard, wlog.InfoLevel),
	)
	require.True(t, r.Enabled())

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	r.Start(ctx)
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
		defer stopCancel()
		_ = r.Stop(stopCtx)
	})

	// The boot run fires immediately (before the first tick), so both the sweep
	// and the backlog sample should land without waiting a full interval.
	require.Eventually(t, func() bool {
		sweeps, stats := repo.counts()
		return sweeps >= 1 && stats >= 1
	}, 2*time.Second, 10*time.Millisecond, "reaper should sweep and sample on boot")
}

func TestTimeoutReaper_DisabledIsNoOp(t *testing.T) {
	t.Parallel()
	repo := &fakeReaperRepo{}
	r := worker.NewTimeoutReaper(
		repo,
		testMetrics(),
		config.Install{Cron: config.CronConfig{TimeoutReaper: config.TimeoutReaperConfig{Enabled: false}}},
		svc1log.New(io.Discard, wlog.InfoLevel),
	)
	assert.False(t, r.Enabled())
	r.Start(t.Context())

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer stopCancel()
	require.NoError(t, r.Stop(stopCtx))

	sweeps, stats := repo.counts()
	assert.Zero(t, sweeps, "disabled reaper must not sweep")
	assert.Zero(t, stats, "disabled reaper must not sample")
}
