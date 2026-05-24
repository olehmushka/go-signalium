// Package storage wraps MinIO (S3-compatible object storage) for the
// signal-message attachment lifecycle: streamed uploads from the multipart
// handler and prefix downloads from the outbox worker. See docs/attachments.md
// for the request lifecycle and bucket layout.
//
// The package owns no business logic — it is a thin adapter so the rest of the
// codebase never sees minio-go types. The interfaces it exposes (Uploader,
// Downloader) are owned by their consumers in internal/service and
// internal/worker.
package storage

import (
	"context"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	werror "github.com/palantir/witchcraft-go-error"
	"github.com/palantir/witchcraft-go-logging/wlog/svclog/svc1log"
	"go.uber.org/fx"

	"github.com/olehmushka/go-signalium/internal/config"
)

// Module wires the storage layer. The minio.Client is constructed eagerly; the
// configured bucket is created idempotently on OnStart so the worker and
// handler can assume it exists. The local tmp directory is created the same
// way.
var Module = fx.Module(
	"storage",
	fx.Provide(
		newMinioClient,
		NewObjectStore,
	),
	fx.Invoke(func(lc fx.Lifecycle, store *ObjectStore, logger svc1log.Logger) {
		lc.Append(fx.Hook{
			OnStart: func(ctx context.Context) error {
				if err := store.EnsureBucket(ctx); err != nil {
					return werror.WrapWithContextParams(ctx, err, "ensure attachments bucket",
						werror.SafeParam("bucket", store.Bucket()))
				}
				if err := store.EnsureLocalTmpDir(); err != nil {
					return werror.WrapWithContextParams(ctx, err, "ensure local tmp dir",
						werror.SafeParam("dir", store.LocalTmpDir()))
				}
				logger.Info("storage ready",
					svc1log.SafeParam("bucket", store.Bucket()),
					svc1log.SafeParam("endpoint", store.cfg.Endpoint))
				return nil
			},
		})
	}),
)

// newMinioClient builds the minio-go client from the install config. Bucket
// creation is deferred to ObjectStore.EnsureBucket so it runs under a context.
func newMinioClient(cfg config.MinIOConfig) (*minio.Client, error) {
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Region: cfg.Region,
		Secure: cfg.UseSSL,
	})
	if err != nil {
		return nil, werror.Wrap(err, "minio client init",
			werror.SafeParam("endpoint", cfg.Endpoint))
	}
	return client, nil
}
