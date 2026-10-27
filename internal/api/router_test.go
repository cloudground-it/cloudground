package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// The router's own tests (SPEC.md §1.11). They state their expectations
// independently of router.go: the roles each policy admits, the messages of
// its refusals, the enrolment routes, and which loader a path needs.

// Roles a request is made as. operator-other is an operator asking for a
// site, or a site's backup, release or cron job, assigned to someone else;
// admin-2fa is an admin the 2FA requirement holds back.
const (
	asAnonymous     = "anonymous"
	asReadonly      = "readonly"
	asOperatorOwn   = "operator-own"
	asOperatorOther = "operator-other"
	asAdmin         = "admin"
	asAdmin2FA      = "admin-2fa"
)

// admitted is who each policy lets through, as the docs promise.
var admitted = map[policy]map[string]bool{
	policyOpen:     {asAnonymous: true, asReadonly: true, asOperatorOwn: true, asOperatorOther: true, asAdmin: true, asAdmin2FA: true},
	policySignedIn: {asReadonly: true, asOperatorOwn: true, asOperatorOther: true, asAdmin: true, asAdmin2FA: true},
	policyGlobal:   {asReadonly: true, asAdmin: true, asAdmin2FA: true},
	policyManage:   {asOperatorOwn: true, asOperatorOther: true, asAdmin: true, asAdmin2FA: true},
	policyAdmin:    {asAdmin: true, asAdmin2FA: true},
	// Quarry's routes take its own session, which none of these roles
	// holds (studio_routes_test.go tests them with one).
	policyStudio:     {},
	policyStudioData: {},
}

var refusal = map[policy]string{
	policyGlobal: "server-wide access required",
	policyManage: "site management access required",
	policyAdmin:  "this is a read-only account",
}

const (
	msgSignIn = "authentication required"
	msg2FA    = "two-factor authentication is required for admin accounts: enrol in Security before continuing"
)

// enrolment is every route an admin held by the 2FA requirement can still
// reach, so it can enrol (or leave).
var enrolment = map[string]bool{
	"POST /api/logout":              true,
	"POST /api/account/2fa/begin":   true,
	"POST /api/account/2fa/confirm": true,
	// The enrolment page lists the account's sessions (W3: it got 403).
	"GET /api/account/sessions": true,
}

// sessionOnly is every route an API token may not reach (403): the ones
// that manage accounts, sessions, second factors, users and tokens, so a
// leaked token cannot mint another, create an admin or reset a password;
// a site's shell and routing; and the panel's own firewall, certificate
// and binary.
var sessionOnly = map[string]bool{
	"POST /api/logout":                  true,
	"POST /api/account/password":        true,
	"POST /api/account/2fa/begin":       true,
	"POST /api/account/2fa/confirm":     true,
	"POST /api/account/2fa/disable":     true,
	"GET /api/account/sessions":         true,
	"DELETE /api/account/sessions/{id}": true,
	"PUT /api/account/profile":          true,
	"POST /api/account/whats-new":       true,
	"POST /api/users":                   true,
	"PUT /api/users/{id}":               true,
	"DELETE /api/users/{id}":            true,
	"POST /api/users/{id}/password":     true,
	"POST /api/settings/backup-ssh-key": true,
	"GET /api/tokens":                   true,
	"POST /api/tokens":                  true,
	"DELETE /api/tokens/{id}":           true,
	"POST /api/sites/{id}/ssh-shell":    true,
	"GET /api/sites/{id}/routing":       true,
	"PUT /api/sites/{id}/routing":       true,
	"POST /api/firewall/rule":           true,
	"POST /api/firewall/enable":         true,
	"POST /api/blocklist/blocks":        true,
	"DELETE /api/blocklist/blocks":      true,
	"POST /api/blocklist/allowed":       true,
	"DELETE /api/blocklist/allowed":     true,
	"PUT /api/blocklist/settings":       true,
	"POST /api/panel/certificate":       true,
	"POST /api/panel/update":            true,
	"POST /api/system/updates/apply":    true,
	// An API token cannot open Quarry.
	"POST /api/sites/{id}/quarry/handoff": true,
}

