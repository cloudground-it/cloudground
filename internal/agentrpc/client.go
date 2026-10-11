package agentrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Caller is the untyped seam under Call: *Client implements it, and the
// API's tests fake it. Implementations should return *Error values.
type Caller interface {
	Call(ctx context.Context, method string, params, out any) error
}

// Call invokes m with p and returns its typed result.
func Call[P, R any](ctx context.Context, c Caller, m Method[P, R], p P) (R, error) {
	var r R
	err := c.Call(ctx, m.Name(), p, &r)
	return r, err
}

// Into is Call decoding into an existing variable; out may be nil when the
// result is not needed.
func Into[P, R any](ctx context.Context, c Caller, m Method[P, R], p P, out *R) error {
	if out == nil {
		return c.Call(ctx, m.Name(), p, nil)
	}
	return c.Call(ctx, m.Name(), p, out)
}

// grace is how much longer than the method's deadline the client waits, so
// the server's typed timeout arrives first.
const grace = 30 * time.Second

// maxResult bounds a response: a 16 MiB download is about 22 MiB as base64.
const maxResult = 64 << 20

// Client calls the agent over its socket. It is safe for concurrent use;
// each call dials its own connection, as before, so a call never lands on a
// connection a restarted agent has already closed.
type Client struct {
	token string
	http  *http.Client
	log   *slog.Logger
}

// NewClient returns a client that calls the agent listening on the socket path.
func NewClient(path, token string, log *slog.Logger) *Client {
	if log == nil {
		log = slog.Default()
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dial(ctx, path)
		},
		DisableKeepAlives:  true,
		DisableCompression: true,
	}
	return &Client{token: token, http: &http.Client{Transport: tr}, log: log}
}

// dial retries briefly: systemd may report panel-agent active a moment
// before its socket is bound. An agent that is really down still fails
// within half a second.
func dial(ctx context.Context, path string) (net.Conn, error) {
	var d net.Dialer
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		var c net.Conn
		dctx, cancel := context.WithTimeout(ctx, time.Second)
		c, err = d.DialContext(dctx, "unix", path)
		cancel()
		if err == nil {
			return c, nil
		}
		if attempt < 4 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	return nil, err
}

// Call implements Caller. Every error it returns is an *Error; its detail is
// logged here, once, and goes no further.
func (c *Client) Call(ctx context.Context, method string, params, out any) error {
	spec, ok := Lookup(method)
	if !ok {
		return c.fail(method, &Error{Code: CodeInternal, Message: "unknown agent method", Detail: method})
	}
	if !spec.ReadOnly {
		// A change, once sent, runs to its end or its deadline: a caller that
		// goes away must not leave the API unaware of what the agent did.
		ctx = context.WithoutCancel(ctx)
	}
	ctx, cancel := context.WithTimeout(ctx, spec.Timeout+grace)
	defer cancel()
	body, err := json.Marshal(params)
	if err != nil {
		return c.fail(method, &Error{Code: CodeInternal, Message: spec.Fail, Detail: "marshal params: " + err.Error()})
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://agent"+pathPrefix+method, bytes.NewReader(body))
	if err != nil {
		return c.fail(method, &Error{Code: CodeInternal, Message: spec.Fail, Detail: err.Error()})
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			e := ctxError(spec, ctx)
			e.Detail += ": " + err.Error()
			return c.fail(method, e)
		}
		return c.fail(method, &Error{Code: CodeUnavailable, Message: "The agent is not reachable", Detail: err.Error()})
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResult))
	if err != nil {
		return c.fail(method, &Error{Code: CodeUnavailable, Message: spec.Fail, Detail: "read response: " + err.Error()})
	}
	if resp.StatusCode != http.StatusOK {
		var e Error
		if json.Unmarshal(data, &e) != nil || e.Code == "" || e.Message == "" {
			e = Error{Code: CodeInternal, Message: spec.Fail, Detail: fmt.Sprintf("HTTP %d: %.200s", resp.StatusCode, data)}
		}
		return c.fail(method, &e)
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return c.fail(method, &Error{Code: CodeInternal, Message: spec.Fail, Detail: "decode result: " + err.Error()})
		}
	}
	return nil
}

func (c *Client) fail(method string, e *Error) *Error {
	c.log.Warn("agent call failed", "method", method, "code", e.Code, "message", e.Message, "detail", RedactCredentials(e.Detail))
	return e
}
