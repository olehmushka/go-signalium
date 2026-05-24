// End-to-end smoke for the M4 send pipeline: multipart POST →
// SendMessageService inserts row → outbox worker claims row → fake daemon
// replies → worker stamps SENT.
//
// In-process by design: a fake signal-cli daemon over loopback TCP, an
// in-memory repo, and an in-memory uploader/downloader. Real Postgres + MinIO
// land in M7's testcontainers suite.

package worker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	pkgmetrics "github.com/palantir/pkg/metrics"
	"github.com/palantir/witchcraft-go-logging/wlog"
	"github.com/palantir/witchcraft-go-logging/wlog/svclog/svc1log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	// register zap backend so svc1log.New returns a real logger.
	_ "github.com/palantir/witchcraft-go-logging/wlog-zap"

	"github.com/olehmushka/go-signalium/internal/config"
	"github.com/olehmushka/go-signalium/internal/domain"
	"github.com/olehmushka/go-signalium/internal/handler"
	appmetrics "github.com/olehmushka/go-signalium/internal/metrics"
	"github.com/olehmushka/go-signalium/internal/repo"
	"github.com/olehmushka/go-signalium/internal/service"
	"github.com/olehmushka/go-signalium/internal/signal"
	"github.com/olehmushka/go-signalium/internal/signal/testfake"
	"github.com/olehmushka/go-signalium/internal/storage"
	"github.com/olehmushka/go-signalium/internal/worker"
)

// testMetrics returns an Outbox backed by a throwaway registry so tests exercise
// the real emission path without asserting on it.
func testMetrics() *appmetrics.Outbox {
	return appmetrics.NewOutbox(pkgmetrics.NewRootMetricsRegistry())
}

func TestE2E_PostToSent(t *testing.T) {
	t.Parallel()

	logger := svc1log.New(io.Discard, wlog.InfoLevel)
	tmp := t.TempDir()

	// Fake signal-cli over loopback.
	fake := testfake.New(t, testfake.SuccessResponder())
	host, port := fake.HostPort()

	install := config.Install{
		MinIO: config.MinIOConfig{
			Bucket:      "signalium-attachments",
			LocalTmpDir: tmp,
		},
		SignalCli: config.SignalCliConfig{
			TCP: config.SignalCliTCP{
				Host:              host,
				Port:              port,
				WaitResultTimeout: 2 * time.Second,
			},
			SenderPhoneNumber: "+380000000000",
		},
		Worker: config.WorkerConfig{
			PollInterval:      20 * time.Millisecond,
			LeaseDuration:     5 * time.Second,
			PerAttemptTimeout: 5 * time.Second,
			Concurrency:       1,
			BaseBackoff:       100 * time.Millisecond,
			MaxBackoff:        1 * time.Second,
		},
	}

	// In-memory persistence + object store.
	mem := newMemRepo()
	mstore := newMemStore(tmp)

	// Real TCP client against the fake daemon.
	tcp := signal.NewTCPClient(install, logger, testMetrics())
	tcp.Start(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = tcp.Close(ctx)
	})

	// Send service and worker driven by the same in-memory backends.
	sendSvc := service.NewSendMessageService(mem, mstore, install, logger)
	wkr := worker.NewWorker(mem, mstore, tcp, noopNotifier{}, testMetrics(), install, logger)
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	t.Cleanup(cancelWorker)
	go wkr.Run(workerCtx)

	mh := handler.NewMultipartHandler(sendSvc, logger)

	// POST a multipart request with one attachment.
	body, ctype := buildMultipart(
		t,
		[]byte(`{"externalId":"e2e-1","recipient":"+380111111111","content":"hello world"}`),
		[]e2ePart{{filename: "photo.jpg", contentType: "image/jpeg", body: []byte("BINARY")}},
	)
	req := httptest.NewRequestWithContext(t.Context(), "POST", handler.MultipartPath, body)
	req.Header.Set("Content-Type", ctype)
	rw := httptest.NewRecorder()
	mh.ServeHTTP(rw, req)
	require.Equal(t, 202, rw.Code, "body=%s", rw.Body.String())

	var accepted struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(rw.Body.Bytes(), &accepted))
	id, err := uuid.Parse(accepted.ID)
	require.NoError(t, err)

	// Wait for the worker to drive the row to SENT.
	deadline := time.Now().Add(3 * time.Second)
	var final domain.SignalMessage
	for time.Now().Before(deadline) {
		final, err = mem.getByID(id)
		require.NoError(t, err)
		if final.Status == domain.StatusSent {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	assert.Equal(t, domain.StatusSent, final.Status, "row did not reach SENT")
	assert.NotNil(t, final.ResultID)
	if final.ResultID != nil {
		assert.NotEmpty(t, *final.ResultID)
	}

	// Confirm signal-cli actually saw one "send" with the right shape.
	reqs := fake.Requests()
	require.Len(t, reqs, 1)
	assert.Equal(t, "send", reqs[0].Method)
}

// --- failure notifier stub -----------------------------------------------

type noopNotifier struct{}

func (noopNotifier) Enabled() bool                                                             { return false }
func (noopNotifier) NotifyPermanentFailure(_ context.Context, _ domain.SignalMessage, _ error) {}

// --- in-memory repo ------------------------------------------------------

type memRepo struct {
	mu   sync.Mutex
	rows map[uuid.UUID]*domain.SignalMessage
}

func newMemRepo() *memRepo { return &memRepo{rows: map[uuid.UUID]*domain.SignalMessage{}} }

func (m *memRepo) GetByIdempotencyKey(_ context.Context, key string) (domain.SignalMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.IdempotencyKey != nil && *r.IdempotencyKey == key {
			return *r, nil
		}
	}
	return domain.SignalMessage{}, domain.ErrNotFound
}