// loaderFor is the loader a path's {id} needs: anything under a site, a
// backup, a release or a cron job must be checked against the caller.
func loaderFor(path string) loader {
	switch {
	case strings.HasPrefix(path, "/api/sites/{id}"):
		return loadSite
	case strings.HasPrefix(path, "/api/backups/{id}"):
		return loadBackup
	case strings.HasPrefix(path, "/api/deployments/{id}"):
		return loadDeployment
	case strings.HasPrefix(path, "/api/cron/{id}"):
		return loadCron
	}
	return loadNone
}

func TestRouteTableIsWellFormed(t *testing.T) {
	s := &Server{}
	seen := map[string]bool{}
	for _, rt := range s.routeTable() {
		p := rt.pattern()
		if err := rt.valid(); err != nil {
			t.Errorf("%s: %v", p, err)
		}
		if seen[p] {
			t.Errorf("%s is in the table twice", p)
		}
		seen[p] = true
		if _, ok := admitted[rt.Policy]; !ok && rt.Policy != policyToken {
			t.Errorf("%s: policy %d is not one the tests know", p, rt.Policy)
		}
		if want := loaderFor(rt.Path); rt.Load != want {
			t.Errorf("%s: loader %q, want %q: the caller's reach is not checked", p, rt.Load.noun(), want.noun())
		}
		if rt.Enrol != enrolment[p] {
			t.Errorf("%s: Enrol is %v, want %v", p, rt.Enrol, enrolment[p])
		}
		if rt.SessionOnly != sessionOnly[p] {
			t.Errorf("%s: SessionOnly is %v, want %v", p, rt.SessionOnly, sessionOnly[p])
		}
		if rt.Section == "" || rt.Doc == "" {
			t.Errorf("%s: no section or description for docs/api.md", p)
		}
		if rt.Loopback != (p == "POST /api/connector/purge" || p == "GET /healthz") {
			t.Errorf("%s: Loopback is %v", p, rt.Loopback)
		}
	}
	for p := range enrolment {
		if !seen[p] {
			t.Errorf("enrolment route %s is not in the table", p)
		}
	}
	for p := range sessionOnly {
		if !seen[p] {
			t.Errorf("session-only route %s is not in the table", p)
		}
	}
	// A row without a policy cannot be registered.
	for _, bad := range []route{
		{Method: "GET", Path: "/api/x", Handle: plain(s.handleHealthz)},
		{Method: "GET", Path: "/api/x", Policy: policyOpen},
		{Method: "GET", Path: "/api/x", Policy: policySignedIn, Load: loadSite, Handle: plain(s.handleHealthz)},
		{Method: "GET", Path: "/api/x/{id}", Policy: policyOpen, Load: loadSite, Handle: plain(s.handleHealthz)},
		{Method: "GET", Path: "/api/x", Policy: policyOpen, SessionOnly: true, Handle: plain(s.handleHealthz)},
	} {
		if bad.valid() == nil {
			t.Errorf("%+v passed validation", bad)
		}
	}
}

// Routes registers the table and nothing else; ConnectorRoutes only its
// loopback rows. And no file of the package registers a handler itself.
func TestRoutesComeOnlyFromTheTable(t *testing.T) {
	s, _, _ := testServer(t)
	var want []string
	for _, rt := range s.routeTable() {
		want = append(want, rt.pattern())
	}
	if fmt.Sprint(s.routePatterns) != fmt.Sprint(want) {
		t.Errorf("registered %v\nwant the table %v", s.routePatterns, want)
	}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") || name == "router.go" {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "HandleFunc", "Handle", "NewServeMux":
				t.Errorf("%s: %s outside the route table (router.go)", fset.Position(sel.Pos()), sel.Sel.Name)
			}
			return true
		})
	}

	// The loopback listener: the connector and the probe, nothing else.
	ls := httptest.NewServer(s.ConnectorRoutes())
	defer ls.Close()
	for _, rt := range s.routeTable() {
		if rt.Loopback || rt.Method == "" {
			continue
		}
		path := strings.NewReplacer("{id}", "1", "{name}", "x", "{fingerprint}", "x", "{table}", "x", "{dir}", "x", "{db}", "x").Replace(rt.Path)
		req, _ := http.NewRequest(rt.Method, ls.URL+path, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("loopback listener answered %s with %d", rt.pattern(), resp.StatusCode)
		}
	}
}

// fixture is one server set up for a role: the caller's own site and one
// assigned to nobody, each with a backup, a release and a cron job.
type fixture struct {
	s          *Server
	url        string
	user       int64 // 0: anonymous
	own, other struct{ site, backup, deployment, cron int64 }
}

