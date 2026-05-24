package signal

import (
	"strconv"

	werror "github.com/palantir/witchcraft-go-error"

	"github.com/olehmushka/go-signalium/internal/domain"
)

// SendParams is the JSON-RPC "send" params shape produced for signal-cli.
// Optional fields are omitted from the JSON when zero so signal-cli does not
// see `"groupId":null`, which it rejects.
type SendParams struct {
	Account        string   `json:"account"`
	Message        string   `json:"message"`
	Attachments    []string `json:"attachments,omitempty"`
	Recipient      []string `json:"recipient,omitempty"`
	GroupID        []string `json:"groupId,omitempty"`
	QuoteTimestamp int64    `json:"quoteTimestamp,omitempty"`
	QuoteAuthor    string   `json:"quoteAuthor,omitempty"`
}

// BuildSendParams converts a domain message plus the local attachment paths
// (already downloaded onto the daemon's disk) into the JSON-RPC payload. The
// daemon's account stays in install.yml's senderPhoneNumber; the message row
// records sender_phone_number for audit but we always send as the configured
// account (a one-process-one-sender invariant — see docs/config.md).
func BuildSendParams(account string, msg domain.SignalMessage, paths []string) (SendParams, error) {
	if account == "" {
		return SendParams{}, werror.Error("send params: empty account")
	}
	if msg.Content == "" {
		return SendParams{}, werror.Error("send params: empty content")
	}

	p := SendParams{
		Account:     account,
		Message:     msg.Content,
		Attachments: paths,
	}
	switch {
	case msg.Recipient != nil && *msg.Recipient != "":
		p.Recipient = []string{*msg.Recipient}
	case msg.GroupID != nil && *msg.GroupID != "":
		p.GroupID = []string{*msg.GroupID}
	default:
		return SendParams{}, werror.Error("send params: neither recipient nor groupId set",
			werror.SafeParam("externalId", msg.ExternalID))
	}
	if msg.QuoteResultID != nil && *msg.QuoteResultID != "" {
		ts, err := strconv.ParseInt(*msg.QuoteResultID, 10, 64)
		if err != nil {
			return SendParams{}, werror.Wrap(err, "send params: parse quoteResultId",
				werror.SafeParam("quoteResultId", *msg.QuoteResultID))
		}
		p.QuoteTimestamp = ts
		p.QuoteAuthor = account
	}
	return p, nil
}
