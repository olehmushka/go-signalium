package signal_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/olehmushka/go-signalium/internal/config"
	"github.com/olehmushka/go-signalium/internal/signal"
	"github.com/olehmushka/go-signalium/internal/signal/testfake"
)

func newClient(t *testing.T, fake *testfake.Daemon, ignoreResults bool) *signal.TCPClient {
	t.Helper()
	host, port := fake.HostPort()
	cli := signal.NewTCPClient(config.Install{
		SignalCli: config.SignalCliConfig{
			TCP: config.SignalCliTCP{
				Host:              host,
				Port:              port,
				WaitResultTimeout: 2 * time.Second,
				IgnoreResults:     ignoreResults,
			},
		},
	}, testLogger())
	cli.Start(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = cli.Close(ctx)
	})
	return cli
}

func TestTCPClient_SendReturnsTimestamp(t *testing.T) {
	t.Parallel()

	fake := testfake.New(t, testfake.SuccessResponder())
	cli := newClient(t, fake, false)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	res, err := cli.Send(ctx, map[string]any{"account": "+380000000000", "message": "hi", "recipient": []string{"+380111111111"}})
	require.NoError(t, err)
	assert.Equal(t, "1700000001", res.ResultID())

	reqs := fake.Requests()
	require.Len(t, reqs, 1)
	assert.Equal(t, "send", reqs[0].Method)
	assert.NotEmpty(t, reqs[0].ID)
}

func TestTCPClient_DemuxesByID(t *testing.T) {
	t.Parallel()

	// Echo the timestamp value supplied in params so a parallel-Send test can
	// verify that the response is routed to the right caller. We piggy-back
	// the "message" field as a unique tag.
	fake := testfake.New(t, func(req testfake.IncomingRequest, out testfake.FrameWriter) {
		var p struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(req.Params, &p)
		out.WriteResult(req.ID, map[string]any{"timestamp": int64(len(p.Message))})
	})
	cli := newClient(t, fake, false)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	type out struct {
		want int64
		got  int64
		err  error
	}
	results := make(chan out, 20)
	for i := 1; i <= 20; i++ {
		go func(i int) {
			msg := make([]byte, i)
			for j := range msg {
				msg[j] = 'x'
			}
			res, err := cli.Send(ctx, map[string]any{"account": "+380000000000", "message": string(msg), "recipient": []string{"+380111111111"}})
			results <- out{want: int64(i), got: res.Timestamp, err: err}
		}(i)
	}
	for i := 0; i < 20; i++ {
		got := <-results
		require.NoError(t, got.err)
		assert.Equal(t, got.want, got.got, "response routed to wrong caller")
	}
}

func TestTCPClient_DaemonError(t *testing.T) {
	t.Parallel()

	fake := testfake.New(t, testfake.ErrorResponder(-32000, "boom"))
	cli := newClient(t, fake, false)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := cli.Send(ctx, map[string]any{"account": "+380000000000", "message": "hi", "recipient": []string{"+380111111111"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "signal-cli error")
}

func TestTCPClient_IgnoreResultsFireAndForget(t *testing.T) {
	t.Parallel()

	// Responder that NEVER replies. With ignoreResults=true the client must
	// still return success after writing the frame.
	fake := testfake.New(t, func(_ testfake.IncomingRequest, _ testfake.FrameWriter) {})
	cli := newClient(t, fake, true)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := cli.Send(ctx, map[string]any{"account": "+380000000000", "message": "hi", "recipient": []string{"+380111111111"}})
	require.NoError(t, err)
}