func newFixture(t *testing.T, role string) *fixture {
	t.Helper()
	s, _, ts := testServer(t)
	setupAdmin(t, s, ts)
	s.Store.UpdatePanelSettings(func(p *state.PanelSettings) error {
		p.UpdateCheck = false
		p.Require2FAAdmin = role == asAdmin2FA
		return nil
	})
	f := &fixture{s: s, url: ts.URL}
	for i, ids := range []*struct{ site, backup, deployment, cron int64 }{&f.own, &f.other} {
		domain := fmt.Sprintf("site%d.example.com", i)
		site, err := s.Store.CreateSite(state.Site{
			Domain: domain, Type: state.SiteStatic, Profile: state.ProfileBalanced, Engine: state.EngineNginx,
			SystemUser: fmt.Sprintf("cg-site-%d", i+1), RootPath: "/srv/sites/" + domain, Status: state.SiteActive,
		})
		if err != nil {
			t.Fatal(err)
		}
		b, err := s.Store.CreateBackup(state.Backup{SiteID: site.ID, SnapshotID: "snap", Repository: "/r", Kind: "full"})
		if err != nil {
			t.Fatal(err)
		}
		d, err := s.Store.CreateDeployment(state.Deployment{SiteID: site.ID, ReleaseID: "20260101-000000", Path: "/p", Status: state.DeployCreated})
		if err != nil {
			t.Fatal(err)
		}
		j, err := s.Store.CreateCronJob(state.CronJob{SiteID: site.ID, Schedule: "0 * * * *", Command: "true", Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		*ids = struct{ site, backup, deployment, cron int64 }{site.ID, b.ID, d.ID, j.ID}
	}
	hash, _ := hashPassword("unused-password")
	switch role {
	case asAdmin, asAdmin2FA:
		u, _ := s.Store.GetUserByEmail("admin@example.com")
		f.user = u.ID
	case asReadonly:
		u, _ := s.Store.CreateUser("readonly@example.com", hash, "readonly")
		f.user = u.ID
	case asOperatorOwn, asOperatorOther:
		u, _ := s.Store.CreateUser("operator@example.com", hash, "operator")
		if err := s.Store.SetUserSites(u.ID, []int64{f.own.site}); err != nil {
			t.Fatal(err)
		}
		f.user = u.ID
	}
	return f
}

// path fills a pattern's wildcards, with the target's resources for the
// loaded {id}.
func (f *fixture) path(rt route, role string) string {
	ids := f.own
	if role == asOperatorOther {
		ids = f.other
	}
	id := "999999"
	switch rt.Load {
	case loadSite:
		id = strconv.FormatInt(ids.site, 10)
	case loadBackup:
		id = strconv.FormatInt(ids.backup, 10)
	case loadDeployment:
		id = strconv.FormatInt(ids.deployment, 10)
	case loadCron:
		id = strconv.FormatInt(ids.cron, 10)
	}
	return strings.NewReplacer("{id}", id, "{name}", "nginx", "{fingerprint}", "SHA256:none",
		"{table}", "wp_options", "{dir}", "exports", "{db}", "site_1").Replace(rt.Path)
}

// Every row of the table, for every role, is refused exactly as its policy
// and loader say, and otherwise not refused by the router.
func TestRouteTableMatrix(t *testing.T) {
	routes := (&Server{}).routeTable()
	// Destructive rows last: the site they delete is the one the others use.
	sort.SliceStable(routes, func(i, j int) bool {
		return routes[i].Method != http.MethodDelete && routes[j].Method == http.MethodDelete
	})
	for _, role := range []string{asAnonymous, asReadonly, asOperatorOwn, asOperatorOther, asAdmin2FA, asAdmin} {
		t.Run(role, func(t *testing.T) {
			f := newFixture(t, role)
			for _, rt := range routes {
				if rt.Policy == policyToken {
					continue
				}
				method := rt.Method
				if method == "" {
					method = http.MethodGet
				}
				cookie := ""
				if f.user != 0 {
					cookie = randomToken(32)
					if err := f.s.Store.CreateSession(cookie, f.user, time.Hour); err != nil {
						t.Fatal(err)
					}
				}
				status, msg := probe(t, f.url, method, f.path(rt, role), cookie)
				wantStatus, wantMsg := expectedRefusal(rt, role)
				switch {
				case wantStatus != 0 && (status != wantStatus || msg != wantMsg):
					t.Errorf("%s %s: %d %q, want %d %q", role, rt.pattern(), status, msg, wantStatus, wantMsg)
				case wantStatus == 0 && routerRefusal(rt, status, msg):
					t.Errorf("%s %s: refused (%d %q); policy %q", role, rt.pattern(), status, msg, rt.Policy.marker())
				}
			}
			waitForTasks(t, f.s)
		})
	}
}

// probe makes one request and reads the error of any refusal (an allowed
// stream, /api/events, is never read: it does not end).
func probe(t *testing.T, base, method, path, cookie string) (int, string) {
	t.Helper()
	var body io.Reader
	if method != http.MethodGet {
		body = strings.NewReader("{}")
	}
	req, err := http.NewRequest(method, base+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: insecureSessionName, Value: cookie})
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var e struct {
		Error string `json:"error"`
	}
	if resp.StatusCode >= 400 {
		json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&e)
	}
	return resp.StatusCode, e.Error
}

