package signal_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/olehmushka/go-signalium/internal/domain"
	"github.com/olehmushka/go-signalium/internal/signal"
)

func TestBuildSendParams_Recipient(t *testing.T) {
	t.Parallel()
	rcpt := "+380111111111"
	p, err := signal.BuildSendParams("+380000000000", domain.SignalMessage{
		ExternalID: "ext-1",
		Content:    "hi",
		Recipient:  &rcpt,
	}, []string{"/tmp/a.jpg"})
	require.NoError(t, err)
	assert.Equal(t, []string{rcpt}, p.Recipient)
	assert.Empty(t, p.GroupID)
	assert.Equal(t, []string{"/tmp/a.jpg"}, p.Attachments)
}

func TestBuildSendParams_Group(t *testing.T) {
	t.Parallel()
	gid := "groupABC"
	p, err := signal.BuildSendParams("+380000000000", domain.SignalMessage{
		ExternalID: "ext-1",
		Content:    "hi",
		GroupID:    &gid,
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{gid}, p.GroupID)
	assert.Empty(t, p.Recipient)
}

func TestBuildSendParams_Quote(t *testing.T) {
	t.Parallel()
	rcpt := "+380111111111"
	qr := "1700000000"
	p, err := signal.BuildSendParams("+380000000000", domain.SignalMessage{
		ExternalID:    "ext-1",
		Content:       "hi",
		Recipient:     &rcpt,
		QuoteResultID: &qr,
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, int64(1700000000), p.QuoteTimestamp)
	assert.Equal(t, "+380000000000", p.QuoteAuthor)
}

func TestBuildSendParams_OmitsEmptyFields(t *testing.T) {
	t.Parallel()
	rcpt := "+380111111111"
	p, err := signal.BuildSendParams("+380000000000", domain.SignalMessage{
		Content:   "hi",
		Recipient: &rcpt,
	}, nil)
	require.NoError(t, err)
	b, err := json.Marshal(p)
	require.NoError(t, err)
	s := string(b)
	assert.NotContains(t, s, "groupId")
	assert.NotContains(t, s, "quoteTimestamp")
	assert.NotContains(t, s, "quoteAuthor")
	assert.NotContains(t, s, "attachments")
}

func TestBuildSendParams_Errors(t *testing.T) {
	t.Parallel()
	_, err := signal.BuildSendParams("", domain.SignalMessage{Content: "hi"}, nil)
	require.Error(t, err)
	_, err = signal.BuildSendParams("+380000000000", domain.SignalMessage{}, nil)
	require.Error(t, err) // empty content
	rcpt := "+380"
	_, err = signal.BuildSendParams("+380000000000", domain.SignalMessage{Content: "hi", Recipient: &rcpt, QuoteResultID: ptr("not-an-int")}, nil)
	require.Error(t, err)
}

func ptr(s string) *string { return &s }
