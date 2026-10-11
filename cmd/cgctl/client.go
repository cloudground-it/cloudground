package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/config"
)

// panel talks to the API.
type panel struct {
	base   string
	http   *http.Client
	token  string
	stderr io.Writer
}

// newPanel builds the client for cfg.URL: the panel's own socket (plain
// HTTP; only root and nginx may open it; its session cookie is carried by
// socketJar, since the standard jar keeps a Secure cookie off plain HTTP),
// or HTTPS, verified unless the host
// is loopback (the panel starts on a self-signed certificate) or
// CLOUDGROUND_INSECURE is set.
func newPanel(cfg *config.Config, stderr io.Writer) (*panel, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{}
	base := cfg.URL
	var jar http.CookieJar
	if u.Scheme == "unix" {
		jar = newSocketJar()
		sock := u.Path
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}
		base = "http://cloudground"
	} else {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: cfg.Insecure || config.IsLoopback(u.Hostname())}
	}
	if jar == nil {
		jar, _ = cookiejar.New(nil)
	}
	return &panel{
		base:   base,
		http:   &http.Client{Jar: jar, Timeout: 60 * time.Second, Transport: tr},
		stderr: stderr,
	}, nil
}

// apiError is a non-2xx answer.
type apiError struct {
	status int
	body   []byte
}

func (e *apiError) Error() string {
	var m struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(e.body, &m) == nil && m.Error != "" {
		return fmt.Sprintf("HTTP %d: %s", e.status, m.Error)
	}
	return fmt.Sprintf("HTTP %d: %s", e.status, strings.TrimSpace(string(e.body)))
}

func (p *panel) do(ctx context.Context, r request) ([]byte, error) {
	var body io.Reader
	if r.body != nil {
		b, err := json.Marshal(r.body)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, r.method, p.base+r.path, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if p.token != "" {
		req.Header.Set("Authorization", "Bearer "+p.token)
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return out, &apiError{resp.StatusCode, out}
	}
	return out, nil
}

// authenticate picks the credential (SPEC.md §2): the token, unless the
// command needs a password session; else email and password.
func (p *panel) authenticate(ctx context.Context, cfg *config.Config, needsPassword bool) error {
	if cfg.Token != "" && !needsPassword {
		p.token = cfg.Token
		return nil
	}
	if cfg.Email == "" || cfg.Password == "" {
		if needsPassword {
			return errors.New("this command needs a password sign-in: set CLOUDGROUND_EMAIL and CLOUDGROUND_PASSWORD (the panel refuses API tokens on accounts, users and tokens)")
		}
		return errors.New("set CLOUDGROUND_TOKEN (or CLOUDGROUND_TOKEN_FILE) to an API token; create one with: cgctl tokens create <name>, signed in with CLOUDGROUND_EMAIL and CLOUDGROUND_PASSWORD")
	}
	_, err := p.do(ctx, request{method: "POST", path: "/api/login", body: map[string]string{"email": cfg.Email, "password": cfg.Password, "totp": cfg.TOTP}})
	if err != nil {
		return fmt.Errorf("sign-in: %w", err)
	}
	return nil
}

// execute runs one invocation and returns the exit code (SPEC.md §4).
func execute(cfg *config.Config, inv invocation, stdout, stderr io.Writer) int {
	for _, w := range inv.req.warnings {
		fmt.Fprintln(stderr, "cgctl: warning:", w)
	}
	p, err := newPanel(cfg, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "cgctl:", err)
		return exitUsage
	}
	ctx := context.Background()
	if err := p.authenticate(ctx, cfg, inv.cmd.password); err != nil {
		fmt.Fprintln(stderr, "cgctl:", err)
		return exitFailed
	}
	out, err := p.do(ctx, inv.req)
	if err != nil {
		fmt.Fprintln(stderr, "cgctl:", err)
		return exitFailed
	}
	taskID, ok := startedTask(out)
	if !inv.wait || !ok {
		if inv.req.siteSummary {
			printSiteSummary(stdout, out, "")
		} else {
			printJSON(stdout, out)
		}
		return exitOK
	}
	code, final := p.wait(ctx, taskID, inv.timeout)
	switch {
	case inv.req.siteSummary:
		var t task
		_ = json.Unmarshal(final, &t)
		if t.Status == "" {
			t.Status = "is still running"
		}
		printSiteSummary(stdout, out, t.Status)
	case final != nil:
		printJSON(stdout, final)
	}
	return code
}