// expectedRefusal is the refusal a role gets from the router on rt, or 0.
func expectedRefusal(rt route, role string) (int, string) {
	if rt.Policy == policyOpen {
		return 0, ""
	}
	if rt.Policy.studio() {
		return http.StatusUnauthorized, msgStudioSignIn
	}
	if role == asAnonymous {
		return http.StatusUnauthorized, msgSignIn
	}
	if role == asAdmin2FA && !enrolment[rt.pattern()] {
		return http.StatusForbidden, msg2FA
	}
	if !admitted[rt.Policy][role] {
		if rt.Policy == policyAdmin && (role == asOperatorOwn || role == asOperatorOther) {
			return http.StatusForbidden, operatorRefusal(rt.AdminOnly)
		}
		return http.StatusForbidden, refusal[rt.Policy]
	}
	if role == asOperatorOther && rt.Load != loadNone {
		return http.StatusNotFound, rt.Load.noun() + " not found"
	}
	return 0, ""
}

// routerRefusal reports whether a response is one of the router's refusals.
func routerRefusal(rt route, status int, msg string) bool {
	switch status {
	case http.StatusUnauthorized:
		return msg == msgSignIn || msg == msgStudioSignIn
	case http.StatusForbidden:
		return msg == msg2FA || msg == refusal[policyGlobal] || msg == refusal[policyManage] || msg == refusal[policyAdmin] ||
			(rt.Policy == policyAdmin && msg == operatorRefusal(rt.AdminOnly))
	case http.StatusNotFound:
		// The loader's refusal; the delete rows run last and may find their
		// site already removed by the delete before them.
		return rt.Load != loadNone && rt.Method != http.MethodDelete && msg == rt.Load.noun()+" not found"
	}
	return false
}

// S-H4 on every row: a write from another site is refused before anything
// else, with or without a session, and must be JSON (or a raw upload where
// the row allows one). The connector's token row is exempt.
func TestRouteTableCrossSiteRules(t *testing.T) {
	s, _, ts := testServer(t)
	send := func(method, path, ctype string, hdr map[string]string) (int, string) {
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader("{}"))
		req.Header.Set("Content-Type", ctype)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var e struct {
			Error string `json:"error"`
		}
		json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&e)
		return resp.StatusCode, e.Error
	}
	for _, rt := range s.routeTable() {
		if rt.Method == "" || rt.Method == http.MethodGet {
			if rt.SameOriginRead {
				if code, _ := send(http.MethodGet, rt.Path, "", map[string]string{"Sec-Fetch-Site": "cross-site"}); code != http.StatusForbidden {
					t.Errorf("%s from another site: %d, want 403", rt.pattern(), code)
				}
			}
			continue
		}
		path := strings.NewReplacer("{id}", "1", "{name}", "x", "{fingerprint}", "x", "{table}", "x", "{dir}", "x", "{db}", "x").Replace(rt.Path)
		for _, hdr := range []map[string]string{{"Sec-Fetch-Site": "same-site"}, {"Origin": "https://staging.example.com"}, {"Origin": "null"}} {
			code, msg := send(rt.Method, path, "application/json", hdr)
			if rt.Policy == policyToken {
				if code == http.StatusForbidden && strings.Contains(msg, "refused") {
					t.Errorf("%s with %v refused, but a token route is exempt", rt.pattern(), hdr)
				}
				continue
			}
			if code != http.StatusForbidden {
				t.Errorf("%s with %v: %d %q, want 403", rt.pattern(), hdr, code, msg)
			}
		}
		if rt.Policy == policyToken {
			continue
		}
		if code, _ := send(rt.Method, path, "text/plain", nil); code != http.StatusUnsupportedMediaType {
			t.Errorf("%s as text/plain: %d, want 415", rt.pattern(), code)
		}
		code, _ := send(rt.Method, path, "application/octet-stream", nil)
		if rt.Body == bodyUpload && code == http.StatusUnsupportedMediaType {
			t.Errorf("%s refused a raw upload", rt.pattern())
		}
		if rt.Body != bodyUpload && code != http.StatusUnsupportedMediaType {
			t.Errorf("%s took application/octet-stream: %d", rt.pattern(), code)
		}
	}
}

