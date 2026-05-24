package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/palantir/witchcraft-go-logging/wlog/svclog/svc1log"

	"github.com/olehmushka/go-signalium/internal/config"
	"github.com/olehmushka/go-signalium/internal/domain"
)

// SlackNotifier posts permanent-failure (and other opt-in) events to a Slack
// incoming webhook. The webhook URL, the master enable flag, and the per-event
// toggles all come from the runtime config — Slack outages must never break
// the send pipeline, so every call is best-effort: failures are logged and
// swallowed.
type SlackNotifier struct {
	cfg    config.SlackConfig
	http   slackHTTPDoer
	logger svc1log.Logger
}

// slackHTTPDoer is the minimum HTTP surface the notifier needs. Tests inject
// a stub; production binds the package-level default client.
type slackHTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// NewSlackNotifier is the fx provider. The HTTP client has a 10s timeout so a
// hung Slack endpoint can't pile up goroutines in the worker.
func NewSlackNotifier(runtime config.Runtime, logger svc1log.Logger) *SlackNotifier {
	return &SlackNotifier{
		cfg:    runtime.Slack,
		http:   &http.Client{Timeout: 10 * time.Second},
		logger: logger,
	}
}

// Enabled reports whether the notifier is on AND has a webhook URL. Callers
// check this so they can skip building the payload when nothing would happen.
func (n *SlackNotifier) Enabled() bool {
	return n.cfg.Enabled && n.cfg.WebhookURL != ""
}

// NotifyPermanentFailure posts a "permanent failure" alert for the given row
// and underlying error. The toggle `slack.notifyOn.permanentFailure` gates
// this — if disabled the call is a no-op. Errors are logged at WARN and
// never propagated to the worker.
func (n *SlackNotifier) NotifyPermanentFailure(ctx context.Context, m domain.SignalMessage, cause error) {
	if !n.Enabled() || !n.cfg.NotifyOn.PermanentFailure {
		return
	}
	payload := slackPermanentFailurePayload(m, cause)
	n.post(ctx, payload, "permanent-failure")
}

// NotifyAttachmentError posts a "remote storage error" alert when MinIO fetch
// fails during a worker dispatch. Gated by `slack.notifyOn.attachmentError`.
func (n *SlackNotifier) NotifyAttachmentError(ctx context.Context, bucket, key string, cause error) {
	if !n.Enabled() || !n.cfg.NotifyOn.AttachmentError {
		return
	}
	payload := slackAttachmentErrorPayload(bucket, key, cause)
	n.post(ctx, payload, "attachment-error")
}

func (n *SlackNotifier) post(ctx context.Context, payload slackPayload, kind string) {
	body, err := json.Marshal(payload)
	if err != nil {
		n.logger.Warn("slack: marshal payload",
			svc1log.SafeParam("kind", kind),
			svc1log.Stacktrace(err))
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.cfg.WebhookURL, bytes.NewReader(body))
	if err != nil {
		n.logger.Warn("slack: build request",
			svc1log.SafeParam("kind", kind),
			svc1log.Stacktrace(err))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.http.Do(req)
	if err != nil {
		n.logger.Warn("slack: post failed",
			svc1log.SafeParam("kind", kind),
			svc1log.Stacktrace(err))
		return
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		n.logger.Warn("slack: non-2xx response",
			svc1log.SafeParam("kind", kind),
			svc1log.SafeParam("status", resp.StatusCode))
		return
	}
}

// slackPayload is the Slack incoming-webhook envelope, with the blocks
// formatting that on-call engineers expect.
type slackPayload struct {
	Channel string       `json:"channel,omitempty"`
	Blocks  []slackBlock `json:"blocks"`
}

type slackBlock struct {
	Type   string       `json:"type"`
	Text   *slackText   `json:"text,omitempty"`
	Fields []slackField `json:"fields,omitempty"`
}

type slackText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type slackField struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func slackPermanentFailurePayload(m domain.SignalMessage, cause error) slackPayload {
	target := "unknown"
	switch {
	case m.GroupID != nil && *m.GroupID != "":
		target = "group=" + *m.GroupID
	case m.Recipient != nil && *m.Recipient != "":
		target = "recipient=" + *m.Recipient
	}
	dur := time.Since(m.CreatedAt).Truncate(time.Second).String()
	return slackPayload{
		Blocks: []slackBlock{
			{
				Type: "section",
				Text: &slackText{
					Type: "mrkdwn",
					Text: fmt.Sprintf("*Signal message permanently failed after %d attempts (%s)*", m.Attempts, dur),
				},
			},
			{
				Type: "section",
				Fields: []slackField{
					{Type: "mrkdwn", Text: "*External ID:* " + m.ExternalID},
					{Type: "mrkdwn", Text: "*Target:* " + target},
					{Type: "mrkdwn", Text: "*Created:* " + m.CreatedAt.UTC().Format(time.RFC3339)},
					{Type: "mrkdwn", Text: "*Content:* " + truncate(m.Content, 256)},
					{Type: "mrkdwn", Text: "*Error:* " + errString(cause)},
				},
			},
		},
	}
}

func slackAttachmentErrorPayload(bucket, key string, cause error) slackPayload {
	return slackPayload{
		Blocks: []slackBlock{
			{
				Type: "section",
				Text: &slackText{
					Type: "mrkdwn",
					Text: "*Failed to download attachment from object storage*",
				},
			},
			{
				Type: "section",
				Fields: []slackField{
					{Type: "mrkdwn", Text: "*Bucket:* " + bucket},
					{Type: "mrkdwn", Text: "*Key:* " + key},
					{Type: "mrkdwn", Text: "*Error:* " + errString(cause)},
				},
			},
		},
	}
}

func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Compile-time guard: SlackNotifier satisfies the worker-owned interface so a
// missing method here surfaces at build time rather than at fx wiring time.
var _ permanentFailureNotifier = (*SlackNotifier)(nil)

// permanentFailureNotifier mirrors the small interface the worker package
// owns. Duplicating the shape here lets us assert compatibility without an
// import cycle.
type permanentFailureNotifier interface {
	Enabled() bool
	NotifyPermanentFailure(ctx context.Context, m domain.SignalMessage, cause error)
}
