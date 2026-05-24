package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/palantir/witchcraft-go-logging/wlog"
	"github.com/palantir/witchcraft-go-logging/wlog/svclog/svc1log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	// register zap backend so svc1log.New returns a real logger.
	_ "github.com/palantir/witchcraft-go-logging/wlog-zap"

	"github.com/olehmushka/go-signalium/internal/handler"
	"github.com/olehmushka/go-signalium/internal/service"
)

type stubEnqueuer struct {
	mu       sync.Mutex
	gotMeta  service.CreateMessage
	gotParts []recordedPart
	id       uuid.UUID
	err      error
}

type recordedPart struct {
	filename    string
	contentType string
	body        []byte
}

func (s *stubEnqueuer) Enqueue(ctx context.Context, m service.CreateMessage, parts service.AttachmentParts) (service.EnqueueResult, error) {
	s.mu.Lock()
	s.gotMeta = m
	s.mu.Unlock()
	if parts != nil {
		for {
			p, err := parts.Next(ctx)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return service.EnqueueResult{}, err
			}
			b, _ := io.ReadAll(p.Body)
			s.mu.Lock()
			s.gotParts = append(s.gotParts, recordedPart{filename: p.Filename, contentType: p.ContentType, body: b})
			s.mu.Unlock()
		}
	}
	if s.err != nil {
		return service.EnqueueResult{}, s.err
	}
	if s.id == uuid.Nil {
		s.id = uuid.New()
	}
	return service.EnqueueResult{ID: s.id}, nil
}

func TestMultipartHandler_HappyPath(t *testing.T) {
	t.Parallel()

	stub := &stubEnqueuer{}
	h := handler.NewMultipartHandler(stub, svc1log.New(io.Discard, wlog.InfoLevel))

	body, ctype := buildMultipart(t, []byte(`{"externalId":"ext-1","recipient":"+380","content":"hello"}`), []recordedPart{
		{filename: "photo.jpg", contentType: "image/jpeg", body: []byte("BINARY")},
	})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, handler.MultipartPath, body)
	req.Header.Set("Content-Type", ctype)
	rw := httptest.NewRecorder()

	h.ServeHTTP(rw, req)

	require.Equal(t, http.StatusAccepted, rw.Code)
	var resp struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(rw.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp.ID)
	assert.Equal(t, "ext-1", stub.gotMeta.ExternalID)
	require.Len(t, stub.gotParts, 1)
	assert.Equal(t, "photo.jpg", stub.gotParts[0].filename)
	assert.Equal(t, "image/jpeg", stub.gotParts[0].contentType)
	assert.Equal(t, []byte("BINARY"), stub.gotParts[0].body)
}

func TestMultipartHandler_RejectsNonPOST(t *testing.T) {
	t.Parallel()
	stub := &stubEnqueuer{}
	h := handler.NewMultipartHandler(stub, svc1log.New(io.Discard, wlog.InfoLevel))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, handler.MultipartPath, nil)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	assert.Equal(t, http.StatusBadRequest, rw.Code) // INVALID_ARGUMENT
}

func TestMultipartHandler_MetadataMustBeFirst(t *testing.T) {
	t.Parallel()
	stub := &stubEnqueuer{}
	h := handler.NewMultipartHandler(stub, svc1log.New(io.Discard, wlog.InfoLevel))

	// Build a multipart with an attachment BEFORE the metadata part.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	att, err := mw.CreateFormFile("attachments", "photo.jpg")
	require.NoError(t, err)
	_, _ = att.Write([]byte("BIN"))
	meta, err := mw.CreatePart(textHeader("metadata", "application/json"))
	require.NoError(t, err)
	_, _ = meta.Write([]byte(`{"externalId":"ext-1","recipient":"+380","content":"hi"}`))
	require.NoError(t, mw.Close())

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, handler.MultipartPath, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rw := httptest.NewRecorder()

	h.ServeHTTP(rw, req)

	assert.Equal(t, http.StatusBadRequest, rw.Code)
	assert.Contains(t, rw.Body.String(), "metadata")
}

func TestMultipartHandler_MalformedMetadataJSON(t *testing.T) {
	t.Parallel()
	stub := &stubEnqueuer{}
	h := handler.NewMultipartHandler(stub, svc1log.New(io.Discard, wlog.InfoLevel))

	body, ctype := buildMultipart(t, []byte(`not-json`), nil)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, handler.MultipartPath, body)
	req.Header.Set("Content-Type", ctype)
	rw := httptest.NewRecorder()

	h.ServeHTTP(rw, req)

	assert.Equal(t, http.StatusBadRequest, rw.Code)
}

func buildMultipart(tb testing.TB, meta []byte, parts []recordedPart) (io.Reader, string) {
	tb.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	metaPart, err := mw.CreatePart(textHeader("metadata", "application/json"))
	require.NoError(tb, err)
	_, _ = metaPart.Write(meta)

	for _, p := range parts {
		att, err := mw.CreatePart(map[string][]string{
			"Content-Disposition": {`form-data; name="attachments"; filename="` + p.filename + `"`},
			"Content-Type":        {p.contentType},
		})
		require.NoError(tb, err)
		_, _ = att.Write(p.body)
	}
	require.NoError(tb, mw.Close())
	return &buf, mw.FormDataContentType()
}

func textHeader(formName, contentType string) map[string][]string {
	return map[string][]string{
		"Content-Disposition": {`form-data; name="` + formName + `"`},
		"Content-Type":        {contentType},
	}
}

// strings import kept linkable so the file compiles on Go versions that strip
// unused imports more aggressively; the header builder may grow uses for it.
var _ = strings.ToLower
