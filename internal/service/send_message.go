package service

import (
	"context"
	"errors" //nolint:depguard // stdlib errors.Is for domain sentinel comparison
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	werror "github.com/palantir/witchcraft-go-error"
	"github.com/palantir/witchcraft-go-logging/wlog/svclog/svc1log"
	"go.uber.org/fx"

	"github.com/olehmushka/go-signalium/internal/config"
	"github.com/olehmushka/go-signalium/internal/domain"
	"github.com/olehmushka/go-signalium/internal/repo"
	"github.com/olehmushka/go-signalium/internal/signal"
	"github.com/olehmushka/go-signalium/internal/storage"
)

// Module wires the send-message service. Adapter providers bind the concrete
// repo / storage types to the consumer-owned MessageInserter /
// AttachmentUploader interfaces so the service constructor depends only on
// the small interfaces (which makes unit testing trivial).
var Module = fx.Module(
	"service",
	fx.Provide(
		func(r *repo.Messages) MessageInserter { return r },
		func(s *storage.ObjectStore) AttachmentUploader { return s },
		func(r *repo.Inbound) InboundWriter { return r },
		func(c *signal.TCPClient) InboundEventSource { return c },
		fx.Annotate(
			NewSendMessageService,
			fx.As(new(SendMessageEnqueuer)),
		),
		NewInboundListener,
		NewSlackNotifier,
	),
	fx.Invoke(RegisterInboundListener),
)

// RegisterInboundListener ties InboundListener.Start/Stop to fx Start/Stop.
// The Start hook is a no-op when listening is disabled in install.yml.
func RegisterInboundListener(lc fx.Lifecycle, l *InboundListener) {
	lc.Append(fx.Hook{
		OnStart: func(startCtx context.Context) error {
			l.Start(startCtx)
			return nil
		},
		OnStop: func(stopCtx context.Context) error {
			return l.Stop(stopCtx)
		},
	})
}

// SendMessageEnqueuer is the surface the multipart handler depends on. Kept
// small so handler tests can fake it without dragging in pgx/MinIO.
type SendMessageEnqueuer interface {
	Enqueue(ctx context.Context, meta CreateMessage, parts AttachmentParts) (EnqueueResult, error)
}

// CreateMessage is the validated subset of the multipart `metadata` part. The
// handler decodes the on-wire CreateSignalMessageRequest into this; this type
// is what services exchange, not the conjure-generated one.
type CreateMessage struct {
	ExternalID        string
	IdempotencyKey    *string
	Recipient         *string
	GroupID           *string
	SenderPhoneNumber *string // optional override; must match configured if set
	Content           string
	QuoteResultID     *string
	TimeoutSeconds    *int
	MaxAttempts       *int
}

// AttachmentParts is an iterator over the streamed multipart "attachments"
// parts. The send service consumes each part exactly once: it does NOT
// buffer the file contents — it pipes the reader straight into MinIO.
type AttachmentParts interface {
	// Next returns the next attachment part, or io.EOF when there are no more.
	// The caller (this service) is responsible for closing/draining the reader
	// once it has finished with it.
	Next(ctx context.Context) (AttachmentPart, error)
}

// AttachmentPart is one streamed attachment from the multipart request.
type AttachmentPart struct {
	Filename    string
	ContentType string
	Body        io.Reader
}

// EnqueueResult is what the handler returns to the client.
type EnqueueResult struct {
	ID         uuid.UUID
	Idempotent bool // true if the row already existed for the given idempotency key
}

// MessageInserter is the small slice of repo.Messages that the service needs.
type MessageInserter interface {
	GetByIdempotencyKey(ctx context.Context, key string) (domain.SignalMessage, error)
	Insert(ctx context.Context, p repo.InsertParams) (domain.SignalMessage, error)
}

// AttachmentUploader is the slice of storage.ObjectStore the service needs.
// Returns the canonical {bucket, key} reference persisted on the row.
type AttachmentUploader interface {
	Put(ctx context.Context, key string, body io.Reader, size int64, contentType string) (bucket, storedKey string, err error)
	Remove(ctx context.Context, bucket, key string) error
}

// SendMessageService validates incoming requests, streams attachments to
// object storage, and inserts the row in PENDING state. The outbox worker
// then drives the row through the state machine.
type SendMessageService struct {
	repo        MessageInserter
	store       AttachmentUploader
	senderPhone string
	logger      svc1log.Logger
}

// NewSendMessageService is the fx provider. Both repo and store are accepted
// as the small consumer-owned interfaces so tests can inject in-memory fakes
// without dragging in pgx or MinIO; the production fx graph binds the
// concrete *repo.Messages / *storage.ObjectStore via adapter providers in
// Module.
func NewSendMessageService(
	r MessageInserter,
	store AttachmentUploader,
	install config.Install,
	logger svc1log.Logger,
) *SendMessageService {
	return &SendMessageService{
		repo:        r,
		store:       store,
		senderPhone: install.SignalCli.SenderPhoneNumber,
		logger:      logger,
	}
}

