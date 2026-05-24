package signal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors" //nolint:depguard // stdlib errors.Is for sentinel comparison in retry loop
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/google/uuid"
	werror "github.com/palantir/witchcraft-go-error"
	"github.com/palantir/witchcraft-go-logging/wlog/svclog/svc1log"

	"github.com/olehmushka/go-signalium/internal/config"
	signalapi "github.com/olehmushka/go-signalium/internal/generated/signalium/api"
)

// HTTPClient wraps the signal-cli sidecar's HTTP/JSON-RPC daemon. It is used
// for read-only proxies (groups listing today, account info later). Send
// itself stays on the persistent TCP socket — see TCPClient.
//
// signal-cli's HTTP daemon exposes a single endpoint `/api/v1/rpc` that takes
// a JSON-RPC envelope and dispatches to the same methods as the TCP socket.
// We use it for synchronous reads (groups listing) where the async TCP
// response model is the wrong shape.
type HTTPClient struct {
	cfg     config.SignalCliHTTP
	account string
	base    string
	http    *http.Client
	logger  svc1log.Logger
}

// NewHTTPClient is the fx provider. The configured sender phone number doubles
// as the signal-cli `account` argument because the daemon is linked to a
// single Signal identity per process — mirroring docs/signal-cli.md.
func NewHTTPClient(install config.Install, logger svc1log.Logger) *HTTPClient {
	timeout := install.SignalCli.HTTP.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	scheme := "http"
	addr := net.JoinHostPort(install.SignalCli.HTTP.Host, strconv.Itoa(install.SignalCli.HTTP.Port))
	return &HTTPClient{
		cfg:     install.SignalCli.HTTP,
		account: install.SignalCli.SenderPhoneNumber,
		base:    (&url.URL{Scheme: scheme, Host: addr}).String(),
		http:    &http.Client{Timeout: timeout},
		logger:  logger,
	}
}

// ListGroups proxies `listGroups` and maps each ListedGroup into the conjure
// SignalGroupInfo shape. On daemon unreachable / 5xx, the call is retried up
// to maxRetries times with exponential backoff; the final failure surfaces as
// SignalCliUnavailable so the conjure handler returns 503.
func (c *HTTPClient) ListGroups(ctx context.Context) ([]signalapi.SignalGroupInfo, error) {
	req := jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  "listGroups",
		ID:      uuid.NewString(),
		Params:  map[string]any{"account": c.account},
	}
	var resp listGroupsResponse
	if err := c.callWithRetry(ctx, req, &resp); err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, werror.ErrorWithContextParams(ctx, "signal-cli listGroups error",
			werror.SafeParam("code", resp.Error.Code),
			werror.SafeParam("message", resp.Error.Message))
	}
	out := make([]signalapi.SignalGroupInfo, 0, len(resp.Result))
	for _, g := range resp.Result {
		out = append(out, mapListedGroup(g))
	}
	return out, nil
}

func (c *HTTPClient) callWithRetry(ctx context.Context, req jsonRPCRequest, out any) error {
	maxRetries := c.cfg.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 3
	}
	body, err := json.Marshal(req)
	if err != nil {
		return werror.Wrap(err, "marshal jsonrpc request")
	}

	var lastErr error
	backoff := 200 * time.Millisecond
	for attempt := 0; attempt <= maxRetries; attempt++ {
		httpResp, err := c.do(ctx, body)
		if err == nil {
			respBody, readErr := io.ReadAll(httpResp.Body)
			_ = httpResp.Body.Close()
			if readErr == nil && httpResp.StatusCode < 500 {
				if httpResp.StatusCode >= 400 {
					return werror.ErrorWithContextParams(ctx, "signal-cli http error",
						werror.SafeParam("status", httpResp.StatusCode))
				}
				if jerr := json.Unmarshal(respBody, out); jerr != nil {
					return werror.WrapWithContextParams(ctx, jerr, "decode jsonrpc response")
				}
				return nil
			}
			lastErr = werror.ErrorWithContextParams(ctx, "signal-cli 5xx",
				werror.SafeParam("status", httpResp.StatusCode))
		} else {
			lastErr = err
		}

		if attempt == maxRetries {
			break
		}
		select {
		case <-ctx.Done():
			return signalapi.WrapWithSignalCliUnavailable(
				werror.WrapWithContextParams(ctx, ctx.Err(), "signal-cli http ctx"),
				c.cfg.Host, c.cfg.Port,
			)
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > 5*time.Second {
			backoff = 5 * time.Second
		}
	}
	return signalapi.WrapWithSignalCliUnavailable(
		werror.WrapWithContextParams(ctx, lastErr, "signal-cli http unreachable"),
		c.cfg.Host, c.cfg.Port,
	)
}

func (c *HTTPClient) do(ctx context.Context, body []byte) (*http.Response, error) {
	endpoint := c.base + "/api/v1/rpc"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, werror.Wrap(err, "build http request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			c.logger.Debug("signal-cli http call failed",
				svc1log.SafeParam("endpoint", endpoint),
				svc1log.SafeParam("op", ue.Op))
		}
		return nil, fmt.Errorf("signal-cli http call: %w", err)
	}
	return resp, nil
}

type jsonRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	ID      string `json:"id"`
	Params  any    `json:"params,omitempty"`
}

type jsonRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type listGroupsResponse struct {
	JSONRPC string        `json:"jsonrpc"`
	Result  []listedGroup `json:"result"`
	ID      string        `json:"id"`
	Error   *jsonRPCError `json:"error,omitempty"`
}

// listedGroup mirrors the signal-cli `listGroups` result row. Only the fields
// surfaced by SignalGroupInfo are pulled out; the rest are ignored.
type listedGroup struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Members     []groupMember `json:"members"`
}

type groupMember struct {
	Number *string `json:"number"`
	UUID   string  `json:"uuid"`
}

func mapListedGroup(g listedGroup) signalapi.SignalGroupInfo {
	info := signalapi.SignalGroupInfo{Id: g.ID, Name: g.Name}
	if g.Description != "" {
		desc := g.Description
		info.Description = &desc
	}
	if len(g.Members) > 0 {
		members := make([]string, 0, len(g.Members))
		for _, m := range g.Members {
			if m.Number != nil && *m.Number != "" {
				members = append(members, *m.Number)
				continue
			}
			if m.UUID != "" {
				members = append(members, m.UUID)
			}
		}
		info.Members = &members
	}
	return info
}
