package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/config"
)

const testToken = "cgt_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// fakePanel answers like the API: bearer auth, tasks that end after a few
// polls with the given outcome.
type fakePanel struct {
	mu       sync.Mutex
	outcome  string // final status of started tasks
	polls    int
	logins   int
	lastBody map[string]any
	lastPath string
}

func (f *fakePanel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/api/login" {
		f.logins++
		http.SetCookie(w, &http.Cookie{Name: "cloudground_session", Value: "s"})
		w.Write([]byte(`{}`))
		return
	}
	_, noCookie := r.Cookie("cloudground_session")
	authz := r.Header.Get("Authorization")
	if authz != "Bearer "+testToken && !(authz == "" && noCookie == nil) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid or revoked API token"}`))
		return
	}
	f.lastPath = r.Method + " " + r.URL.Path
	f.lastBody = nil
	json.NewDecoder(r.Body).Decode(&f.lastBody)
	switch {
	case r.URL.Path == "/api/sites/7/warm" || r.URL.Path == "/api/sites/7/database/import":
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{"task":{"id":42,"kind":"x","status":"pending"}}`)
	case r.URL.Path == "/api/tasks/42":
		f.polls++
		status, errText := "running", ""
		if f.polls >= 3 {
			status = f.outcome
			if status == "failed" {
				errText = "the import file was not found"
			}
		}
		fmt.Fprintf(w, `{"id":42,"kind":"database.import","status":%q,"progress":%d,"message":"step %d","error":%q}`, status, f.polls*30, f.polls, errText)
	case r.URL.Path == "/api/tasks/43":
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"task not found"}`)
	case r.URL.Path == "/api/sites":
		fmt.Fprint(w, `[]`)
	default:
		fmt.Fprint(w, `{"ok":true}`)
	}
}

func runCLI(t *testing.T, f *fakePanel, cfg config.Config, args ...string) (int, string, string) {
	t.Helper()
	ts := httptest.NewServer(f)
	t.Cleanup(ts.Close)
	cfg.URL = ts.URL
	inv, err := parseArgs(args)
	if err != nil {
		return exitUsage, "", err.Error()
	}
	var out, errb bytes.Buffer
	code := execute(&cfg, inv, &out, &errb)
	return code, out.String(), errb.String()
}

func init() { pollEvery = time.Millisecond }

func TestTokenIsSentAndNoLoginHappens(t *testing.T) {
	f := &fakePanel{}
	code, out, errs := runCLI(t, f, config.Config{Token: testToken}, "sites", "list")
	if code != exitOK || strings.TrimSpace(out) != "[]" || f.logins != 0 {
		t.Fatalf("code %d out %q err %q logins %d", code, out, errs, f.logins)
	}
}

func TestWaitSucceeded(t *testing.T) {
	f := &fakePanel{outcome: "succeeded"}
	code, out, errs := runCLI(t, f, config.Config{Token: testToken}, "--wait", "warm", "7")
	if code != exitOK || f.polls != 3 || !strings.Contains(out, `"succeeded"`) || !strings.Contains(errs, "step 2") {
		t.Fatalf("code %d polls %d\nout %s\nerr %s", code, f.polls, out, errs)
	}
}

func TestWaitFailedExitsNonZero(t *testing.T) {
	f := &fakePanel{outcome: "failed"}
	code, _, errs := runCLI(t, f, config.Config{Token: testToken}, "database", "import", "7", "example.com", "missing.sql", "--wait")
	if code != exitFailed || !strings.Contains(errs, "the import file was not found") {
		t.Fatalf("code %d err %s", code, errs)
	}
	if f.lastBody["confirm"] != nil { // the last request was the poll
		t.Fatalf("unexpected body %v", f.lastBody)
	}
}

func TestWaitTimeout(t *testing.T) {
	f := &fakePanel{outcome: "running"}
	code, _, errs := runCLI(t, f, config.Config{Token: testToken}, "--wait", "--timeout=50ms", "warm", "7")
	if code != exitTimeout || !strings.Contains(errs, "still running") {
		t.Fatalf("code %d err %s", code, errs)
	}
}

