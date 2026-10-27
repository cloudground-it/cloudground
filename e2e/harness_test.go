//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestMain lifts `go test`'s default 10-minute timeout, which the suite
// exceeds by design (safe pushes alone may take 15 minutes each), unless the
// operator passed -test.timeout explicitly.
func TestMain(m *testing.M) {
	flag.Parse()
	set := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "test.timeout" {
			set = true
		}
	})
	if !set {
		_ = flag.Set("test.timeout", "4h")
	}
	os.Exit(m.Run())
}

// ---------------------------------------------------------------- inputs --

// env holds the suite's inputs, the same ones scripts/e2e-verify.sh reads.
type env struct {
	Panel, Sock, IP, Email, PW, Dash, PHPV string
}

const adminEnvFile = "/root/cloudground-admin.env"

func loadEnv(t *testing.T) env {
	t.Helper()
	e := env{
		Panel: getenv("PANEL", "http://localhost"),
		Sock:  getenv("API_SOCK", "/run/cloudground/api.sock"),
		IP:    os.Getenv("IP"),
		Email: os.Getenv("PANEL_EMAIL"),
		PW:    os.Getenv("PANEL_PW"),
		PHPV:  os.Getenv("PHPV"),
	}
	if e.IP == "" || e.Email == "" || e.PW == "" {
		if b, err := os.ReadFile(adminEnvFile); err == nil {
			f := ParseEnvFile(string(b))
			first := func(keys ...string) string {
				for _, k := range keys {
					if f[k] != "" {
						return f[k]
					}
				}
				return ""
			}
			if e.IP == "" {
				e.IP = first("IP", "PUBLIC_IP")
			}
			if e.Email == "" {
				e.Email = first("PANEL_EMAIL", "ADMIN_EMAIL", "EMAIL")
			}
			if e.PW == "" {
				e.PW = first("PANEL_PW", "PANEL_PASSWORD", "ADMIN_PASSWORD", "PASSWORD")
			}
		}
	}
	if e.IP == "" {
		// The bash suite requires IP. The admin env file holds only the
		// credentials, so fall back to the source address of the default
		// route, but only when it is a public address: behind NAT it is not
		// the address the sites and the panel answer on.
		e.IP = defaultRouteIP()
		if e.IP != "" {
			t.Logf("IP not set: using the default route's public address %s", e.IP)
		}
	}
	if e.IP == "" {
		t.Fatal("set IP=<public ip> so sites resolve via sslip.io (no public address on the default route; " + adminEnvFile + " holds no IP)")
	}
	if e.Email == "" {
		t.Fatal("set PANEL_EMAIL=<admin email> (or put it in " + adminEnvFile + ")")
	}
	if e.PW == "" {
		t.Fatal("set PANEL_PW=<admin password> (or put it in " + adminEnvFile + ")")
	}
	e.Dash = DashIP(e.IP)
	// Never hardcode a PHP version: 24.04 ships 8.4, 26.04 ships 8.5. Detect
	// it by the FPM binary, never by /etc/php/<ver>/ (rendering a pool creates
	// that directory, so a failed provision leaves one that reads as installed).
	if e.PHPV == "" {
		matches, _ := filepath.Glob("/usr/sbin/php-fpm8.*")
		var execs []string
		for _, m := range matches {
			if fi, err := os.Stat(m); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
				execs = append(execs, m)
			}
		}
		e.PHPV = PickPHPVersion(execs)
	}
	if e.PHPV == "" {
		e.PHPV = "8.4"
	}
	return e
}

