// Package-doc lives in send_message.go.

package service

import (
	"context"
	"encoding/json"
	"sync"

	werror "github.com/palantir/witchcraft-go-error"
	"github.com/palantir/witchcraft-go-logging/wlog/svclog/svc1log"

	"github.com/olehmushka/go-signalium/internal/config"
	"github.com/olehmushka/go-signalium/internal/repo"
	"github.com/olehmushka/go-signalium/internal/signal"
)

// InboundEventSource is the small slice of signal.TCPClient the listener
// consumes. Tests inject an in-memory channel directly.
type InboundEventSource interface {
	Events() <-chan signal.Event
}

// InboundWriter is the persistence slice the listener calls.
type InboundWriter interface {
	Insert(ctx context.Context, p repo.InsertInboundParams) (bool, error)
}

// InboundListener consumes asynchronous signal-cli events, applies the
// configured ignore filter, and persists the survivors. It owns one goroutine
// started by fx; OnStop cancels its context and waits for the goroutine to
// drain in-flight inserts.
type InboundListener struct {
	src     InboundEventSource
	writer  InboundWriter
	enabled bool
	ignore  config.InboundIgnoreConfig
	logger  svc1log.Logger

	wg     sync.WaitGroup
	cancel context.CancelFunc
}

// NewInboundListener is the fx provider. Wiring is gated on the install +
// runtime configs:
//   - `signalCli.enableListening` is the install-time toggle that turns the
//     listener on at all — when false the goroutine is not started.
//   - `inbound.ignore.*` is the per-event-type filter (refreshable in the
//     wider design; for M6 we snapshot at boot and reload on restart).
func NewInboundListener(
	src InboundEventSource,
	writer InboundWriter,
	install config.Install,
	runtime config.Runtime,
	logger svc1log.Logger,
) *InboundListener {
	return &InboundListener{
		src:     src,
		writer:  writer,
		enabled: install.SignalCli.EnableListening,
		ignore:  runtime.Inbound.Ignore,
		logger:  logger,
	}
}

// Enabled returns whether the listener will run when Start is called.
func (l *InboundListener) Enabled() bool { return l.enabled }

// Start launches the consumer goroutine. It returns immediately; the loop
// runs until ctx is cancelled.
func (l *InboundListener) Start(ctx context.Context) {
	if !l.enabled {
		l.logger.Info("inbound listener disabled (signalCli.enableListening=false)")
		return
	}
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	l.cancel = cancel
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		l.run(loopCtx)
	}()
}

// Stop cancels the loop and waits up to ctx's deadline for the goroutine to
// exit. Returns ctx.Err() if the drain times out.
func (l *InboundListener) Stop(ctx context.Context) error {
	if !l.enabled {
		return nil
	}
	if l.cancel != nil {
		l.cancel()
	}
	done := make(chan struct{})
	go func() { l.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return werror.WrapWithContextParams(ctx, ctx.Err(), "inbound listener stop: drain timeout")
	}
}

func (l *InboundListener) run(ctx context.Context) {
	events := l.src.Events()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			l.handle(ctx, ev)
		}
	}
}

func (l *InboundListener) handle(ctx context.Context, ev signal.Event) {
	// signal-cli emits one wrapper method per envelope: `receive` carries
	// dataMessage / editMessage / receiptMessage / syncMessage / typingMessage
	// / callMessage variants inside params.envelope. Anything else is metadata
	// noise we don't capture.
	if ev.Method != "receive" {
		return
	}
	env, raw, ok := parseEnvelope(ev.Params)
	if !ok {
		l.logger.Warn("inbound: malformed envelope, dropping",
			svc1log.SafeParam("method", ev.Method),
			svc1log.SafeParam("paramsBytes", len(ev.Params)))
		return
	}
	if l.filtered(env) {
		return
	}
	if env.Source == "" || env.Timestamp == 0 {
		// We need both columns for the UNIQUE key; drop noise that lacks them.
		l.logger.Debug("inbound: envelope missing source/timestamp, dropping",
			svc1log.SafeParam("hasSource", env.Source != ""),
			svc1log.SafeParam("timestamp", env.Timestamp))
		return
	}
	attachmentsJSON, err := json.Marshal(attachmentsOf(env))
	if err != nil {
		l.logger.Warn("inbound: marshal attachments", svc1log.Stacktrace(err))
		return
	}
	inserted, err := l.writer.Insert(ctx, repo.InsertInboundParams{
		Source:          env.Source,
		SourceUUID:      env.SourceUUID,
		SourceTimestamp: env.Timestamp,
		GroupID:         groupIDOf(env),
		Content:         contentOf(env),
		Attachments:     attachmentsJSON,
		Raw:             raw,
	})
	if err != nil {
		l.logger.Warn("inbound: insert failed",
			svc1log.SafeParam("source", env.Source),
			svc1log.SafeParam("timestamp", env.Timestamp),
			svc1log.Stacktrace(err))
		return
	}
	if !inserted {
		l.logger.Debug("inbound: duplicate redelivery skipped",
			svc1log.SafeParam("source", env.Source),
			svc1log.SafeParam("timestamp", env.Timestamp))
		return
	}
	l.logger.Debug("inbound: persisted",
		svc1log.SafeParam("source", env.Source),
		svc1log.SafeParam("timestamp", env.Timestamp))
}