func TestWaitOnAnExistingTaskAndAMissingOne(t *testing.T) {
	f := &fakePanel{outcome: "succeeded"}
	if code, _, errs := runCLI(t, f, config.Config{Token: testToken}, "-w", "task", "42"); code != exitOK {
		t.Fatalf("code %d %s", code, errs)
	}
	if code, _, _ := runCLI(t, f, config.Config{Token: testToken}, "-w", "task", "43"); code != exitFailed {
		t.Fatalf("a missing task: %d", code)
	}
}

func TestWithoutWaitTheTaskIsOnlyStarted(t *testing.T) {
	f := &fakePanel{outcome: "failed"}
	code, out, _ := runCLI(t, f, config.Config{Token: testToken}, "warm", "7")
	if code != exitOK || f.polls != 0 || !strings.Contains(out, `"id": 42`) {
		t.Fatalf("code %d polls %d out %s", code, f.polls, out)
	}
}

func TestRefusedTokenExitsNonZero(t *testing.T) {
	f := &fakePanel{}
	code, _, errs := runCLI(t, f, config.Config{Token: "cgt_" + strings.Repeat("B", 43)}, "sites", "list")
	if code != exitFailed || !strings.Contains(errs, "HTTP 401: invalid or revoked API token") {
		t.Fatalf("code %d err %s", code, errs)
	}
}

func TestPasswordFallbackAndTokenCreate(t *testing.T) {
	f := &fakePanel{}
	cfg := config.Config{Email: "a@example.com", Password: "pw"}
	if code, _, errs := runCLI(t, f, cfg, "sites", "list"); code != exitOK || f.logins != 1 {
		t.Fatalf("password fallback: %d %s logins %d", code, errs, f.logins)
	}
	// tokens create signs in with the password even when a token is set.
	cfg.Token = testToken
	if code, _, errs := runCLI(t, f, cfg, "tokens", "create", "ci", "30"); code != exitOK || f.logins != 2 ||
		f.lastPath != "POST /api/tokens" || f.lastBody["name"] != "ci" || f.lastBody["expires_in_days"] != float64(30) {
		t.Fatalf("tokens create: %d %s logins %d %s %v", code, errs, f.logins, f.lastPath, f.lastBody)
	}
	if code, _, errs := runCLI(t, f, config.Config{Token: testToken}, "tokens", "create", "ci"); code != exitFailed || !strings.Contains(errs, "password") {
		t.Fatalf("tokens create without a password: %d %s", code, errs)
	}
	if code, _, errs := runCLI(t, f, config.Config{}, "sites", "list"); code != exitFailed || !strings.Contains(errs, "CLOUDGROUND_TOKEN") {
		t.Fatalf("no credentials: %d %s", code, errs)
	}
}