// defaultRouteIP is the local address the kernel picks to reach the
// internet (a UDP "connect" sends nothing), if it is a public IPv4 address.
func defaultRouteIP() string {
	c, err := net.Dial("udp4", "1.1.1.1:53")
	if err != nil {
		return ""
	}
	defer c.Close()
	ip := c.LocalAddr().(*net.UDPAddr).IP.String()
	if !IsPublicIPv4(ip) {
		return ""
	}
	return ip
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ---------------------------------------------------------------- harness --

// harness is the state one run shares: the inputs, the admin API session,
// the canary, the pass/fail tally and the safety-net cleanups.
type harness struct {
	root *testing.T
	env
	api    *apiClient
	canary *canary
	pid    int // stands in for the bash suite's $$ in markers

	mu     sync.Mutex
	pass   int
	failed []string
	// stats is the per-section tally and time, for the summary at the end.
	stats map[string]*sectionStats
	ran   []string // sections in the order they finished

	fixtures fixtureSet
}

type sectionStats struct {
	pass, fail int
	took       time.Duration
}

func newHarness(t *testing.T) *harness {
	if os.Geteuid() != 0 {
		t.Fatal("the e2e suite runs on a CloudGround server, as root")
	}
	h := &harness{root: t, env: loadEnv(t), pid: os.Getpid()}
	h.canary = newCanary(t, canaryDirs...)
	h.api = newAPIClient(h, h.Email, h.PW)
	t.Cleanup(func() {
		h.canary.check("end of run")
		h.mu.Lock()
		defer h.mu.Unlock()
		h.logSectionTimes(t)
		t.Logf("RESULT: %d passed, %d failed", h.pass, len(h.failed))
		for _, f := range h.failed {
			t.Logf("  failed: %s", f)
		}
	})
	return h
}

// step is called before every command, request and file change the suite
// makes, like the bash suite's DEBUG trap: the canary is checked and the
// description becomes the "after" that a later canary failure names.
func (h *harness) step(desc string) {
	h.canary.check(desc)
	if os.Getenv("CG_E2E_TRACE") != "" {
		fmt.Fprintf(os.Stderr, "    + %s\n", desc)
	}
}

// ---------------------------------------------------------------- checks --

// record counts one check in the RESULT tally and in its section's (t is
// the check's test or its section's).
func (h *harness) record(t *testing.T, name string, passed bool) {
	sec, _ := sectionOf(t)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stats == nil {
		h.stats = map[string]*sectionStats{}
	}
	st := h.stats[sec.name]
	if st == nil {
		st = &sectionStats{}
		h.stats[sec.name] = st
	}
	if passed {
		h.pass++
		st.pass++
	} else {
		h.failed = append(h.failed, name)
		st.fail++
	}
}

// sectionDone keeps a section's time for the summary.
func (h *harness) sectionDone(name string, took time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stats == nil {
		h.stats = map[string]*sectionStats{}
	}
	st := h.stats[name]
	if st == nil {
		st = &sectionStats{}
		h.stats[name] = st
	}
	st.took = took
	h.ran = append(h.ran, name)
}

// logSectionTimes prints each section that ran with its time and tally
// (h.mu held), so the durations in e2e/README.md can be kept measured.
func (h *harness) logSectionTimes(t *testing.T) {
	if len(h.ran) == 0 {
		return
	}
	t.Logf("SECTIONS (in the order they finished):")
	for _, name := range h.ran {
		st := h.stats[name]
		t.Logf("  %-32s %8s  %3d passed  %d failed", name, st.took.Round(time.Second), st.pass, st.fail)
	}
}

// check is the bash suite's `check "<name>" "$got" "$want"`: one subtest
// named after the check, passing when got equals want. A check that
// -test.run does not select is neither run nor counted.
func (h *harness) check(t *testing.T, name, got, want string) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		h.record(t, name, got == want)
		if got != want {
			t.Errorf("got %q want %q", got, want)
		} else {
			t.Logf("(%s)", got)
		}
	})
}

// assert is the bash suite's `cond && ok "<name>" || bad "<failure text>"`:
// the subtest carries the passing text, the failure carries the bad() text.
func (h *harness) assert(t *testing.T, name string, cond bool, failure string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(failure, args...)
	t.Run(name, func(t *testing.T) {
		h.record(t, name, cond)
		if !cond {
			t.Error(msg)
		}
	})
}

// fail is a bare `bad "<text>"` with no passing branch in the bash suite.
func (h *harness) fail(t *testing.T, name string, format string, args ...any) {
	t.Helper()
	h.assert(t, name, false, format, args...)
}