func (l *InboundListener) filtered(env inboundEnvelope) bool {
	if l.ignore.Typing && env.Typing != nil {
		return true
	}
	if l.ignore.Receipt && env.Receipt != nil {
		return true
	}
	if l.ignore.SyncMessage && env.Sync != nil {
		return true
	}
	if l.ignore.CallMessage && env.Call != nil {
		return true
	}
	return false
}

// inboundEnvelope is the subset of fields signal-cli publishes in
// params.envelope on a receive event. Variants are nullable pointers so the
// filter can detect "this envelope is X" by presence alone.
type inboundEnvelope struct {
	Source     string  `json:"source"`
	SourceUUID *string `json:"sourceUuid"`
	Timestamp  int64   `json:"timestamp"`

	Data    *envData    `json:"dataMessage,omitempty"`
	Edit    *envEdit    `json:"editMessage,omitempty"`
	Receipt *envReceipt `json:"receiptMessage,omitempty"`
	Sync    *envSync    `json:"syncMessage,omitempty"`
	Typing  *envTyping  `json:"typingMessage,omitempty"`
	Call    *envCall    `json:"callMessage,omitempty"`
}

type envData struct {
	Message     *string         `json:"message"`
	Attachments []envAttachment `json:"attachments,omitempty"`
	GroupInfo   *envGroupInfo   `json:"groupInfo,omitempty"`
}

type envEdit struct {
	DataMessage *envData `json:"dataMessage,omitempty"`
}

type (
	envReceipt struct{}
	envTyping  struct{}
	envCall    struct{}
)

type envSync struct {
	SentMessage *envSyncSent `json:"sentMessage,omitempty"`
}

type envSyncSent struct {
	Message     *string         `json:"message"`
	Attachments []envAttachment `json:"attachments,omitempty"`
	GroupInfo   *envGroupInfo   `json:"groupInfo,omitempty"`
}

type envGroupInfo struct {
	GroupID string `json:"groupId"`
}

type envAttachment struct {
	ID          string `json:"id"`
	ContentType string `json:"contentType"`
	Size        int64  `json:"size,omitempty"`
	Filename    string `json:"filename,omitempty"`
}

// envelopeWrapper matches signal-cli's `receive` notification shape:
// {"envelope": {...}, "account": "..."}.
type envelopeWrapper struct {
	Envelope json.RawMessage `json:"envelope"`
}

// parseEnvelope returns the typed envelope, the raw envelope JSON for the
// forensic `raw` column, and ok=false if the bytes don't match the expected
// shape.
func parseEnvelope(params json.RawMessage) (inboundEnvelope, []byte, bool) {
	var wrap envelopeWrapper
	if err := json.Unmarshal(params, &wrap); err != nil || len(wrap.Envelope) == 0 {
		return inboundEnvelope{}, nil, false
	}
	var env inboundEnvelope
	if err := json.Unmarshal(wrap.Envelope, &env); err != nil {
		return inboundEnvelope{}, nil, false
	}
	return env, wrap.Envelope, true
}

func groupIDOf(env inboundEnvelope) *string {
	switch {
	case env.Data != nil && env.Data.GroupInfo != nil && env.Data.GroupInfo.GroupID != "":
		v := env.Data.GroupInfo.GroupID
		return &v
	case env.Edit != nil && env.Edit.DataMessage != nil && env.Edit.DataMessage.GroupInfo != nil && env.Edit.DataMessage.GroupInfo.GroupID != "":
		v := env.Edit.DataMessage.GroupInfo.GroupID
		return &v
	case env.Sync != nil && env.Sync.SentMessage != nil && env.Sync.SentMessage.GroupInfo != nil && env.Sync.SentMessage.GroupInfo.GroupID != "":
		v := env.Sync.SentMessage.GroupInfo.GroupID
		return &v
	}
	return nil
}

func contentOf(env inboundEnvelope) *string {
	switch {
	case env.Data != nil && env.Data.Message != nil && *env.Data.Message != "":
		return env.Data.Message
	case env.Edit != nil && env.Edit.DataMessage != nil && env.Edit.DataMessage.Message != nil && *env.Edit.DataMessage.Message != "":
		return env.Edit.DataMessage.Message
	case env.Sync != nil && env.Sync.SentMessage != nil && env.Sync.SentMessage.Message != nil && *env.Sync.SentMessage.Message != "":
		return env.Sync.SentMessage.Message
	}
	return nil
}

func attachmentsOf(env inboundEnvelope) []envAttachment {
	switch {
	case env.Data != nil && len(env.Data.Attachments) > 0:
		return env.Data.Attachments
	case env.Edit != nil && env.Edit.DataMessage != nil && len(env.Edit.DataMessage.Attachments) > 0:
		return env.Edit.DataMessage.Attachments
	case env.Sync != nil && env.Sync.SentMessage != nil && len(env.Sync.SentMessage.Attachments) > 0:
		return env.Sync.SentMessage.Attachments
	}
	return []envAttachment{}
}
