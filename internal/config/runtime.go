package config

import (
	"time"

	wconfig "github.com/palantir/witchcraft-go-server/v3/config"
)

// Runtime is the typed refreshable configuration. It satisfies
// wconfig.BaseRuntimeConfig via the embedded wconfig.Runtime and adds the
// project-specific live-reloadable blocks (worker tuning, signal-cli listening
// toggle, inbound ignore rules, Slack notifier).
type Runtime struct {
	wconfig.Runtime `yaml:",inline"`

	Worker    WorkerRuntimeConfig `yaml:"worker"`
	SignalCli SignalCliRuntime    `yaml:"signalCli"`
	Inbound   InboundConfig       `yaml:"inbound"`
	Slack     SlackConfig         `yaml:"slack"`
}

// BaseRuntimeConfig satisfies the witchcraft interface and lets generic code
// retrieve the embedded base struct without type assertions.
func (r Runtime) BaseRuntimeConfig() wconfig.Runtime { return r.Runtime }

// WorkerRuntimeConfig is the refreshable portion of `worker:`.
// docs/config.md "Refreshable boundary" enumerates which keys are live-tunable.
type WorkerRuntimeConfig struct {
	PollInterval time.Duration `yaml:"pollInterval"`
	MaxAttempts  int           `yaml:"maxAttempts"`
}

// SignalCliRuntime is the refreshable portion of `signalCli:`.
type SignalCliRuntime struct {
	EnableListening bool `yaml:"enableListening"`
}

// InboundConfig maps `inbound:` — filters applied to incoming signal-cli events.
type InboundConfig struct {
	Ignore InboundIgnoreConfig `yaml:"ignore"`
}

// InboundIgnoreConfig is the ignore-list of event types from signal-cli.
type InboundIgnoreConfig struct {
	Typing      bool `yaml:"typing"`
	Receipt     bool `yaml:"receipt"`
	SyncMessage bool `yaml:"syncMessage"`
	CallMessage bool `yaml:"callMessage"`
}

// SlackConfig configures the Slack notifier (M6). All fields are refreshable.
type SlackConfig struct {
	Enabled    bool          `yaml:"enabled"`
	WebhookURL string        `yaml:"webhookUrl"`
	ChannelIDs []string      `yaml:"channelIds"`
	NotifyOn   SlackNotifyOn `yaml:"notifyOn"`
}

// SlackNotifyOn toggles which event types trigger a Slack post.
type SlackNotifyOn struct {
	PermanentFailure bool `yaml:"permanentFailure"`
	AttachmentError  bool `yaml:"attachmentError"`
}