// undo registers fn as a safety net on the whole run and returns a function
// that runs it now; either way it runs once. Used for everything the bash
// suite tears down inline, so a failed check or a panic cannot leave the box
// changed.
func (h *harness) undo(desc string, fn func()) func() {
	var once sync.Once
	run := func() {
		once.Do(func() {
			h.step("undo: " + desc)
			fn()
		})
	}
	h.root.Cleanup(run)
	return run
}

// ---------------------------------------------------------------- canary --

// canary watches the system directories that must stay root's for the
// whole run: a root-side chown that follows a planted symlink shows up as
// /etc owned by a site user. It reports, it does not repair.
type canary struct {
	t    *testing.T
	mu   sync.Mutex
	prev string
	dirs []string
	bad  map[string]string // dir -> owner last reported
}

var canaryDirs = []string{"/etc", "/root", "/srv", "/usr", "/var/lib"}

func newCanary(t *testing.T, dirs ...string) *canary {
	return &canary{t: t, prev: "start of run", dirs: dirs, bad: map[string]string{}}
}

func (c *canary) check(next string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	uid, gid := uint32(os.Geteuid()), uint32(os.Getegid())
	for _, d := range c.dirs {
		var st syscall.Stat_t
		if err := syscall.Stat(d, &st); err != nil {
			continue
		}
		if st.Uid == uid && st.Gid == gid {
			delete(c.bad, d) // back to root's: a later change reports again
			continue
		}
		owner := fmt.Sprintf("uid=%d gid=%d mode=%o", st.Uid, st.Gid, st.Mode&0o7777)
		if c.bad[d] == owner {
			continue
		}
		c.bad[d] = owner
		c.t.Errorf("CANARY: %s stopped being root:root after: %s (%s)", d, c.prev, owner)
		fmt.Fprintf(os.Stderr, "  FAIL %s stopped being root:root after: %s (%s)\n", d, c.prev, owner)
	}
	c.prev = next
}

// ------------------------------------------------------------- API client --

// apiClient talks to the panel API over its Unix socket with a cookie jar,
// as `curl --unix-socket … -b jar -c jar` did. Every request opens a new
// connection, so an API restart (R-0.7) needs no special handling.
type apiClient struct {
	h         *harness
	hc        *http.Client
	jar       *Jar
	email, pw string
}