// startedTask finds the task a response started: {"task": {...}} (or
// {"job": {...}}, the job queue's name), or a task itself (cgctl task <id>).
func startedTask(body []byte) (int64, bool) {
	var r struct {
		Task *struct {
			ID int64 `json:"id"`
		} `json:"task"`
		Job *struct {
			ID int64 `json:"id"`
		} `json:"job"`
		ID     int64  `json:"id"`
		Status string `json:"status"`
		Kind   string `json:"kind"`
	}
	if json.Unmarshal(body, &r) != nil {
		return 0, false
	}
	switch {
	case r.Task != nil && r.Task.ID > 0:
		return r.Task.ID, true
	case r.Job != nil && r.Job.ID > 0:
		return r.Job.ID, true
	case r.ID > 0 && r.Status != "" && r.Kind != "":
		return r.ID, true
	}
	return 0, false
}

// task is what cgctl reads of a task while waiting.
type task struct {
	ID       int64  `json:"id"`
	Kind     string `json:"kind"`
	Status   string `json:"status"`
	Progress int    `json:"progress"`
	Message  string `json:"message"`
	Error    string `json:"error"`
}

// finished reports whether status is terminal, and whether it is success.
func finished(status string) (done, ok bool) {
	switch status {
	case "succeeded":
		return true, true
	case "pending", "queued", "running", "":
		return false, false
	}
	return true, false // failed, cancelled, or anything else final
}

// pollEvery is how often --wait asks for the task (tests shorten it).
var pollEvery = time.Second

// wait polls the task until it ends or the timeout runs out, and answers
// the exit code with the final task (nil when it did not end). Answers
// that fail on the way (the panel restarting during a self-update) are
// retried.
func (p *panel) wait(ctx context.Context, id int64, timeout time.Duration) (int, []byte) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	path := fmt.Sprintf("/api/tasks/%d", id)
	last := ""
	for {
		body, err := p.do(ctx, request{method: "GET", path: path, body: nil})
		var t task
		if err == nil && json.Unmarshal(body, &t) == nil {
			if line := progressLine(t); line != last {
				fmt.Fprintln(p.stderr, line)
				last = line
			}
			if done, ok := finished(t.Status); done {
				if !ok {
					fmt.Fprintf(p.stderr, "cgctl: task %d (%s) %s: %s\n", t.ID, t.Kind, t.Status, t.Error)
					return exitFailed, body
				}
				return exitOK, body
			}
		} else if ae := (*apiError)(nil); errors.As(err, &ae) && (ae.status == http.StatusNotFound || ae.status == http.StatusUnauthorized || ae.status == http.StatusForbidden) {
			fmt.Fprintln(p.stderr, "cgctl: waiting for task", id, "failed:", err)
			return exitFailed, nil
		}
		select {
		case <-ctx.Done():
			fmt.Fprintf(p.stderr, "cgctl: task %d still running after %s\n", id, timeout)
			return exitTimeout, nil
		case <-time.After(pollEvery):
		}
	}
}

func progressLine(t task) string {
	s := fmt.Sprintf("[%s #%d] %s %d%%", t.Kind, t.ID, t.Status, t.Progress)
	if t.Message != "" {
		s += " — " + t.Message
	}
	return s
}

func printJSON(w io.Writer, raw []byte) {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		fmt.Fprintln(w, string(raw))
		return
	}
	out, _ := json.MarshalIndent(v, "", "  ")
	fmt.Fprintln(w, string(out))
}
