package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	if err != nil || !inv.wait || inv.req.path != "/api/backups/3/restore" || inv.req.body.(map[string]string)["mode"] != "files" {
		t.Fatalf("%+v %v", inv, err)
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
