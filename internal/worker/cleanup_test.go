package worker_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
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

func TestCleanup_RemovesStaleFiles(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()

	// Two files: one old, one fresh.
	oldFile := filepath.Join(tmp, "msg-old", "photo.jpg")
	require.NoError(t, os.MkdirAll(filepath.Dir(oldFile), 0o700))
	require.NoError(t, os.WriteFile(oldFile, []byte("old"), 0o600))
	require.NoError(t, os.Chtimes(oldFile, time.Now().Add(-2*time.Hour), time.Now().Add(-2*time.Hour)))

	freshFile := filepath.Join(tmp, "msg-fresh", "photo.jpg")
	require.NoError(t, os.MkdirAll(filepath.Dir(freshFile), 0o700))
	require.NoError(t, os.WriteFile(freshFile, []byte("fresh"), 0o600))

	c := worker.NewCleanup(
		config.Install{Cron: config.CronConfig{CleanupOldFiles: config.CleanupOldFilesConfig{
			Enabled:     true,
			Directories: []string{tmp},
			FileTTL:     10 * time.Minute,
		}}},
		svc1log.New(io.Discard, wlog.InfoLevel),
	)
	require.True(t, c.Enabled())

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	c.Start(ctx)
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
		defer stopCancel()
		_ = c.Stop(stopCtx)
	})

	require.Eventually(t, func() bool {
		_, err := os.Stat(oldFile)
		return os.IsNotExist(err)
	}, 2*time.Second, 20*time.Millisecond, "old file should be removed")

	// Fresh file must survive.
	_, err := os.Stat(freshFile)
	assert.NoError(t, err)
}

func TestCleanup_DisabledIsNoOp(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	c := worker.NewCleanup(
		config.Install{Cron: config.CronConfig{CleanupOldFiles: config.CleanupOldFilesConfig{
			Enabled:     false,
			Directories: []string{tmp},
			FileTTL:     time.Minute,
		}}},
		svc1log.New(io.Discard, wlog.InfoLevel),
	)
	assert.False(t, c.Enabled())
	c.Start(t.Context())

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer stopCancel()
	assert.NoError(t, c.Stop(stopCtx))
}