// A row's body limit is what the handler can read.
func TestRouteBodyLimits(t *testing.T) {
	s, _, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	big := func(n int) []byte { return bytes.Repeat([]byte("a"), n) }

	// Sign-in: 8 KiB of credentials, no more.
	body := append(append([]byte(`{"email":"x@example.com","password":"`), big(9<<10)...), `"}`...)
	resp, err := http.Post(ts.URL+"/api/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a 9 KiB sign-in: %d, want 400", resp.StatusCode)
	}

	// A Quarry chunk: 8 MiB.
	site, err := s.Store.CreateSite(state.Site{
		Domain: "limits.example.com", Type: state.SiteWordPress, Profile: state.ProfileBalanced, Engine: state.EngineNginx,
		SystemUser: "cg-site-9", RootPath: "/srv/sites/limits.example.com", Status: state.SiteActive,
		Config: state.SiteConfig{Database: state.DatabaseConfig{Enabled: true, Name: "wp_limits", User: "wp_limits"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.AddSiteDatabase(site.ID, "wp_limits"); err != nil {
		t.Fatal(err)
	}
	studio := openStudio(t, c, site.ID)
	url := ts.URL + "/api/quarry/import/upload?name=a.sql&offset=0"
	for _, tc := range []struct {
		size int
		want int
	}{{uploadChunk + 1, http.StatusRequestEntityTooLarge}, {1 << 10, http.StatusOK}} {
		req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(big(tc.size)))
		req.Header.Set("Content-Type", "application/octet-stream")
		for _, ck := range studio.cookies {
			req.AddCookie(ck)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("a %d-byte chunk: %d, want %d", tc.size, resp.StatusCode, tc.want)
		}
	}
}

func TestErrorReply(t *testing.T) {
	agentErr := func(code agentrpc.Code) error {
		return fmt.Errorf("wrapped: %w", &agentrpc.Error{Code: code, Message: "safe text", Detail: "/root/secret path"})
	}
	for _, tc := range []struct {
		name       string
		err        error
		def        int
		wantStatus int
		wantMsg    string
	}{
		{"invalid", agentErr(agentrpc.CodeInvalid), 0, 400, "safe text"},
		{"not found", agentErr(agentrpc.CodeNotFound), 0, 404, "safe text"},
		{"conflict", agentErr(agentrpc.CodeConflict), 0, 409, "safe text"},
		{"timeout", agentErr(agentrpc.CodeTimeout), 0, 504, "safe text"},
		{"unavailable", agentErr(agentrpc.CodeUnavailable), 0, 503, "safe text"},
		{"internal", agentErr(agentrpc.CodeInternal), 0, 502, "safe text"},
		{"internal, own default", agentErr(agentrpc.CodeInternal), 500, 500, "safe text"},
		{"store not found", fmt.Errorf("x: %w", state.ErrNotFound), 0, 404, "fallback"},
		{"body too large", &http.MaxBytesError{Limit: 1}, 0, 413, "request body too large"},
		{"anything else", errors.New("dial unix /run/x: no such file"), 0, 502, "fallback"},
	} {
		status, msg := errorReply(tc.err, tc.def, "fallback")
		if status != tc.wantStatus || msg != tc.wantMsg {
			t.Errorf("%s: %d %q, want %d %q", tc.name, status, msg, tc.wantStatus, tc.wantMsg)
		}
	}
}

// docs/api.md is what the generator writes from the table.
func TestAPIDocIsCurrent(t *testing.T) {
	want, err := Reference()
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile("../../docs/api.md")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Error("docs/api.md is stale: run `go generate ./internal/api` (make docker-api-doc)")
	}
	for _, rt := range (&Server{}).routeTable() {
		if !bytes.Contains(want, []byte("| `"+rt.Path)) && rt.Path != "/" {
			t.Errorf("%s is not in the reference", rt.pattern())
		}
	}
}

// Every body limit is pinned here, independently of the table: dropping a
// row's MaxBody falls back to the 4 MiB default, which no other test (and
// a regenerated docs/api.md) would notice.
func TestRouteBodyLimitsArePinned(t *testing.T) {
	want := map[string]int64{
		"POST /api/setup":                              smallBody,
		"POST /api/login":                              smallBody,
		"POST /api/account/password":                   smallBody,
		"POST /api/account/2fa/begin":                  smallBody,
		"POST /api/account/2fa/disable":                smallBody,
		"PUT /api/account/profile":                     smallBody,
		"POST /api/account/whats-new":                  smallBody,
		"POST /api/users":                              smallBody,
		"POST /api/users/{id}/password":                smallBody,
		"POST /api/backups/{id}/archive":               smallBody,
		"POST /api/tokens":                             smallBody,
		"PUT /api/sites/{id}/backups/policy":           smallBody,
		"POST /api/blocklist/blocks":                   smallBody,
		"DELETE /api/blocklist/blocks":                 smallBody,
		"POST /api/blocklist/allowed":                  smallBody,
		"DELETE /api/blocklist/allowed":                smallBody,
		"PUT /api/blocklist/settings":                  smallBody,
		"PUT /api/sites/{id}/blocking":                 blockingBody,
		"PUT /api/sites/{id}/dev-mode":                 smallBody,
		"PUT /api/sites/{id}/maintenance":              smallBody,
		"PUT /api/sites/{id}/wp/cleanup":               smallBody,
		"POST /api/sites/{id}/protected-paths":         smallBody,
		"PUT /api/sites/{id}/wp/two-factor":            smallBody,
		"PUT /api/sites/{id}/wp/hardening":             smallBody,
		"PUT /api/sites/{id}/wp/updates":               smallBody,
		"DELETE /api/sites/{id}/protected-paths":       smallBody,
		"PUT /api/sites/{id}/uptime":                   smallBody,
		"POST /api/system/updates/apply":               smallBody,
		"PUT /api/sites/{id}/mail":                     smallBody,
		"POST /api/sites/{id}/mail/test":               smallBody,
		"POST /api/settings/mail/test":                 smallBody,
		"PUT /api/sites/{id}/certificate/custom":       certBody,
		"PUT /api/sites/{id}/routing":                  routingBody,
		"POST /api/sites/{id}/files/upload":            fileUploadBody,
		"POST /api/quarry/db/{db}/tables/{table}/edit": dbEditBody,
		"POST /api/quarry/import/upload":               dbChunkBody,
		"POST /api/quarry/login":                       smallBody,
		"POST /api/quarry/handoff":                     smallBody,
		"POST /api/sites/{id}/quarry/handoff":          smallBody,
	}
	s := &Server{}
	for _, rt := range s.routeTable() {
		got := rt.MaxBody
		if got == 0 {
			got = defaultMaxBody
		}
		exp, ok := want[rt.pattern()]
		if !ok {
			exp = defaultMaxBody
		}
		if got != exp {
			t.Errorf("%s: body limit %d, want %d", rt.pattern(), got, exp)
		}
		delete(want, rt.pattern())
	}
	for p := range want {
		t.Errorf("%s: pinned here but not in the route table", p)
	}
}

// An operator cannot create sites by design: it is told so, not that its
// account is read-only (it is not). A read-only account keeps its message.
func TestAdminRouteRefusalNamesTheRole(t *testing.T) {
	var create route
	for _, rt := range (&Server{}).routeTable() {
		if rt.pattern() == "POST /api/sites" {
			create = rt
		}
	}
	if ok, msg := create.Policy.admits(roleOperator, create.AdminOnly); ok || msg != "only administrators can create sites" {
		t.Fatalf("operator: %v %q", ok, msg)
	}
	if ok, msg := create.Policy.admits(roleReadonly, create.AdminOnly); ok || msg != "this is a read-only account" {
		t.Fatalf("read-only: %v %q", ok, msg)
	}
	if ok, msg := policyAdmin.admits(roleOperator, ""); ok || msg != "only administrators can do this" {
		t.Fatalf("operator, unnamed row: %v %q", ok, msg)
	}
}