// Enqueue is the handler-facing entry point. See docs/rest-api.md "Send"
// for the semantics.
func (s *SendMessageService) Enqueue(ctx context.Context, m CreateMessage, parts AttachmentParts) (EnqueueResult, error) {
	if err := s.validate(m); err != nil {
		return EnqueueResult{}, err
	}

	// Idempotency: if the key already maps to a row, short-circuit before
	// re-uploading anything. The dedup window is the lifetime of the row.
	if m.IdempotencyKey != nil && *m.IdempotencyKey != "" {
		existing, err := s.repo.GetByIdempotencyKey(ctx, *m.IdempotencyKey)
		switch {
		case err == nil:
			return EnqueueResult{ID: existing.ID, Idempotent: true}, nil
		case errors.Is(err, domain.ErrNotFound):
			// fall through to fresh insert
		default:
			return EnqueueResult{}, werror.WrapWithContextParams(ctx, err, "idempotency lookup")
		}
	}

	id := uuid.New()
	attachments, err := s.uploadAll(ctx, id, parts)
	if err != nil {
		return EnqueueResult{}, err
	}

	maxAttempts := int32(5)
	if m.MaxAttempts != nil && *m.MaxAttempts > 0 {
		maxAttempts = int32(*m.MaxAttempts)
	}
	var timeoutAt *time.Time
	if m.TimeoutSeconds != nil && *m.TimeoutSeconds > 0 {
		t := time.Now().UTC().Add(time.Duration(*m.TimeoutSeconds) * time.Second)
		timeoutAt = &t
	}

	row, err := s.repo.Insert(ctx, repo.InsertParams{
		ExternalID:        m.ExternalID,
		IdempotencyKey:    m.IdempotencyKey,
		Recipient:         m.Recipient,
		GroupID:           m.GroupID,
		SenderPhoneNumber: s.senderPhone,
		Content:           m.Content,
		Attachments:       attachments,
		MaxAttempts:       maxAttempts,
		TimeoutAt:         timeoutAt,
		CorrelationID:     correlationFromCtx(ctx, id),
	})
	if err != nil {
		// Best-effort cleanup of just-uploaded objects.
		s.rollback(ctx, attachments)
		return EnqueueResult{}, err
	}
	return EnqueueResult{ID: row.ID}, nil
}

func (s *SendMessageService) validate(m CreateMessage) error {
	if strings.TrimSpace(m.ExternalID) == "" {
		return invalid("externalId", "must not be empty")
	}
	if len(m.ExternalID) > 255 {
		return invalid("externalId", "must be ≤ 255 chars")
	}
	if strings.TrimSpace(m.Content) == "" {
		return invalid("content", "must not be empty")
	}
	rcptSet := m.Recipient != nil && *m.Recipient != ""
	groupSet := m.GroupID != nil && *m.GroupID != ""
	if rcptSet == groupSet {
		return invalid("recipient|groupId", "exactly one of recipient or groupId must be set")
	}
	if m.SenderPhoneNumber != nil && *m.SenderPhoneNumber != "" && *m.SenderPhoneNumber != s.senderPhone {
		return senderMismatch(s.senderPhone, *m.SenderPhoneNumber)
	}
	return nil
}

func (s *SendMessageService) uploadAll(ctx context.Context, id uuid.UUID, parts AttachmentParts) (domain.Attachments, error) {
	if parts == nil {
		return nil, nil
	}
	var attachments domain.Attachments
	for {
		part, err := parts.Next(ctx)
		if errors.Is(err, io.EOF) {
			return attachments, nil
		}
		if err != nil {
			s.rollback(ctx, attachments)
			return nil, attachmentUploadFailed("", err)
		}
		filename := sanitizeFilename(part.Filename)
		if filename == "" {
			s.rollback(ctx, attachments)
			return nil, invalid("attachments", "rejected attachment with empty filename")
		}
		key := id.String() + "/" + filename
		bucket, storedKey, err := s.store.Put(ctx, key, part.Body, -1, part.ContentType)
		if err != nil {
			s.rollback(ctx, attachments)
			return nil, attachmentUploadFailed(filename, err)
		}
		attachments = append(attachments, domain.Attachment{
			Bucket:   bucket,
			Filename: storedKey,
			MimeType: part.ContentType,
		})
	}
}

func (s *SendMessageService) rollback(ctx context.Context, refs domain.Attachments) {
	for _, ref := range refs {
		if err := s.store.Remove(ctx, ref.Bucket, ref.Filename); err != nil {
			s.logger.Warn("rollback attachment failed",
				svc1log.SafeParam("bucket", ref.Bucket),
				svc1log.SafeParam("key", ref.Filename),
				svc1log.Stacktrace(err))
		}
	}
}

// sanitizeFilename strips path components and replaces anything outside the
// allowed character set with '_'. Mirrors docs/attachments.md "Validation".
var filenameAllowed = regexp.MustCompile(`[^A-Za-z0-9._-]`)

func sanitizeFilename(raw string) string {
	base := filepath.Base(raw)
	if base == "" || base == "." || base == "/" {
		return ""
	}
	cleaned := filenameAllowed.ReplaceAllString(base, "_")
	if cleaned == "" || strings.HasPrefix(cleaned, ".") {
		return ""
	}
	return cleaned
}

func correlationFromCtx(ctx context.Context, id uuid.UUID) string {
	// witchcraft populates a request-id; service code reads it via wlog if
	// needed. Here we fall back to the message uuid so the row always has a
	// non-empty correlation id (NOT NULL column).
	if logger := svc1log.FromContext(ctx); logger != nil {
		_ = logger // currently no public accessor for the request id; keep slot for future wlog API
	}
	return id.String()
}
