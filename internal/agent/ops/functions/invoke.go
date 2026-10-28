package functions

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	fn "github.com/cloudground-it/cloudground/internal/functions"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

// Invoke is InvokeFunction (SPEC §5): through nginx's internal server, as
// a visitor's request but without the token check.
func (o *Ops) Invoke(ctx context.Context, p rpc.InvokeFunctionParams) (rpc.InvokeResult, error) {
	sock := or(o.NginxSocket, fn.InvokeSocket)
	timeout := time.Duration(max(p.TimeoutSec, 1)+10) * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// The unix socket only root reaches (internal/engine SPEC §8.2): no TLS
	// on a hop that never leaves the machine.
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
		DisableKeepAlives: true,
	}
	c := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	method := p.Method
	if method == "" {
		method = http.MethodPost
	}
	var body io.Reader
	if p.Body != "" || method != http.MethodGet {
		body = strings.NewReader(p.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+fn.InternalHost+"/"+p.Name+p.Path, body)
	if err != nil {
		return rpc.InvokeResult{Error: err.Error()}, nil
	}
	req.Header.Set("User-Agent", "CloudGround-Functions/"+p.Source)
	req.Header.Set("X-CloudGround-Source", p.Source)
	if p.Body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	start := time.Now()
	resp, err := c.Do(req)
	res := rpc.InvokeResult{MS: time.Since(start).Milliseconds()}
	if err != nil {
		res.Error = err.Error()
		return res, nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	res.Status, res.Body = resp.StatusCode, string(b)
	return res, nil
}

// accessLine is the cloudground log format, as far as an invocation needs.
var accessLine = regexp.MustCompile(`^\S+ - \S+ \[([^\]]+)\] "(\S+) (\S+) [^"]*" (\d{3}) \d+ ([0-9.]+) \S+ "([^"]*)"`)

// parseInvocation reads one access-log line.
func parseInvocation(line string) (rpc.Invocation, bool) {
	m := accessLine.FindStringSubmatch(line)
	if m == nil {
		return rpc.Invocation{}, false
	}
	status, _ := strconv.Atoi(m[4])
	secs, _ := strconv.ParseFloat(m[5], 64)
	src := "http"
	if rest, ok := strings.CutPrefix(m[6], "CloudGround-Functions/"); ok && (rest == "schedule" || rest == "test") {
		src = rest // the agent's own run (SPEC §5)
	}
	at := m[1]
	if t, err := time.Parse("02/Jan/2006:15:04:05 -0700", m[1]); err == nil {
		at = t.UTC().Format(time.RFC3339)
	}
	pth, _, _ := strings.Cut(m[3], "?")
	return rpc.Invocation{Time: at, Method: m[2], Path: pth, Status: status, MS: int64(secs * 1000), Source: src}, true
}

// Logs is FunctionLogs (SPEC §6).
func (o *Ops) Logs(_ context.Context, p rpc.FunctionRef) (rpc.FunctionLogsResult, error) {
	res := rpc.FunctionLogsResult{Invocations: []rpc.Invocation{}, Output: []string{}}
	for _, line := range tailLines(o.accessLog(p.Name), 2<<20, 0) {
		if inv, ok := parseInvocation(line); ok {
			res.Invocations = append(res.Invocations, inv)
		}
	}
	slices.Reverse(res.Invocations)
	if len(res.Invocations) > 100 {
		res.Invocations = res.Invocations[:100]
	}
	if out := tailLines(o.outputLog(p.Name), 256<<10, 200); out != nil {
		res.Output = out
	}
	return res, nil
}

// Summary is FunctionsSummary (SPEC §6).
func (o *Ops) Summary(_ context.Context, p rpc.FunctionsSummaryParams) (rpc.FunctionsSummaryResult, error) {
	res := rpc.FunctionsSummaryResult{Last: map[string]rpc.Invocation{}}
	for _, name := range p.Names {
		lines := tailLines(o.accessLog(name), 64<<10, 0)
		for i := len(lines) - 1; i >= 0; i-- {
			if inv, ok := parseInvocation(lines[i]); ok {
				res.Last[name] = inv
				break
			}
		}
	}
	return res, nil
}

// tailLines is a file's last lines within its last max bytes (at most n
// lines when n > 0).
func tailLines(path string, max int64, n int) []string {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return nil
	}
	off := fi.Size() - max
	if off < 0 {
		off = 0
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(f, max))
	if off > 0 {
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:]
		}
	}
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	if n > 0 && len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}
