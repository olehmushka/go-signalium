package signal

import (
	"bufio"
	"context"
	"encoding/json"
	"errors" //nolint:depguard // stdlib errors.Is for io.EOF / net.ErrClosed comparison
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	werror "github.com/palantir/witchcraft-go-error"
	"github.com/palantir/witchcraft-go-logging/wlog/svclog/svc1log"

	"github.com/olehmushka/go-signalium/internal/config"
)

// Errors surfaced by the TCP client. Service code matches with errors.Is.
var (
	// ErrDisconnected is returned by in-flight Send calls when the underlying
	// socket has been torn down (graceful close, reader EOF, or unrecoverable
	// read error). Callers treat this as transient and let the outbox worker
	// retry on the next tick.
	ErrDisconnected = werror.Error("signal-cli: tcp disconnected")

	// ErrShutdown is returned when Send is called after Close has been
	// initiated. Distinct from ErrDisconnected so handlers can map it to
	// SignalCliUnavailable without triggering retry storms during drain.
	ErrShutdown = werror.Error("signal-cli: client shutting down")
)

// TCPClient is the persistent signal-cli JSON-RPC client. Topology and design
// notes live in docs/signal-cli.md. One *net.Conn per process; concurrent Send
// is safe (writes serialised via writeMu); responses + asynchronous events are
// demultiplexed by a single reader goroutine.
type TCPClient struct {
	cfg    config.SignalCliTCP
	logger svc1log.Logger

	// runCtx is bound to the fx lifecycle (Start/Close). Once cancelled the
	// reader/dialer loops exit and Send returns ErrShutdown.
	runCtx    context.Context
	runCancel context.CancelFunc
	wg        sync.WaitGroup

	mu       sync.Mutex
	conn     net.Conn
	connGen  uint64                      // bumped each (re)connect; pending entries belong to a generation
	pending  map[string]chan rpcResponse // request-id → response channel
	closed   bool
	dialOnce chan struct{} // closed when the first dial attempt completes (used by Send to wait at boot)

	writeMu sync.Mutex // held around conn.Write to serialize frames

	events chan Event // buffered; clients read via Events()
}

// Event is an asynchronous JSON-RPC notification received from signal-cli
// (id is null/absent). The raw Params slice is the full JSON for the params
// field; the inbound listener decodes it per signal-cli's schema (M6).
type Event struct {
	Method string
	Params json.RawMessage
}

// SendResult is what a successful "send" JSON-RPC call returns from the
// daemon. The result_id (signal-cli "timestamp") is stamped on the row as
// result_id; downstream services compare against incoming receipts using it.
type SendResult struct {
	Timestamp int64           `json:"timestamp"`
	Raw       json.RawMessage `json:"-"`
}

// ResultID returns the timestamp as a string for storage in the row's
// result_id column.
func (r SendResult) ResultID() string { return strconv.FormatInt(r.Timestamp, 10) }

// NewTCPClient builds an unstarted TCP client. fx.Lifecycle.OnStart calls
// Start, which kicks off the dialer + reader goroutines.
func NewTCPClient(install config.Install, logger svc1log.Logger) *TCPClient {
	return &TCPClient{
		cfg:      install.SignalCli.TCP,
		logger:   logger,
		pending:  make(map[string]chan rpcResponse),
		dialOnce: make(chan struct{}),
		events:   make(chan Event, 64),
	}
}

// Start kicks off the dial/reconnect loop. Returns immediately — the first
// dial happens asynchronously. Send blocks until the first successful dial
// (or runCtx cancellation).
func (c *TCPClient) Start(ctx context.Context) {
	c.runCtx, c.runCancel = context.WithCancel(context.WithoutCancel(ctx))
	c.wg.Add(1)
	go c.dialLoop()
}

// Close tears down the client. Cancels runCtx, closes the active conn so the
// reader unblocks, then waits for goroutines. Safe to call concurrently with
// in-flight Send; in-flight callers see ErrDisconnected.
func (c *TCPClient) Close(ctx context.Context) error {
	c.mu.Lock()
	c.closed = true
	conn := c.conn
	c.mu.Unlock()
	if c.runCancel != nil {
		c.runCancel()
	}
	if conn != nil {
		_ = conn.Close()
	}
	done := make(chan struct{})
	go func() { c.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return werror.WrapWithContextParams(ctx, ctx.Err(), "tcp client close: drain timeout")
	}
}

