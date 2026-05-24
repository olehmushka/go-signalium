// Package testfake provides an in-process TCP server that speaks the same
// JSON-RPC 2.0 line-delimited dialect as signal-cli. It lets unit and
// end-to-end tests cover the TCPClient and outbox worker without spinning up
// the real Java daemon. See docs/signal-cli.md for the protocol surface.
package testfake

import (
	"bufio"
	"encoding/json"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Daemon is the test double. Start it with New(t); each accepted connection
// is read line-by-line, and the configured Responder produces a reply (or
// scripts an asynchronous event, or a disconnect).
type Daemon struct {
	t         *testing.T
	ln        net.Listener
	responder Responder

	mu       sync.Mutex
	conns    []net.Conn
	requests []ReceivedRequest
	closed   atomic.Bool
	wg       sync.WaitGroup
}

// Responder decides how the daemon replies to one parsed JSON-RPC request. It
// receives the full inbound frame and writes back zero, one, or many output
// frames. Implementations should not block indefinitely; tests pass a context
// to whatever they're observing.
type Responder func(req IncomingRequest, out FrameWriter)

// IncomingRequest is the parsed JSON-RPC request from the client.
type IncomingRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	ID      string          `json:"id"`
}

// FrameWriter writes one JSON-RPC frame back to the client. It appends the
// trailing newline so callers don't have to.
type FrameWriter interface {
	WriteResult(id string, result any)
	WriteError(id string, code int, message string)
	WriteNotification(method string, params any)
	Disconnect()
}

// ReceivedRequest captures one request as the fake saw it; tests assert
// against the slice via Requests().
type ReceivedRequest struct {
	Method string
	ID     string
	Raw    json.RawMessage
}

// New starts a daemon on a random loopback port. The caller passes a
// Responder; nil falls back to SuccessResponder.
func New(t *testing.T, r Responder) *Daemon {
	t.Helper()
	lc := net.ListenConfig{}
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("testfake: listen: %v", err)
	}
	d := &Daemon{t: t, ln: ln, responder: r}
	if d.responder == nil {
		d.responder = SuccessResponder()
	}
	d.wg.Add(1)
	go d.acceptLoop()
	t.Cleanup(d.Close)
	return d
}

// Addr returns the host:port the daemon is listening on.
func (d *Daemon) Addr() string { return d.ln.Addr().String() }

// HostPort returns the listener split into host and port for the
// SignalCliTCP config struct.
func (d *Daemon) HostPort() (string, int) {
	addr := d.ln.Addr().(*net.TCPAddr)
	return addr.IP.String(), addr.Port
}

// Requests returns a snapshot of every request the fake has received.
func (d *Daemon) Requests() []ReceivedRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]ReceivedRequest, len(d.requests))
	copy(out, d.requests)
	return out
}

// Close stops accepting + closes every active conn. Called from t.Cleanup.
func (d *Daemon) Close() {
	if d.closed.Swap(true) {
		return
	}
	_ = d.ln.Close()
	d.mu.Lock()
	for _, c := range d.conns {
		_ = c.Close()
	}
	d.mu.Unlock()
	// Don't block test teardown indefinitely if the accept loop is wedged.
	done := make(chan struct{})
	go func() { d.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
}

func (d *Daemon) acceptLoop() {
	defer d.wg.Done()
	for {
		conn, err := d.ln.Accept()
		if err != nil {
			return
		}
		d.mu.Lock()
		d.conns = append(d.conns, conn)
		d.mu.Unlock()
		d.wg.Add(1)
		go d.handle(conn)
	}
}

func (d *Daemon) handle(conn net.Conn) {
	defer d.wg.Done()
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	w := &connWriter{conn: conn, disconnect: func() { _ = conn.Close() }}
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			var req IncomingRequest
			if jerr := json.Unmarshal(line, &req); jerr == nil {
				d.mu.Lock()
				d.requests = append(d.requests, ReceivedRequest{Method: req.Method, ID: req.ID, Raw: append(json.RawMessage(nil), line...)})
				d.mu.Unlock()
				d.responder(req, w)
				if w.disconnected {
					return
				}
			}
		}
		if err != nil {
			return
		}
	}
}

// SuccessResponder returns a Responder that replies to every "send" with a
// deterministic timestamp = 1700000000 + n.
func SuccessResponder() Responder {
	var seq atomic.Int64
	return func(req IncomingRequest, out FrameWriter) {
		ts := int64(1700000000) + seq.Add(1)
		out.WriteResult(req.ID, map[string]any{"timestamp": ts})
	}
}

// ErrorResponder always returns the given JSON-RPC error.
func ErrorResponder(code int, message string) Responder {
	return func(req IncomingRequest, out FrameWriter) {
		out.WriteError(req.ID, code, message)
	}
}

// DisconnectResponder closes the connection without replying. Used to test
// the client's reconnect logic.
func DisconnectResponder() Responder {
	return func(_ IncomingRequest, out FrameWriter) {
		out.Disconnect()
	}
}

type connWriter struct {
	conn         net.Conn
	disconnect   func()
	disconnected bool
}

func (w *connWriter) WriteResult(id string, result any) {
	raw, _ := json.Marshal(result)
	_ = writeFrame(w.conn, map[string]any{"jsonrpc": "2.0", "id": id, "result": json.RawMessage(raw)})
}

func (w *connWriter) WriteError(id string, code int, message string) {
	_ = writeFrame(w.conn, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}

func (w *connWriter) WriteNotification(method string, params any) {
	_ = writeFrame(w.conn, map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (w *connWriter) Disconnect() {
	w.disconnected = true
	w.disconnect()
}

func writeFrame(conn net.Conn, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = conn.Write(b)
	return err
}