func (m *memRepo) Insert(_ context.Context, p repo.InsertParams) (domain.SignalMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := uuid.New()
	now := time.Now().UTC()
	row := &domain.SignalMessage{
		ID:                id,
		ExternalID:        p.ExternalID,
		IdempotencyKey:    p.IdempotencyKey,
		Recipient:         p.Recipient,
		GroupID:           p.GroupID,
		SenderPhoneNumber: p.SenderPhoneNumber,
		Content:           p.Content,
		Attachments:       p.Attachments,
		Attempts:          0,
		MaxAttempts:       int(p.MaxAttempts),
		NextAttemptAt:     now,
		TimeoutAt:         p.TimeoutAt,
		Status:            domain.StatusPending,
		CorrelationID:     p.CorrelationID,
		CreatedAt:         now,
		ModifiedAt:        now,
	}
	m.rows[id] = row
	return *row, nil
}

func (m *memRepo) ClaimPending(_ context.Context, lease time.Duration) (domain.SignalMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	for _, r := range m.rows {
		eligible := r.Status == domain.StatusPending || (r.Status == domain.StatusSending && r.NextAttemptAt.Before(now))
		// Mirror the SQL claim: an overdue row (timeout_at <= now) is invisible to
		// the worker; the timeout reaper terminalises it instead.
		if r.TimeoutAt != nil && !r.TimeoutAt.After(now) {
			continue
		}
		if eligible && !r.NextAttemptAt.After(now) {
			r.Status = domain.StatusSending
			r.Attempts++
			r.NextAttemptAt = now.Add(lease)
			r.ModifiedAt = now
			return *r, nil
		}
	}
	return domain.SignalMessage{}, domain.ErrNotFound
}

func (m *memRepo) MarkSent(_ context.Context, id uuid.UUID, resultID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	if !ok {
		return errors.New("mem repo: id not found")
	}
	r.Status = domain.StatusSent
	rid := resultID
	r.ResultID = &rid
	r.ModifiedAt = time.Now().UTC()
	return nil
}

func (m *memRepo) MarkFailed(_ context.Context, id uuid.UUID, lastErr string, nextAttemptAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	if !ok {
		return errors.New("mem repo: id not found")
	}
	if r.Attempts >= r.MaxAttempts {
		r.Status = domain.StatusPermanentFailed
	} else {
		r.Status = domain.StatusFailed
	}
	le := lastErr
	r.LastError = &le
	r.NextAttemptAt = nextAttemptAt
	r.ModifiedAt = time.Now().UTC()
	return nil
}

func (m *memRepo) getByID(id uuid.UUID) (domain.SignalMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	if !ok {
		return domain.SignalMessage{}, domain.ErrNotFound
	}
	return *r, nil
}

// --- in-memory object store ---------------------------------------------

type memStore struct {
	tmpDir string

	mu      sync.Mutex
	objects map[string][]byte // key="<bucket>/<key>"
}

func newMemStore(tmpDir string) *memStore {
	return &memStore{tmpDir: tmpDir, objects: map[string][]byte{}}
}

func (s *memStore) Put(_ context.Context, key string, body io.Reader, _ int64, _ string) (string, string, error) {
	b, err := io.ReadAll(body)
	if err != nil {
		return "", "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket := "test-bucket"
	s.objects[bucket+"/"+key] = b
	return bucket, key, nil
}

func (s *memStore) Remove(_ context.Context, bucket, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, bucket+"/"+key)
	return nil
}

func (s *memStore) DownloadAll(_ context.Context, messageID string, refs []storage.ObjectRef) ([]string, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	dir := filepath.Join(s.tmpDir, messageID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(refs))
	for _, ref := range refs {
		s.mu.Lock()
		b, ok := s.objects[ref.Bucket+"/"+ref.Key]
		s.mu.Unlock()
		if !ok {
			return nil, errors.New("mem store: object not found: " + ref.Bucket + "/" + ref.Key)
		}
		dst := filepath.Join(dir, filepath.Base(ref.Key))
		if err := os.WriteFile(dst, b, 0o600); err != nil {
			return nil, err
		}
		paths = append(paths, dst)
	}
	return paths, nil
}

func (s *memStore) CleanupLocal(_ context.Context, messageID string) {
	_ = os.RemoveAll(filepath.Join(s.tmpDir, messageID))
}

// --- multipart helpers ---------------------------------------------------

type e2ePart struct {
	filename    string
	contentType string
	body        []byte
}

func buildMultipart(t *testing.T, meta []byte, parts []e2ePart) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	metaPart, err := mw.CreatePart(map[string][]string{
		"Content-Disposition": {`form-data; name="metadata"`},
		"Content-Type":        {"application/json"},
	})
	require.NoError(t, err)
	_, _ = metaPart.Write(meta)

	for _, p := range parts {
		att, err := mw.CreatePart(map[string][]string{
			"Content-Disposition": {`form-data; name="attachments"; filename="` + p.filename + `"`},
			"Content-Type":        {p.contentType},
		})
		require.NoError(t, err)
		_, _ = att.Write(p.body)
	}
	require.NoError(t, mw.Close())
	return &buf, mw.FormDataContentType()
}
