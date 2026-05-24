package storage

import (
	"context"
	"errors" //nolint:depguard // stdlib errors.As is the canonical way to inspect minio.ErrorResponse
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/minio/minio-go/v7"
	werror "github.com/palantir/witchcraft-go-error"
	"github.com/palantir/witchcraft-go-logging/wlog/svclog/svc1log"

	"github.com/olehmushka/go-signalium/internal/config"
)

// ObjectStore is the streaming-upload + download facade over a minio.Client.
// It is the concrete that satisfies the consumer-defined Uploader/Downloader
// interfaces in service/worker — those packages import this one, not the
// other way around.
type ObjectStore struct {
	client *minio.Client
	cfg    config.MinIOConfig
}

// NewObjectStore is the fx provider; cfg is the install-time MinIO subsection.
func NewObjectStore(client *minio.Client, install config.Install) *ObjectStore {
	return &ObjectStore{client: client, cfg: install.MinIO}
}

// Bucket returns the configured bucket name.
func (s *ObjectStore) Bucket() string { return s.cfg.Bucket }

// LocalTmpDir returns the configured tmp directory used for worker downloads.
func (s *ObjectStore) LocalTmpDir() string { return s.cfg.LocalTmpDir }

// EnsureBucket creates the configured bucket if it does not already exist. A
// pre-existing bucket owned by this account is treated as success (mirrors the
// Node entrypoint behaviour).
func (s *ObjectStore) EnsureBucket(ctx context.Context) error {
	if err := s.client.MakeBucket(ctx, s.cfg.Bucket, minio.MakeBucketOptions{Region: s.cfg.Region}); err != nil {
		if isAlreadyOwned(err) {
			return nil
		}
		return werror.WrapWithContextParams(ctx, err, "make bucket",
			werror.SafeParam("bucket", s.cfg.Bucket))
	}
	return nil
}

// EnsureLocalTmpDir creates the configured tmp directory with 0o700.
func (s *ObjectStore) EnsureLocalTmpDir() error {
	if s.cfg.LocalTmpDir == "" {
		return nil
	}
	if err := os.MkdirAll(s.cfg.LocalTmpDir, 0o700); err != nil {
		return werror.Wrap(err, "mkdir tmp",
			werror.SafeParam("dir", s.cfg.LocalTmpDir))
	}
	return nil
}

// Put streams body into the configured bucket at the given key. size may be
// -1 if the size is unknown — minio-go switches to its multipart code path
// and streams in chunks. Returns the canonical {bucket, key} reference the
// row stores.
func (s *ObjectStore) Put(ctx context.Context, key string, body io.Reader, size int64, contentType string) (bucket, storedKey string, err error) {
	opts := minio.PutObjectOptions{ContentType: contentType}
	info, err := s.client.PutObject(ctx, s.cfg.Bucket, key, body, size, opts)
	if err != nil {
		return "", "", werror.WrapWithContextParams(ctx, err, "minio put object",
			werror.SafeParam("bucket", s.cfg.Bucket),
			werror.SafeParam("key", key))
	}
	return s.cfg.Bucket, info.Key, nil
}

// Remove deletes a single object best-effort. Used for rollback when the
// multipart handler fails mid-stream.
func (s *ObjectStore) Remove(ctx context.Context, bucket, key string) error {
	if err := s.client.RemoveObject(ctx, bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return werror.WrapWithContextParams(ctx, err, "minio remove object",
			werror.SafeParam("bucket", bucket),
			werror.SafeParam("key", key))
	}
	return nil
}

// DownloadAll fetches every object under <messageID>/ into <tmpDir>/<messageID>/
// and returns the absolute local paths in the order the input refs were given.
// The caller passes in the explicit refs (from the row) so the worker stays
// authoritative over which attachments belong to a message.
func (s *ObjectStore) DownloadAll(ctx context.Context, messageID string, refs []ObjectRef) ([]string, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	dir := filepath.Join(s.cfg.LocalTmpDir, messageID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, werror.WrapWithContextParams(ctx, err, "mkdir attachments tmp",
			werror.SafeParam("dir", dir))
	}
	paths := make([]string, 0, len(refs))
	logger := svc1log.FromContext(ctx)
	for _, ref := range refs {
		path, err := s.downloadOne(ctx, dir, ref)
		if err != nil {
			// Best-effort cleanup of anything we already wrote — the worker
			// will retry the whole message on the next tick.
			s.cleanupBestEffort(dir, paths, logger)
			return nil, err
		}
		paths = append(paths, path)
	}
	return paths, nil
}

func (s *ObjectStore) downloadOne(ctx context.Context, dir string, ref ObjectRef) (string, error) {
	obj, err := s.client.GetObject(ctx, ref.Bucket, ref.Key, minio.GetObjectOptions{})
	if err != nil {
		return "", werror.WrapWithContextParams(ctx, err, "minio get object",
			werror.SafeParam("bucket", ref.Bucket),
			werror.SafeParam("key", ref.Key))
	}
	defer func() { _ = obj.Close() }()

	dst := filepath.Join(dir, filepath.Base(ref.Key))
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", werror.WrapWithContextParams(ctx, err, "open attachment dst",
			werror.SafeParam("dst", dst))
	}
	if _, err := io.Copy(f, obj); err != nil {
		_ = f.Close()
		return "", werror.WrapWithContextParams(ctx, err, "copy attachment to disk",
			werror.SafeParam("dst", dst))
	}
	if err := f.Close(); err != nil {
		return "", werror.WrapWithContextParams(ctx, err, "close attachment dst",
			werror.SafeParam("dst", dst))
	}
	return dst, nil
}

// CleanupLocal removes the per-message tmp directory. Best-effort; logs and
// swallows errors.
func (s *ObjectStore) CleanupLocal(ctx context.Context, messageID string) {
	if s.cfg.LocalTmpDir == "" || messageID == "" {
		return
	}
	dir := filepath.Join(s.cfg.LocalTmpDir, messageID)
	if err := os.RemoveAll(dir); err != nil {
		svc1log.FromContext(ctx).Warn("cleanup tmp failed",
			svc1log.SafeParam("dir", dir),
			svc1log.Stacktrace(err))
	}
}

func (s *ObjectStore) cleanupBestEffort(dir string, paths []string, logger svc1log.Logger) {
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			logger.Warn("rollback attachment file failed",
				svc1log.SafeParam("path", p),
				svc1log.Stacktrace(err))
		}
	}
	if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
		// Directory may still hold non-attachment files; not a fatal issue.
		logger.Debug("rollback attachment dir not empty",
			svc1log.SafeParam("dir", dir),
			svc1log.Stacktrace(err))
	}
}

// ObjectRef is the {bucket, key} pair persisted on a signal-message row. It is
// intentionally simpler than domain.Attachment (no mime/size) — the worker
// only needs the addressing information to download.
type ObjectRef struct {
	Bucket string
	Key    string
}

func isAlreadyOwned(err error) bool {
	var resp minio.ErrorResponse
	if errors.As(err, &resp) {
		switch resp.Code {
		case "BucketAlreadyOwnedByYou", "BucketAlreadyExists":
			return true
		}
		if resp.StatusCode == http.StatusConflict {
			return true
		}
	}
	return false
}
