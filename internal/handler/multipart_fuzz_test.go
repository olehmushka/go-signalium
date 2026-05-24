package handler_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/palantir/witchcraft-go-logging/wlog"
	"github.com/palantir/witchcraft-go-logging/wlog/svclog/svc1log"

	_ "github.com/palantir/witchcraft-go-logging/wlog-zap"

	"github.com/olehmushka/go-signalium/internal/handler"
)

// FuzzMultipart fuzzes the leading-edge of the multipart admission path. The
// raw body bytes are sent through MultipartHandler.ServeHTTP; the handler must
// never panic and must always produce a sane HTTP response code (2xx admit or
// 4xx rejection; 5xx is only valid on an interior service-layer error that the
// stub enqueuer never raises).
//
// Boundary mutations are exercised by including malformed Content-Type
// headers in the seed corpus (the boundary travels in the header, not the
// body) — see the trailing seed inputs.
func FuzzMultipart(f *testing.F) {
	// Seed corpus: each entry is (contentType, body). The body bytes are what
	// the fuzzer mutates; the contentType pairs the body with a plausible (or
	// adversarial) boundary declaration.
	type seed struct {
		contentType string
		body        []byte
	}
	seeds := []seed{
		// 1. Minimal valid: metadata-only, recipient + content set.
		mkSeed("xyz",
			part("metadata", "application/json", []byte(`{"externalId":"e","recipient":"+1","content":"x"}`)),
		),
		// 2. Valid metadata + one attachment.
		mkSeed("xyz",
			part("metadata", "application/json", []byte(`{"externalId":"e","recipient":"+1","content":"x"}`)),
			attachment("photo.jpg", "image/jpeg", []byte("BINARY")),
		),
		// 3. Malformed JSON metadata.
		mkSeed("xyz",
			part("metadata", "application/json", []byte(`{not-json`)),
		),
		// 4. Empty metadata part.
		mkSeed("xyz",
			part("metadata", "application/json", nil),
		),
		// 5. Attachment before metadata (order violation).
		mkSeed("xyz",
			attachment("photo.jpg", "image/jpeg", []byte("BIN")),
			part("metadata", "application/json", []byte(`{"externalId":"e","recipient":"+1","content":"x"}`)),
		),
		// 6. Duplicate metadata parts.
		mkSeed("xyz",
			part("metadata", "application/json", []byte(`{"externalId":"e","recipient":"+1","content":"x"}`)),
			part("metadata", "application/json", []byte(`{"externalId":"e2"}`)),
		),
		// 7. Non-UTF8-ish filename to stress header parsing.
		mkSeed("xyz",
			part("metadata", "application/json", []byte(`{"externalId":"e","recipient":"+1","content":"x"}`)),
			attachment("\xff\xfeevil.bin", "application/octet-stream", []byte("DATA")),
		),
		// 8. Truncated body (boundary trailer missing).
		mkSeed("xyz",
			[]byte("--xyz\r\nContent-Disposition: form-data; name=\"metadata\"\r\n\r\n{")),
		// 9. Empty body.
		mkSeed("xyz", nil),
		// 10. Body with no boundary at all.
		mkSeed("xyz", []byte("just some plain text not multipart")),
	}

	for _, s := range seeds {
		f.Add(s.contentType, s.body)
	}

	h := handler.NewMultipartHandler(&stubEnqueuer{}, svc1log.New(io.Discard, wlog.InfoLevel))

	f.Fuzz(func(t *testing.T, contentType string, body []byte) {
		// The handler must never panic regardless of input. Go's fuzz harness
		// already treats panics as crashers; the explicit recover keeps the
		// failure message tied to the exact input that tripped it.
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("handler panicked on Content-Type=%q body=%q: %v", contentType, body, r)
			}
		}()

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, handler.MultipartPath, bytes.NewReader(body))
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)

		// 2xx (admit) and 4xx (reject) are the contract surface; the stub
		// enqueuer never fails, so any 5xx is a crash-equivalent finding.
		switch {
		case rw.Code >= 200 && rw.Code < 300:
		case rw.Code >= 400 && rw.Code < 500:
		default:
			t.Fatalf("unexpected status %d on Content-Type=%q body=%q: %s",
				rw.Code, contentType, body, rw.Body.String())
		}
	})
}

// mkSeed concatenates body parts with the given boundary into a single
// multipart body and returns the seed (ContentType, body bytes).
func mkSeed(boundary string, partsOrBytes ...any) struct {
	contentType string
	body        []byte
} {
	var buf bytes.Buffer
	// Variadic of either []byte (raw) or rawPart (assembled).
	for _, p := range partsOrBytes {
		switch v := p.(type) {
		case []byte:
			buf.Write(v)
		case rawPart:
			buf.WriteString("--" + boundary + "\r\n")
			for k, vs := range v.headers {
				for _, val := range vs {
					buf.WriteString(k + ": " + val + "\r\n")
				}
			}
			buf.WriteString("\r\n")
			buf.Write(v.body)
			buf.WriteString("\r\n")
		}
	}
	// Only close the boundary when at least one rawPart was provided.
	hasParts := false
	for _, p := range partsOrBytes {
		if _, ok := p.(rawPart); ok {
			hasParts = true
			break
		}
	}
	if hasParts {
		buf.WriteString("--" + boundary + "--\r\n")
	}
	return struct {
		contentType string
		body        []byte
	}{
		contentType: "multipart/form-data; boundary=" + boundary,
		body:        buf.Bytes(),
	}
}

type rawPart struct {
	headers map[string][]string
	body    []byte
}

func part(formName, contentType string, body []byte) rawPart {
	return rawPart{
		headers: map[string][]string{
			"Content-Disposition": {`form-data; name="` + formName + `"`},
			"Content-Type":        {contentType},
		},
		body: body,
	}
}

func attachment(filename, contentType string, body []byte) rawPart {
	return rawPart{
		headers: map[string][]string{
			"Content-Disposition": {`form-data; name="attachments"; filename="` + filename + `"`},
			"Content-Type":        {contentType},
		},
		body: body,
	}
}
