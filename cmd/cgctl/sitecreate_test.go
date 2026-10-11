package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/config"
)

const createdAnswer = `{"site":{"id":13,"domain":"blog.example.com","type":"wordpress"},"task":{"id":42,"kind":"site.create","status":"pending"},
"summary":{"site_url":"https://blog.example.com/","panel_path":"/sites/13","panel_url":"https://panel.example.com:8443/sites/13",
"wordpress":{"admin_url":"https://blog.example.com/wp-admin/","admin_user":"owner"},
"database":{"host":"127.0.0.1","port":3306,"socket":"/run/mysqld/mysqld.sock","name":"blog_example","user":"blog_admin","password":"the db password 1"},
"sftp":{"enabled":false,"user":"cg-site-13"}}}`

// createPanel answers POST /api/sites with createdAnswer and the task as
// succeeded at once.
func createPanel(t *testing.T, got *map[string]any) string {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sites":
			json.NewDecoder(r.Body).Decode(got)
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, createdAnswer)
		case "/api/tasks/42":
			fmt.Fprint(w, `{"id":42,"kind":"site.create","status":"succeeded","progress":100}`)
		}
	}))
	t.Cleanup(ts.Close)
	return ts.URL
}

func runCreate(t *testing.T, args ...string) (int, string, map[string]any) {
	t.Helper()
	var body map[string]any
	cfg := config.Config{Token: testToken, URL: createPanel(t, &body)}
	inv, err := parseArgs(args)
	if err != nil {
		t.Fatalf("%q: %v", args, err)
	}
	var out, errb bytes.Buffer
	code := execute(&cfg, inv, &out, &errb)
	return code, out.String(), body
}

// Fase 9b/2: the database flags reach the request, and the summary is
// printed with everything the creator needs, the password included.
func TestSitesCreateFlagsAndSummary(t *testing.T) {
	pwFile := filepath.Join(t.TempDir(), "pw")
	os.WriteFile(pwFile, []byte("the db password 1\n"), 0o600)
	code, out, body := runCreate(t, "sites", "create", "blog.example.com", "wordpress",
		`{"admin_email":"a@example.com","database_name":"from_json","title":"T"}`,
		"--db-name", "blog_example", "--db-user=blog_admin", "--db-password-file", pwFile, "--wait")
	if code != exitOK {
		t.Fatalf("code %d out %s", code, out)
	}
	if body["database_name"] != "blog_example" || body["database_user"] != "blog_admin" ||
		body["database_password"] != "the db password 1" || body["title"] != "T" || body["domain"] != "blog.example.com" {
		t.Fatalf("body %v", body)
	}
	for _, want := range []string{"Created.", "https://blog.example.com/", "https://panel.example.com:8443/sites/13",
		"https://blog.example.com/wp-admin/", "owner", "/run/mysqld/mysqld.sock", "3306", "blog_example", "blog_admin",
		"Password:    the db password 1", "Off: turn it on", "cg-site-13", "shown only now"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}
	// Without --wait it says the creation is under way; --json is the raw answer.
	_, out, body = runCreate(t, "sites", "create", "s.example.com", "static", "--with-database", "--db-password", "x y z long pass")
	if !strings.Contains(out, "Being created: task 42") || body["create_database"] != true || body["database_password"] != "x y z long pass" {
		t.Fatalf("out %s body %v", out, body)
	}
	_, out, _ = runCreate(t, "sites", "create", "s.example.com", "static", "--json")
	var raw map[string]any
	if json.Unmarshal([]byte(out), &raw) != nil || raw["summary"] == nil {
		t.Fatalf("--json printed %s", out)
	}
	for _, bad := range [][]string{
		{"sites", "create", "a.com", "static", "--db-name"},
		{"sites", "create", "a.com", "static", "--db-color", "red"},
		{"sites", "create", "a.com"},
		{"sites", "create", "a.com", "static", "{}", "extra"},
		{"sites", "create", "a.com", "static", "--db-password-file", "/nonexistent/pw"},
	} {
		if _, err := parseArgs(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// createWithStdin runs sites create with stdin and returns its exit code,
// stderr and the request body.
func createWithStdin(t *testing.T, stdin string, args ...string) (int, string, map[string]any) {
	t.Helper()
	old := siteCreateStdin
	siteCreateStdin = strings.NewReader(stdin)
	t.Cleanup(func() { siteCreateStdin = old })
	var body map[string]any
	cfg := config.Config{Token: testToken, URL: createPanel(t, &body)}
	inv, err := parseArgs(args)
	if err != nil {
		t.Fatalf("%q: %v", args, err)
	}
	var out, errb bytes.Buffer
	code := execute(&cfg, inv, &out, &errb)
	return code, errb.String(), body
}

// The password can come from stdin, out of ps and the shell history; the
// argv form still works but warns on stderr.
func TestSitesCreatePasswordFromStdin(t *testing.T) {
	code, stderr, body := createWithStdin(t, "pass with spaces 1\r\nsecond line\n",
		"sites", "create", "s.example.com", "static", "--with-database", "--db-password-stdin")
	if code != exitOK || body["database_password"] != "pass with spaces 1" || stderr != "" {
		t.Fatalf("code %d stderr %q body %v", code, stderr, body)
	}
	// No final newline is still one line.
	if _, _, body = createWithStdin(t, "no-newline", "sites", "create", "s.example.com", "static", "--db-password-stdin"); body["database_password"] != "no-newline" {
		t.Fatalf("body %v", body)
	}
	_, stderr, body = createWithStdin(t, "", "sites", "create", "s.example.com", "static", "--db-password-file", writePw(t, "from file\n"))
	if body["database_password"] != "from file" || stderr != "" {
		t.Fatalf("stderr %q body %v", stderr, body)
	}
	_, stderr, body = createWithStdin(t, "", "sites", "create", "s.example.com", "static", "--db-password", "in argv")
	if body["database_password"] != "in argv" || !strings.Contains(stderr, "cgctl: warning: --db-password puts the password where ps") ||
		!strings.Contains(stderr, "--db-password-stdin or --db-password-file") {
		t.Fatalf("stderr %q body %v", stderr, body)
	}
	pw := writePw(t, "x\n")
	for _, c := range []struct {
		stdin string
		args  []string
	}{
		{"", []string{"sites", "create", "a.com", "static", "--db-password-stdin"}},
		{"\n", []string{"sites", "create", "a.com", "static", "--db-password-stdin"}},
		{strings.Repeat("x", maxPasswordLine+1), []string{"sites", "create", "a.com", "static", "--db-password-stdin"}},
		{"pw\n", []string{"sites", "create", "a.com", "static", "--db-password-stdin=pw"}},
		{"pw\n", []string{"sites", "create", "a.com", "static", "--db-password-stdin", "--db-password", "x"}},
		{"", []string{"sites", "create", "a.com", "static", "--db-password", "x", "--db-password-file", pw}},
	} {
		old := siteCreateStdin
		siteCreateStdin = strings.NewReader(c.stdin)
		_, err := parseArgs(c.args)
		siteCreateStdin = old
		if err == nil {
			t.Errorf("stdin %.20q, %q accepted", c.stdin, c.args)
		}
	}
	if !strings.Contains(siteCreateArgs, "--db-password-stdin") {
		t.Error("the usage does not name --db-password-stdin")
	}
}

func writePw(t *testing.T, s string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