func TestParseArgs(t *testing.T) {
	inv, err := parseArgs([]string{"backup", "restore", "3", "example.com", "--wait", "files"})
	if err != nil || !inv.wait || inv.req.path != "/api/backups/3/restore" || inv.req.body.(map[string]any)["mode"] != "files" {
		t.Fatalf("%+v %v", inv, err)
	}
	if _, ok := inv.req.body.(map[string]any)["databases"]; ok {
		t.Fatalf("a restore without --database names databases: %+v", inv.req.body)
	}
	// #15: --database, repeatable, anywhere after the command, in either
	// spelling, restores only those databases.
	inv, err = parseArgs([]string{"backup", "restore", "--database", "site_1", "3", "example.com", "database", "--database=site_1_logs"})
	if err != nil {
		t.Fatal(err)
	}
	body := inv.req.body.(map[string]any)
	if inv.req.path != "/api/backups/3/restore" || body["confirm"] != "example.com" || body["mode"] != "database" ||
		!slices.Equal(body["databases"].([]string), []string{"site_1", "site_1_logs"}) {
		t.Fatalf("restore with --database: %s %+v", inv.req.path, body)
	}
	for _, bad := range [][]string{
		{"backup", "restore", "3", "example.com", "--database"},
		{"backup", "restore", "3", "example.com", "--database="},
		{"backup", "restore", "3", "example.com", "--databases", "x"},
		{"backup", "restore", "3", "--database", "x"},
		{"backup", "restore", "3", "example.com", "full", "extra"},
		{"backup", "restore", "../3", "example.com"},
	} {
		if _, err := parseArgs(bad); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
	inv, err = parseArgs([]string{"purge", "4", "https://example.com/a/", "https://example.com/b/"})
	if err != nil || inv.req.path != "/api/sites/4/purge" || len(inv.req.body.(map[string]any)["urls"].([]string)) != 2 {
		t.Fatalf("%+v %v", inv, err)
	}
	for _, bad := range [][]string{
		{}, {"nope"}, {"sites"}, {"sites", "delete"}, {"sites", "delete", "../users"}, {"sites", "delete", "1", "2"},
		{"--timeout", "soon", "status"}, {"sites", "create", "a.com", "wordpress", "{not json"}, {"tokens", "create", "x", "ten"},
	} {
		if _, err := parseArgs(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if inv, err := parseArgs([]string{"sites", "create", "a.com", "wordpress", `{"title":"T","domain":"evil.com"}`}); err != nil ||
		inv.req.body.(map[string]any)["domain"] != "a.com" {
		t.Fatalf("flags-json must not override the domain: %+v %v", inv.req, err)
	}
}

func TestStartedTask(t *testing.T) {
	cases := map[string]int64{
		`{"task":{"id":5}}`:                      5,
		`{"site":{"id":9},"task":{"id":6}}`:      6,
		`{"job":{"id":7}}`:                       7,
		`{"id":8,"kind":"x","status":"running"}`: 8,
		`{"id":9}`:                               0,
		`[]`:                                     0,
	}
	for body, want := range cases {
		if got, _ := startedTask([]byte(body)); got != want {
			t.Errorf("%s → %d, want %d", body, got, want)
		}
	}
}

// #47: the SFTP commands map to POST /sftp; the password comes from stdin,
// the key from a file.
func TestSFTPCommands(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "id.pub")
	if err := os.WriteFile(keyFile, []byte("ssh-ed25519 AAAA dev@laptop\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	inv, err := parseArgs([]string{"sftp", "keys-only", "7", keyFile})
	if err != nil {
		t.Fatal(err)
	}
	b := inv.req.body.(map[string]any)
	if inv.req.method != "POST" || inv.req.path != "/api/sites/7/sftp" || b["enable"] != true || b["keys_only"] != true || b["public_key"] != "ssh-ed25519 AAAA dev@laptop" {
		t.Fatalf("keys-only: %+v", inv.req)
	}
	if inv, err = parseArgs([]string{"sftp", "keys-only", "7"}); err != nil || inv.req.body.(map[string]any)["public_key"] != nil {
		t.Fatalf("keys-only without a key: %+v %v", inv.req, err)
	}

	old := siteCreateStdin
	t.Cleanup(func() { siteCreateStdin = old })
	siteCreateStdin = strings.NewReader("a-long-sftp-password\n")
	inv, err = parseArgs([]string{"sftp", "password", "7"})
	if err != nil {
		t.Fatal(err)
	}
	b = inv.req.body.(map[string]any)
	if b["password"] != "a-long-sftp-password" || b["keys_only"] != false || b["enable"] != true {
		t.Fatalf("password: %+v", inv.req)
	}
	siteCreateStdin = strings.NewReader("")
	if _, err := parseArgs([]string{"sftp", "password", "7"}); err == nil {
		t.Error("sftp password with nothing on stdin accepted")
	}

	if inv, err = parseArgs([]string{"sftp", "off", "7"}); err != nil || inv.req.body.(map[string]any)["enable"] != false {
		t.Fatalf("off: %+v %v", inv.req, err)
	}
	if inv, err = parseArgs([]string{"sftp", "keys", "7"}); err != nil || inv.req.method != "GET" || inv.req.path != "/api/sites/7/ssh-keys" {
		t.Fatalf("keys: %+v %v", inv.req, err)
	}
	for _, bad := range [][]string{
		{"sftp", "keys-only", "x"}, {"sftp", "keys-only", "7", filepath.Join(t.TempDir(), "missing.pub")},
		{"sftp", "password", "7", "inline-password"}, {"sftp", "off"},
	} {
		if _, err := parseArgs(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// #26: protect add takes the password from stdin, never from argv, and
// protect remove names the path in the body.
func TestProtectPaths(t *testing.T) {
	old := siteCreateStdin
	t.Cleanup(func() { siteCreateStdin = old })
	siteCreateStdin = strings.NewReader("s3cret pass\n")
	inv, err := parseArgs([]string{"protect", "add", "5", "/wp-admin", "boss"})
	if err != nil || inv.req.method != "POST" || inv.req.path != "/api/sites/5/protected-paths" {
		t.Fatalf("%+v %v", inv.req, err)
	}
	if b := inv.req.body.(map[string]string); b["path"] != "/wp-admin" || b["username"] != "boss" || b["password"] != "s3cret pass" {
		t.Fatalf("body %v", b)
	}
	siteCreateStdin = strings.NewReader("")
	if _, err := parseArgs([]string{"protect", "add", "5", "/wp-admin", "boss"}); err == nil {
		t.Fatal("an empty stdin was accepted")
	}
	inv, err = parseArgs([]string{"protect", "remove", "5", "/wp-admin"})
	if err != nil || inv.req.method != "DELETE" || inv.req.path != "/api/sites/5/protected-paths" || inv.req.body.(map[string]string)["path"] != "/wp-admin" {
		t.Fatalf("%+v %v", inv.req, err)
	}
	if _, err := parseArgs([]string{"protect", "add", "x", "/a", "u"}); err == nil {
		t.Fatal("a site id that is not a number was accepted")
	}
}

// #21: the block list's commands put the address in the body, sign in with
// the password for a change, and refuse a duration that is not a number.
func TestBlocklistCommands(t *testing.T) {
	inv, err := parseArgs([]string{"blocklist"})
	if err != nil || inv.req.method != "GET" || inv.req.path != "/api/blocklist" {
		t.Fatalf("%+v %v", inv.req, err)
	}
	inv, err = parseArgs([]string{"blocklist", "block", "203.0.113.0/24", "30"})
	if err != nil || inv.req.method != "POST" || inv.req.path != "/api/blocklist/blocks" || !inv.cmd.password {
		t.Fatalf("%+v %v", inv.req, err)
	}
	if b := inv.req.body.(map[string]any); b["address"] != "203.0.113.0/24" || b["minutes"] != 30 {
		t.Fatalf("body %v", b)
	}
	for words, want := range map[string]string{
		"unblock":  "DELETE /api/blocklist/blocks",
		"allow":    "POST /api/blocklist/allowed",
		"disallow": "DELETE /api/blocklist/allowed",
	} {
		inv, err := parseArgs([]string{"blocklist", words, "198.51.100.7"})
		if err != nil || inv.req.method+" "+inv.req.path != want || inv.req.body.(map[string]any)["address"] != "198.51.100.7" {
			t.Errorf("%s: %+v %v", words, inv.req, err)
		}
	}
	if _, err := parseArgs([]string{"blocklist", "block", "203.0.113.9", "soon"}); err == nil {
		t.Fatal("a duration that is not a number was accepted")
	}
}

// #25: a site's rules go as the JSON given; JSON that does not parse is a
// usage error.
func TestSitesBlock(t *testing.T) {
	inv, err := parseArgs([]string{"sites", "block", "7", `{"countries":["CN"]}`})
	if err != nil || inv.req.method != "PUT" || inv.req.path != "/api/sites/7/blocking" {
		t.Fatalf("%+v %v", inv.req, err)
	}
	if b := inv.req.body.(map[string]any); fmt.Sprint(b["countries"]) != "[CN]" {
		t.Fatalf("body %v", b)
	}
	if _, err := parseArgs([]string{"sites", "block", "7", "{nope"}); err == nil {
		t.Fatal("bad JSON accepted")
	}
}

// #80: development mode takes 15, 30 or 60 minutes, or off.
func TestDevMode(t *testing.T) {
	inv, err := parseArgs([]string{"devmode", "7", "30"})
	if err != nil || inv.req.method != "PUT" || inv.req.path != "/api/sites/7/dev-mode" || fmt.Sprint(inv.req.body) != "map[minutes:30]" {
		t.Fatalf("%+v %v", inv.req, err)
	}
	if inv, err = parseArgs([]string{"devmode", "7", "off"}); err != nil || fmt.Sprint(inv.req.body) != "map[minutes:0]" {
		t.Fatalf("%+v %v", inv.req, err)
	}
	if _, err := parseArgs([]string{"devmode", "7", "20"}); err == nil {
		t.Fatal("20 minutes accepted")
	}
}
