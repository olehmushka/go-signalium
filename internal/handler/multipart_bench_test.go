package handler_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/palantir/witchcraft-go-logging/wlog"
	"github.com/palantir/witchcraft-go-logging/wlog/svclog/svc1log"
	"github.com/stretchr/testify/require"

	_ "github.com/palantir/witchcraft-go-logging/wlog-zap"

	"github.com/olehmushka/go-signalium/internal/handler"
)

// BenchmarkServeHTTP_MultipartMetadataOnly isolates the leading-metadata parse
// path that every request — even pure-text sends — traverses. The body is
// metadata-only so the cost reflects splitMetadata + JSON decode, not stream
// copy.
func BenchmarkServeHTTP_MultipartMetadataOnly(b *testing.B) {
	body, ctype := buildMultipart(b,
		[]byte(`{"externalId":"ext-bench","recipient":"+380","content":"hello"}`),
		nil,
	)
	raw, err := io.ReadAll(body)
	require.NoError(b, err)

	h := handler.NewMultipartHandler(&stubEnqueuer{}, svc1log.New(io.Discard, wlog.InfoLevel))
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		req := httptest.NewRequestWithContext(b.Context(), http.MethodPost, handler.MultipartPath, bytes.NewReader(raw))
		req.Header.Set("Content-Type", ctype)
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)
		if rw.Code != http.StatusAccepted {
			b.Fatalf("unexpected status %d: %s", rw.Code, rw.Body.String())
		}
	}
}

// BenchmarkServeHTTP_MultipartWithAttachment measures the realistic admit path
// with a single binary attachment at three representative sizes. Throughput is
// reported via b.SetBytes so the human-eye check is MB/s, not just ns/op.
func BenchmarkServeHTTP_MultipartWithAttachment(b *testing.B) {
	sizes := []struct {
		name string
		body int
	}{
		{"1KB", 1024},
		{"64KB", 64 * 1024},
		{"1MB", 1024 * 1024},
	}
	for _, sz := range sizes {
		sz := sz
		b.Run(sz.name, func(b *testing.B) {
			body, ctype := buildMultipart(b,
				[]byte(`{"externalId":"ext-bench","recipient":"+380","content":"hi"}`),
				[]recordedPart{
					{filename: "blob.bin", contentType: "application/octet-stream", body: bytes.Repeat([]byte{0xAB}, sz.body)},
				},
			)
			raw, err := io.ReadAll(body)
			require.NoError(b, err)
			h := handler.NewMultipartHandler(&stubEnqueuer{}, svc1log.New(io.Discard, wlog.InfoLevel))

			b.SetBytes(int64(len(raw)))
			b.ReportAllocs()
			b.ResetTimer()

			for b.Loop() {
				req := httptest.NewRequestWithContext(b.Context(), http.MethodPost, handler.MultipartPath, bytes.NewReader(raw))
				req.Header.Set("Content-Type", ctype)
				rw := httptest.NewRecorder()
				h.ServeHTTP(rw, req)
				if rw.Code != http.StatusAccepted {
					b.Fatalf("unexpected status %d: %s", rw.Code, rw.Body.String())
				}
			}
		})
	}
}