// Events returns the receive channel for asynchronous events. The channel is
// closed when the client shuts down. The buffer is fixed; a slow consumer
// loses events (logged at WARN). Sized for typical signal-cli notify volume.
func (c *TCPClient) Events() <-chan Event { return c.events }

// Send dispatches a JSON-RPC "send" call with the given params and blocks
// until the daemon replies or ctx expires.
//
// `tcpIgnoreResults` flips Send to fire-and-forget: writes the frame and
// returns success without registering a pending entry. Matches the Node
// implementation's legacy mode and is gated by install.yml.
func (c *TCPClient) Send(ctx context.Context, params any) (SendResult, error) {
	if c.cfg.IgnoreResults {
		return c.fireAndForget(ctx, params)
	}
	return c.sendAwait(ctx, params)
}

// rpcRequest is the JSON-RPC 2.0 envelope written to the socket.
type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
	ID      string `json:"id,omitempty"`
}

// rpcResponse / rpcFrame are the demux types. A frame is either a response
// (Result OR Error set) or a notification (Method set, ID nil).
type rpcFrame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *string         `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return e.Message }

type rpcResponse struct {
	result json.RawMessage
	err    *rpcError
	// disconnect is set when the reader fails the pending channel during
	// reconnect — distinguishes from a daemon-supplied error.
	disconnect bool
}

func (c *TCPClient) sendAwait(ctx context.Context, params any) (SendResult, error) {
	if err := c.waitConnected(ctx); err != nil {
		return SendResult{}, err
	}
	id := uuid.NewString()
	ch := make(chan rpcResponse, 1)

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return SendResult{}, ErrShutdown
	}
	c.pending[id] = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	if err := c.writeFrame(rpcRequest{JSONRPC: "2.0", Method: "send", Params: params, ID: id}); err != nil {
		return SendResult{}, err
	}

	timeout := c.cfg.WaitResultTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return SendResult{}, werror.WrapWithContextParams(ctx, ctx.Err(), "signal-cli send ctx")
	case <-c.runCtx.Done():
		return SendResult{}, ErrShutdown
	case <-timer.C:
		return SendResult{}, werror.ErrorWithContextParams(ctx, "signal-cli send timeout",
			werror.SafeParam("timeout", timeout.String()))
	case resp := <-ch:
		if resp.disconnect {
			return SendResult{}, ErrDisconnected
		}
		if resp.err != nil {
			return SendResult{}, werror.ErrorWithContextParams(ctx, "signal-cli error",
				werror.SafeParam("code", resp.err.Code),
				werror.SafeParam("message", resp.err.Message))
		}
		out := SendResult{Raw: resp.result}
		_ = json.Unmarshal(resp.result, &out) // best-effort timestamp extraction
		return out, nil
	}
}

func (c *TCPClient) fireAndForget(ctx context.Context, params any) (SendResult, error) {
	if err := c.waitConnected(ctx); err != nil {
		return SendResult{}, err
	}
	if err := c.writeFrame(rpcRequest{JSONRPC: "2.0", Method: "send", Params: params}); err != nil {
		return SendResult{}, err
	}
	return SendResult{}, nil
}

func (c *TCPClient) waitConnected(ctx context.Context) error {
	c.mu.Lock()
	dialOnce := c.dialOnce
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return ErrShutdown
	}
	select {
	case <-dialOnce:
		return nil
	case <-ctx.Done():
		return werror.WrapWithContextParams(ctx, ctx.Err(), "signal-cli waiting for dial")
	case <-c.runCtx.Done():
		return ErrShutdown
	}
}

func (c *TCPClient) writeFrame(req rpcRequest) error {
	body, err := json.Marshal(req)
	if err != nil {
		return werror.Wrap(err, "marshal rpc frame")
	}
	body = append(body, '\n')

	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return ErrDisconnected
	}
	if _, err := conn.Write(body); err != nil {
		return werror.Wrap(err, "tcp write")
	}
	return nil
}

// dialLoop is the connect+reader+reconnect supervisor. It owns runCtx;
// each successful dial spawns a reader that returns when the conn closes,
// then the loop reconnects after a backoff (unless runCtx is done).
func (c *TCPClient) dialLoop() {
	defer c.wg.Done()
	addr := net.JoinHostPort(c.cfg.Host, strconv.Itoa(c.cfg.Port))

	const (
		baseBackoff = 500 * time.Millisecond
		maxBackoff  = 30 * time.Second
	)
	backoff := baseBackoff
	first := true

	for {
		if c.runCtx.Err() != nil {
			c.markShutdown()
			return
		}
		conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(c.runCtx, "tcp", addr)
		if err != nil {
			c.logger.Warn("signal-cli dial failed",
				svc1log.SafeParam("addr", addr),
				svc1log.Stacktrace(err))
			if !sleepCtx(c.runCtx, backoff) {
				c.markShutdown()
				return
			}
			backoff = nextBackoff(backoff, maxBackoff)
			continue
		}

		// Success.
		c.mu.Lock()
		c.conn = conn
		c.connGen++
		if first {
			close(c.dialOnce)
		}
		// `first` is reset to true on every disconnect below, so explicitly
		// setting it to false here is dead — the loop top reads only the
		// value that was set after the previous iteration's reconnect path.
		c.mu.Unlock()
		backoff = baseBackoff
		c.logger.Info("signal-cli connected", svc1log.SafeParam("addr", addr))

		c.readLoop(conn)
		// readLoop returned → conn dead → fail pending and reconnect.
		c.failPending(ErrDisconnected)
		_ = conn.Close()

		c.mu.Lock()
		c.conn = nil
		closed := c.closed
		c.mu.Unlock()
		if closed || c.runCtx.Err() != nil {
			c.markShutdown()
			return
		}
		// On unexpected disconnect, also re-arm dialOnce so new Send calls
		// wait for the next successful dial instead of racing through.
		c.mu.Lock()
		c.dialOnce = make(chan struct{})
		first = true
		c.mu.Unlock()
	}
}

// readLoop reads one JSON line at a time from conn until EOF / error.
func (c *TCPClient) readLoop(conn net.Conn) {
	r := bufio.NewReaderSize(conn, 64*1024)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			c.dispatchFrame(line)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) && c.runCtx.Err() == nil {
				c.logger.Warn("signal-cli read error", svc1log.Stacktrace(err))
			}
			return
		}
	}
}

func (c *TCPClient) dispatchFrame(line []byte) {
	// Trim trailing newline / spaces; tolerate blank frames.
	for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r' || line[len(line)-1] == ' ') {
		line = line[:len(line)-1]
	}
	if len(line) == 0 {
		return
	}
	var frame rpcFrame
	if err := json.Unmarshal(line, &frame); err != nil {
		c.logger.Warn("signal-cli: malformed frame",
			svc1log.SafeParam("len", len(line)),
			svc1log.Stacktrace(err))
		return
	}
	if frame.ID != nil && *frame.ID != "" {
		c.mu.Lock()
		ch, ok := c.pending[*frame.ID]
		if ok {
			delete(c.pending, *frame.ID)
		}
		c.mu.Unlock()
		if !ok {
			c.logger.Debug("signal-cli: unmatched response id",
				svc1log.SafeParam("id", *frame.ID))
			return
		}
		ch <- rpcResponse{result: frame.Result, err: frame.Error}
		return
	}
	// Notification — route to events channel; drop if full so a slow consumer
	// can't stall the read loop.
	if frame.Method == "" {
		return
	}
	select {
	case c.events <- Event{Method: frame.Method, Params: frame.Params}:
	default:
		c.logger.Warn("signal-cli: events buffer full, dropping",
			svc1log.SafeParam("method", frame.Method))
	}
}

// failPending releases every pending Send call when the connection dies.
// Each waiter unblocks with ErrDisconnected.
func (c *TCPClient) failPending(_ error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, ch := range c.pending {
		select {
		case ch <- rpcResponse{disconnect: true}:
		default:
		}
		delete(c.pending, id)
	}
}

func (c *TCPClient) markShutdown() {
	c.failPending(ErrShutdown)
	c.mu.Lock()
	if !c.closed {
		c.closed = true
	}
	// dialOnce may not be closed yet if shutdown happens before the first
	// successful dial; close it so any waiting Send unblocks.
	select {
	case <-c.dialOnce:
	default:
		close(c.dialOnce)
	}
	c.mu.Unlock()
}

func nextBackoff(cur, max time.Duration) time.Duration {
	next := cur * 2
	if next > max {
		next = max
	}
	return next
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