func newAPIClient(h *harness, email, pw string) *apiClient {
	jar := NewJar()
	sock := h.Sock
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
		DisableKeepAlives:  true,
		DisableCompression: true,
	}
	return &apiClient{h: h, jar: jar, email: email, pw: pw, hc: &http.Client{
		Transport: tr, Jar: jar, Timeout: 5 * time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// do sends one request. body may be nil, a string or []byte (sent verbatim,
// the exact JSON the bash suite sent) or any value (JSON-encoded). hdr are
// "Name: value" pairs; Content-Type defaults to application/json when there
// is a body. A transport error yields code 0 (curl's 000) and is logged.
func (a *apiClient) do(method, path string, body any, hdr ...string) (int, []byte) {
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	case []byte:
		rd = bytes.NewReader(b)
	default:
		enc, err := json.Marshal(b)
		if err != nil {
			panic(err)
		}
		rd = bytes.NewReader(enc)
	}
	a.h.step(method + " " + path)
	req, err := http.NewRequest(method, a.h.Panel+path, rd)
	if err != nil {
		a.h.root.Logf("API %s %s: %v", method, path, err)
		return 0, nil
	}
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, kv := range hdr {
		k, v, _ := strings.Cut(kv, ":")
		req.Header.Set(strings.TrimSpace(k), strings.TrimSpace(v))
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		a.h.root.Logf("API %s %s: %v", method, path, err)
		return 0, nil
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func (a *apiClient) get(path string) (int, []byte)            { return a.do("GET", path, nil) }
func (a *apiClient) post(path string, body any) (int, []byte) { return a.do("POST", path, body) }
func (a *apiClient) put(path string, body any) (int, []byte)  { return a.do("PUT", path, body) }
func (a *apiClient) del(path string) (int, []byte)            { return a.do("DELETE", path, nil) }

// code is `curl -o /dev/null -w '%{http_code}'` as a string ("000" on a
// transport error), the form the bash checks compare.
func code(c int, _ []byte) string {
	if c == 0 {
		return "000"
	}
	return strconv.Itoa(c)
}

func (a *apiClient) login() bool {
	c, _ := a.post("/api/login", map[string]string{"email": a.email, "password": a.pw})
	return c == http.StatusOK && a.jar.Len() > 0
}

// relogin signs in again when the session no longer answers (after the API
// restarts in R-0.7, as the bash suite did there, and before cleanups).
func (a *apiClient) relogin() {
	if c, _ := a.get("/api/sites"); c == http.StatusUnauthorized {
		a.login()
	}
}

// siteInfo is the part of a site the suite reads.
type siteInfo struct {
	ID         int64    `json:"id"`
	Domain     string   `json:"domain"`
	Type       string   `json:"type"`
	Status     string   `json:"status"`
	SystemUser string   `json:"system_user"`
	StagingOf  int64    `json:"staging_of"`
	Aliases    []string `json:"aliases"`
	Config     struct {
		ObjectCache  bool `json:"object_cache"`
		SFTPEnabled  bool `json:"sftp_enabled"`
		SFTPKeysOnly bool `json:"sftp_keys_only"`
		Database     struct {
			Name string `json:"name"`
		} `json:"database"`
		Backups struct {
			Enabled       bool   `json:"enabled"`
			Schedule      string `json:"schedule"`
			RetentionDays int    `json:"retention_days"`
		} `json:"backups"`
	} `json:"config"`
	DesiredGeneration int64 `json:"desired_generation"`
	AppliedGeneration int64 `json:"applied_generation"`
}

func (a *apiClient) site(id int64) siteInfo {
	var s siteInfo
	if id == 0 {
		return s
	}
	_, b := a.get(fmt.Sprintf("/api/sites/%d", id))
	_ = json.Unmarshal(b, &s)
	return s
}

func (a *apiClient) sites() ([]siteInfo, []byte) {
	var s []siteInfo
	_, b := a.get("/api/sites")
	_ = json.Unmarshal(b, &s)
	return s, b
}

// stagingOf is the staging site of production site id (zero if none).
func (a *apiClient) stagingOf(id int64) siteInfo {
	all, _ := a.sites()
	for _, s := range all {
		if s.StagingOf == id {
			return s
		}
	}
	return siteInfo{}
}

// createSite posts a site and returns its id (0 when refused). Every
// created site is also deleted at the end of the run if a check left it.
func (a *apiClient) createSite(body string) int64 {
	_, b := a.post("/api/sites", body)
	var r struct {
		Site struct {
			ID int64 `json:"id"`
		} `json:"site"`
	}
	_ = json.Unmarshal(b, &r)
	if r.Site.ID != 0 {
		id := r.Site.ID
		a.h.undo(fmt.Sprintf("delete site %d", id), func() {
			a.relogin()
			if c, _ := a.get(fmt.Sprintf("/api/sites/%d", id)); c == http.StatusNotFound {
				return
			}
			a.h.root.Logf("cleanup: deleting site %d left by the run", id)
			a.deleteSite(id)
			a.teardownGone(id)
		})
	}
	return r.Site.ID
}

// taskID reads {"task":{"id":N}} (0 when absent).
func taskID(b []byte) int64 {
	var r struct {
		Task struct {
			ID int64 `json:"id"`
		} `json:"task"`
	}
	_ = json.Unmarshal(b, &r)
	return r.Task.ID
}

// jsonField reads one top-level string field of a JSON object ("" if absent).
func jsonField(b []byte, key string) string {
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return ""
	}
	switch v := m[key].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatInt(int64(v), 10)
	}
	return ""
}

// pollEvery is how often the waits below ask the API again. The waits
// return as soon as the state is reached, so a shorter interval only cuts
// the time lost after it (it was 3 to 4 s: about 2 s on each of the ~150
// waits of a run); the timeouts are unchanged.
const pollEvery = time.Second

// waitActive polls a site until it is active or in error: "active",
// "error" or "timeout".
func (a *apiClient) waitActive(id int64, timeout time.Duration) string {
	if id == 0 {
		return "timeout"
	}
	for end := time.Now().Add(timeout); time.Now().Before(end); time.Sleep(pollEvery) {
		// A site whose provisioning failed may be rolled back, record and
		// all: gone is an error, not something to wait out.
		c, b := a.get(fmt.Sprintf("/api/sites/%d", id))
		if c == http.StatusNotFound {
			return "error"
		}
		var s siteInfo
		_ = json.Unmarshal(b, &s)
		switch s.Status {
		case "active":
			return "active"
		case "error":
			return "error"
		}
	}
	return "timeout"
}

// waitTask polls a task until it ends: "succeeded", "failed" or "timeout".
func (a *apiClient) waitTask(id int64, timeout time.Duration) string {
	if id == 0 {
		return "timeout"
	}
	for end := time.Now().Add(timeout); time.Now().Before(end); time.Sleep(pollEvery) {
		_, b := a.get(fmt.Sprintf("/api/tasks/%d", id))
		switch st := jsonField(b, "status"); st {
		case "succeeded", "failed":
			return st
		}
	}
	return "timeout"
}

func (a *apiClient) deleteSite(id int64) string {
	return code(a.del(fmt.Sprintf("/api/sites/%d", id)))
}

// teardownGone polls until the site 404s: "yes" or "no" after 60 s.
func (a *apiClient) teardownGone(id int64) string {
	for end := time.Now().Add(60 * time.Second); time.Now().Before(end); time.Sleep(pollEvery) {
		if c, _ := a.get(fmt.Sprintf("/api/sites/%d", id)); c == http.StatusNotFound {
			return "yes"
		}
	}
	return "no"
}

// ------------------------------------------------------------ site client --

// req is one HTTPS request to a hosted site, the port of
// `curl -sk --resolve <host>:443:<addr> …`: TLS is not verified, redirects
// are not followed, nothing is decompressed and no Accept-Encoding is sent
// unless the caller sets one.
type req struct {
	URL     string            // https://host/path
	Addr    string            // host:port actually dialled ("" = 127.0.0.1:443)
	Method  string            // default GET, POST when Form is set
	Form    string            // urlencoded body, as curl -d
	Header  map[string]string // extra request headers
	User    string            // basic auth "user:password"
	Jar     http.CookieJar
	Timeout time.Duration // default 60 s
}

type resp struct {
	Code     int
	Header   http.Header
	Body     []byte
	Err      error
	Duration time.Duration
}

// CodeStr is the status as curl's %{http_code} prints it.
func (r resp) CodeStr() string {
	if r.Code == 0 {
		return "000"
	}
	return strconv.Itoa(r.Code)
}

func (h *harness) fetch(q req) resp {
	method := q.Method
	if method == "" {
		method = "GET"
		if q.Form != "" {
			method = "POST"
		}
	}
	h.step(method + " " + q.URL)
	var body io.Reader
	if q.Form != "" {
		body = strings.NewReader(q.Form)
	}
	r, err := http.NewRequest(method, q.URL, body)
	if err != nil {
		return resp{Err: err}
	}
	if q.Form != "" {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range q.Header {
		r.Header.Set(k, v)
	}
	if q.User != "" {
		u, p, _ := strings.Cut(q.User, ":")
		r.SetBasicAuth(u, p)
	}
	addr := q.Addr
	if addr == "" {
		addr = "127.0.0.1:443"
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", addr)
		},
		TLSClientConfig:    &tls.Config{InsecureSkipVerify: true, ServerName: r.URL.Hostname()},
		ForceAttemptHTTP2:  true,
		DisableKeepAlives:  true,
		DisableCompression: true,
	}
	defer tr.CloseIdleConnections()
	timeout := q.Timeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	hc := &http.Client{Transport: tr, Jar: q.Jar, Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	start := time.Now()
	res, err := hc.Do(r)
	if err != nil {
		return resp{Err: err, Duration: time.Since(start)}
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	return resp{Code: res.StatusCode, Header: res.Header, Body: b, Err: err, Duration: time.Since(start)}
}

// get fetches https://dom<path> through 127.0.0.1 with optional headers
// given as name, value pairs.
func (h *harness) get(dom, path string, hdr ...string) resp {
	m := map[string]string{}
	for i := 0; i+1 < len(hdr); i += 2 {
		m[hdr[i]] = hdr[i+1]
	}
	return h.fetch(req{URL: "https://" + dom + path, Header: m})
}

// serves is the bash suite's serves(): the status and whether the body
// shows a PHP fatal ("yes"/"no").
func (h *harness) serves(dom, path string) (string, string) {
	r := h.get(dom, path)
	fatal := "no"
	if HasPHPFatal(r.Body) {
		fatal = "yes"
	}
	return r.CodeStr(), fatal
}

// cacheStatus is status_of(): the X-CloudGround-Cache word for one request.
func (h *harness) cacheStatus(dom, acceptEncoding, path string) string {
	return CacheStatus(h.get(dom, path, "Accept-Encoding", acceptEncoding).Header)
}

// ---------------------------------------------------------------- shell --

type result struct {
	Out, Err string
	Code     int // -1 when the command could not start
}

func (r result) OK() bool { return r.Code == 0 }

// Trim is the output as $(…) gives it: trailing newlines removed.
func (r result) Trim() string { return strings.TrimRight(r.Out, "\n") }

func (r result) String() string {
	return fmt.Sprintf("exit %d, stdout %q, stderr %q", r.Code, clip(r.Out), clip(r.Err))
}

func clip(s string) string {
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return s
}

// run executes a command and returns its output; it never fails the test
// by itself, the checks decide.
func (h *harness) run(name string, args ...string) result { return h.runIn("", name, args...) }

func (h *harness) runIn(stdin, name string, args ...string) result {
	h.step(name + " " + strings.Join(args, " "))
	cmd := exec.Command(name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	r := result{Out: out.String(), Err: errb.String()}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		r.Code = ee.ExitCode()
	default:
		r.Code, r.Err = -1, err.Error()
	}
	return r
}

// as runs a command as a site user with sudo, as the bash suite did.
func (h *harness) as(u string, args ...string) result {
	return h.run("sudo", append([]string{"-u", u}, args...)...)
}

// wp runs wp-cli as the site user on a release path.
func (h *harness) wp(u, path string, args ...string) result {
	return h.as(u, append([]string{"wp", "--path=" + path}, args...)...)
}

// ---------------------------------------------------------------- files --

func lookupIDs(name string) (int, int, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, 0, err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return uid, gid, nil
}

// writeAs is `printf … > path; chown u:u path`, without following a link a
// site may have planted at path: the old file is removed, the new one is
// created O_EXCL|O_NOFOLLOW and re-owned through its descriptor.
func (h *harness) writeAs(path, content, owner string, mode os.FileMode) error {
	h.step("write " + path + " as " + owner)
	uid, gid, err := lookupIDs(owner)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, mode)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		return err
	}
	if err := f.Chmod(mode); err != nil {
		return err
	}
	return f.Chown(uid, gid)
}

// mustWriteAs logs a fixture write that failed; the check that depends on
// it then fails with its own message, as in the bash suite.
func (h *harness) mustWriteAs(t *testing.T, path, content, owner string) {
	t.Helper()
	if err := h.writeAs(path, content, owner, 0o644); err != nil {
		t.Logf("fixture: writing %s as %s: %v", path, owner, err)
	}
}

func (h *harness) remove(paths ...string) {
	for _, p := range paths {
		h.step("rm " + p)
		_ = os.RemoveAll(p)
	}
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func readTrim(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(b), "\n")
}

// statOwnerMode is `stat -c '%U %a'`.
func statOwnerMode(p string) (string, string) {
	var st syscall.Stat_t
	if err := syscall.Stat(p, &st); err != nil {
		return "", ""
	}
	name := strconv.Itoa(int(st.Uid))
	if u, err := user.LookupId(name); err == nil {
		name = u.Username
	}
	return name, strconv.FormatUint(uint64(st.Mode&0o7777), 8)
}

func inode(p string) string {
	var st syscall.Stat_t
	if err := syscall.Stat(p, &st); err != nil {
		return ""
	}
	return strconv.FormatUint(uint64(st.Ino), 10)
}
