package service_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/palantir/witchcraft-go-logging/wlog"
	"github.com/palantir/witchcraft-go-logging/wlog/svclog/svc1log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	// register zap backend so svc1log.New returns a real logger.
	_ "github.com/palantir/witchcraft-go-logging/wlog-zap"

	"github.com/olehmushka/go-signalium/internal/config"
	"github.com/olehmushka/go-signalium/internal/domain"
	"github.com/olehmushka/go-signalium/internal/service"
)

func TestSlackNotifier_DisabledByConfig(t *testing.T) {
	t.Parallel()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	t.Cleanup(srv.Close)

	n := service.NewSlackNotifier(
		config.Runtime{Slack: config.SlackConfig{
			Enabled:    false, // master switch off
			WebhookURL: srv.URL,
			NotifyOn:   config.SlackNotifyOn{PermanentFailure: true},
		}},
		svc1log.New(io.Discard, wlog.InfoLevel),
	)
	assert.False(t, n.Enabled())

	n.NotifyPermanentFailure(t.Context(), domain.SignalMessage{ID: uuid.New(), ExternalID: "x", CreatedAt: time.Now()}, errors.New("boom"))

	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, int32(0), atomic.LoadInt32(&hits), "should not have hit webhook when disabled")
}

func TestSlackNotifier_NotifyOnToggle(t *testing.T) {
	t.Parallel()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	n := service.NewSlackNotifier(
		config.Runtime{Slack: config.SlackConfig{
			Enabled:    true,
			WebhookURL: srv.URL,
			NotifyOn: config.SlackNotifyOn{
				PermanentFailure: false, // per-event toggle off
			},
		}},
		svc1log.New(io.Discard, wlog.InfoLevel),
	)

	// Per-event toggle off → no hit.
	n.NotifyPermanentFailure(t.Context(), domain.SignalMessage{ID: uuid.New(), ExternalID: "x", CreatedAt: time.Now()}, errors.New("boom"))
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, int32(0), atomic.LoadInt32(&hits))
}

func TestSlackNotifier_PostsPermanentFailurePayload(t *testing.T) {
	t.Parallel()
	type capture struct {
		Blocks []map[string]any `json:"blocks"`
	}
	var (
		got         capture
		contentType string
		decodeErr   error
		done        = make(chan struct{}, 1)
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType = r.Header.Get("Content-Type")
		decodeErr = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
		select {
		case done <- struct{}{}:
		default:
		}
	}))
	t.Cleanup(srv.Close)

	n := service.NewSlackNotifier(
		config.Runtime{Slack: config.SlackConfig{
			Enabled:    true,
			WebhookURL: srv.URL,
			NotifyOn:   config.SlackNotifyOn{PermanentFailure: true},
		}},
		svc1log.New(io.Discard, wlog.InfoLevel),
	)
	rcpt := "+380111111111"
	n.NotifyPermanentFailure(t.Context(), domain.SignalMessage{
		ID:         uuid.New(),
		ExternalID: "ext-1",
		Recipient:  &rcpt,
		Attempts:   5,
		CreatedAt:  time.Now().Add(-time.Minute),
		Content:    "hello",
	}, errors.New("daemon offline"))

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("webhook never hit")
	}
	require.NoError(t, decodeErr)
	require.Equal(t, "application/json", contentType)
	require.NotEmpty(t, got.Blocks)
	// Header block has a "text" subfield with the headline.
	hdr := got.Blocks[0]
	require.Equal(t, "section", hdr["type"])
	require.NotNil(t, hdr["text"])
}
