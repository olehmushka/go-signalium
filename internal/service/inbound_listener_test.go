package service_test

import (
	"context"
	"encoding/json"
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
	"github.com/olehmushka/go-signalium/internal/repo"
	"github.com/olehmushka/go-signalium/internal/service"
	"github.com/olehmushka/go-signalium/internal/signal"
)

type stubSource struct {
	ch chan signal.Event
}

func newStubSource() *stubSource                  { return &stubSource{ch: make(chan signal.Event, 8)} }
func (s *stubSource) Events() <-chan signal.Event { return s.ch }
func (s *stubSource) send(ev signal.Event)        { s.ch <- ev }

type stubWriter struct {
	mu      sync.Mutex
	rows    []repo.InsertInboundParams
	seenKey map[string]struct{}
}

func newStubWriter() *stubWriter {
	return &stubWriter{seenKey: map[string]struct{}{}}
}

func (w *stubWriter) Insert(_ context.Context, p repo.InsertInboundParams) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	key := p.Source + "|" + intToStr(p.SourceTimestamp)
	if _, dup := w.seenKey[key]; dup {
		return false, nil
	}
	w.seenKey[key] = struct{}{}
	w.rows = append(w.rows, p)
	return true, nil
}

func (w *stubWriter) snapshot() []repo.InsertInboundParams {
	w.mu.Lock()
	defer w.mu.Unlock()
	cp := make([]repo.InsertInboundParams, len(w.rows))
	copy(cp, w.rows)
	return cp
}

func intToStr(n int64) string {
	return time.Unix(0, n).Format(time.RFC3339Nano)
}

func newListener(t *testing.T, src service.InboundEventSource, writer service.InboundWriter, ignore config.InboundIgnoreConfig) *service.InboundListener {
	t.Helper()
	return service.NewInboundListener(
		src,
		writer,
		config.Install{SignalCli: config.SignalCliConfig{EnableListening: true}},
		config.Runtime{Inbound: config.InboundConfig{Ignore: ignore}},
		svc1log.New(io.Discard, wlog.InfoLevel),
	)
}

func TestInboundListener_PersistsDataMessage(t *testing.T) {
	t.Parallel()
	src := newStubSource()
	w := newStubWriter()
	l := newListener(t, src, w, config.InboundIgnoreConfig{})

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	l.Start(ctx)
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
		defer stopCancel()
		_ = l.Stop(stopCtx)
	})

	src.send(signal.Event{
		Method: "receive",
		Params: json.RawMessage(`{"envelope":{"source":"+380111111111","sourceUuid":"abc","timestamp":17000000,"dataMessage":{"message":"hi","groupInfo":{"groupId":"G1"},"attachments":[{"id":"att1","contentType":"image/jpeg","size":42}]}},"account":"+380000000000"}`),
	})

	require.Eventually(t, func() bool { return len(w.snapshot()) == 1 }, 2*time.Second, 10*time.Millisecond)

	got := w.snapshot()[0]
	assert.Equal(t, "+380111111111", got.Source)
	require.NotNil(t, got.SourceUUID)
	assert.Equal(t, "abc", *got.SourceUUID)
	assert.Equal(t, int64(17000000), got.SourceTimestamp)
	require.NotNil(t, got.GroupID)
	assert.Equal(t, "G1", *got.GroupID)
	require.NotNil(t, got.Content)
	assert.Equal(t, "hi", *got.Content)

	var atts []map[string]any
	require.NoError(t, json.Unmarshal(got.Attachments, &atts))
	require.Len(t, atts, 1)
	assert.Equal(t, "att1", atts[0]["id"])
}

func TestInboundListener_FiltersReceiptAndTyping(t *testing.T) {
	t.Parallel()
	src := newStubSource()
	w := newStubWriter()
	l := newListener(t, src, w, config.InboundIgnoreConfig{Receipt: true, Typing: true})

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	l.Start(ctx)
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
		defer stopCancel()
		_ = l.Stop(stopCtx)
	})

	src.send(signal.Event{
		Method: "receive",
		Params: json.RawMessage(`{"envelope":{"source":"+1","timestamp":1,"receiptMessage":{}}}`),
	})
	src.send(signal.Event{
		Method: "receive",
		Params: json.RawMessage(`{"envelope":{"source":"+2","timestamp":2,"typingMessage":{}}}`),
	})
	// One non-ignored event so we can wait for *something* to land — if filtering
	// is broken the assertion below catches the extra rows.
	src.send(signal.Event{
		Method: "receive",
		Params: json.RawMessage(`{"envelope":{"source":"+3","timestamp":3,"dataMessage":{"message":"keep"}}}`),
	})

	require.Eventually(t, func() bool { return len(w.snapshot()) == 1 }, 2*time.Second, 10*time.Millisecond)
	got := w.snapshot()
	require.Len(t, got, 1)
	assert.Equal(t, "+3", got[0].Source)
}

func TestInboundListener_DropsNonReceiveMethods(t *testing.T) {
	t.Parallel()
	src := newStubSource()
	w := newStubWriter()
	l := newListener(t, src, w, config.InboundIgnoreConfig{})

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	l.Start(ctx)
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
		defer stopCancel()
		_ = l.Stop(stopCtx)
	})

	src.send(signal.Event{Method: "syncResponse", Params: json.RawMessage(`{}`)})
	src.send(signal.Event{Method: "receive", Params: json.RawMessage(`{"envelope":{"source":"+9","timestamp":9,"dataMessage":{"message":"ok"}}}`)})

	require.Eventually(t, func() bool { return len(w.snapshot()) == 1 }, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, "+9", w.snapshot()[0].Source)
}

func TestInboundListener_DisabledIsNoOp(t *testing.T) {
	t.Parallel()
	src := newStubSource()
	w := newStubWriter()
	l := service.NewInboundListener(
		src,
		w,
		config.Install{SignalCli: config.SignalCliConfig{EnableListening: false}},
		config.Runtime{},
		svc1log.New(io.Discard, wlog.InfoLevel),
	)
	assert.False(t, l.Enabled())

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	l.Start(ctx)

	src.send(signal.Event{Method: "receive", Params: json.RawMessage(`{"envelope":{"source":"+1","timestamp":1,"dataMessage":{"message":"x"}}}`)})

	// Listener should never consume; the buffered channel still has the event.
	time.Sleep(50 * time.Millisecond)
	assert.Empty(t, w.snapshot())

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer stopCancel()
	assert.NoError(t, l.Stop(stopCtx))
}
