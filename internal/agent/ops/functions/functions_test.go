package functions

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	fn "github.com/cloudground-it/cloudground/internal/functions"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

type env struct {
	*Ops
	root     string
	systemd  *host.FakeSystemd
	accounts *host.FakeAccounts
	nginx    *host.FakeNginx
	acme     *host.FakeACME
	runs     []host.Service
	runErr   error
}

// The tests run as root in the container (make docker-test): the trees'
// owners are real.
const fnUID, fnGID = 4242, 4242

func newEnv(t *testing.T) *env {
	t.Helper()
	if os.Getuid() != 0 {
		t.Skip("needs root: the function's tree is root's with the function's group")
	}
	root := t.TempDir()
	mk := func(p string) string {
		d := filepath.Join(root, p)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		return d
	}
	sd := host.NewFakeSystemd()
	clock := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	e := &env{root: root, systemd: sd, accounts: host.NewFakeAccounts(), nginx: &host.FakeNginx{}, acme: &host.FakeACME{}}
	e.Ops = &Ops{
		Systemd: sd, Accounts: e.accounts, Nginx: e.nginx, ACME: e.acme,
		LookupOwner: func(string) (sitefs.Owner, error) { return sitefs.Owner{UID: fnUID, GID: fnGID}, nil },
		OpenSite: func(st state.Site) (*sitefs.Root, error) {
			return sitefs.Open(st.RootPath, sitefs.Owner{UID: 0, GID: 0})
		},
		Run: func(_ context.Context, svc host.Service, _ string) (string, error) {
			e.runs = append(e.runs, svc)
			if e.runErr != nil {
				return "npm ERR! nope", e.runErr
			}
			return "added 1 package", nil
		},
		Installed: func(rt, v string) bool { return rt == "node" && v == "22" },
		NginxMu:   &sync.Mutex{}, CertMu: &sync.Mutex{},
		Root: filepath.Join(root, "srv-functions"), Dir: mk("etc-fn"), LogDir: filepath.Join(root, "log-fn"),
		SystemdDir: mk("systemd"), SitesDir: mk("sites-enabled"), CertLiveDir: mk("live"),
		FallbackCert: "/etc/cloudground/certs/fallback.pem", FallbackKey: "/etc/cloudground/certs/fallback.key",
		ACMEWebroot: "/var/www/cloudground-acme",
		Now:         func() time.Time { clock = clock.Add(time.Second); return clock },
	}
	sd.UnitDir = e.SystemdDir
	return e
}

func hello() state.Function {
	return fn.Defaults(state.Function{ID: 7, Name: "hello", Runtime: "node"})
}

func (e *env) set(t *testing.T, files map[string]string) rpc.FunctionResult {
	t.Helper()
	res, err := e.Set(context.Background(), rpc.SetFunctionParams{Function: hello(), Files: files, Env: map[string]string{"TOKEN": "t0p"}})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func owner(t *testing.T, p string) (int, int, os.FileMode) {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	return int(st.Uid), int(st.Gid), fi.Mode().Perm()
}

func TestSetReleasesRollbackDelete(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	first := e.set(t, nil)
	if first.Release == "" || len(first.Releases) != 1 {
		t.Fatalf("first %+v", first)
	}
	if u, ok := e.accounts.User("cg-fn-7"); !ok || !u.Spec.System || u.Spec.Shell != "/usr/sbin/nologin" {
		t.Fatalf("the function's user: %+v %v", u, ok)
	}
	// The tree: /srv/functions root's 0711; the function's directory and
	// releases/ root's with its group; a release its user's.
	if uid, _, mode := owner(t, e.Root); uid != 0 || mode != 0o711 {
		t.Errorf("root dir %d %o", uid, mode)
	}
	dir := filepath.Join(e.Root, "hello")
	for _, d := range []string{dir, filepath.Join(dir, "releases")} {
		if uid, gid, mode := owner(t, d); uid != 0 || gid != fnGID || mode != 0o750 {
			t.Errorf("%s: %d:%d %o", d, uid, gid, mode)
		}
	}
	rel := filepath.Join(dir, "releases", first.Release)
	if uid, gid, _ := owner(t, filepath.Join(rel, "index.js")); uid != fnUID || gid != fnGID {
		t.Errorf("code file %d:%d", uid, gid)
	}
	if uid, _, _ := owner(t, filepath.Join(dir, "current")); uid != 0 {
		t.Errorf("current is %d's", uid)
	}
	code, err := e.Code(ctx, rpc.FunctionRef{ID: 7, Name: "hello"})
	if err != nil || !strings.Contains(code.Files["index.js"], "exports.handler") || len(code.Files) != 1 {
		t.Fatalf("starter code %+v %v", code, err)
	}
	for _, f := range []string{fn.SocketUnit(7), fn.ServiceTemplate(7), fn.ParentSlice} {
		if _, err := os.Stat(filepath.Join(e.SystemdDir, f)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	if !e.systemd.Did("start", fn.SocketUnit(7)) && !e.systemd.Did("restart", fn.SocketUnit(7)) {
		t.Fatalf("socket not started: %v", e.systemd.Ops())
	}
	if uid, _, mode := owner(t, e.envPath(7)); uid != 0 || mode != 0o600 {
		t.Fatalf("env %d %o", uid, mode)
	}
	if env, _ := os.ReadFile(e.envPath(7)); !strings.Contains(string(env), `TOKEN="t0p"`) {
		t.Fatalf("env %q", env)
	}
	for _, shim := range []string{"fn.mjs", "fn.py"} {
		if _, _, mode := owner(t, filepath.Join(e.Dir, "shim", shim)); mode != 0o644 {
			t.Fatalf("shim %s: %o", shim, mode)
		}
	}
	if len(e.runs) != 0 {
		t.Fatal("a dependency step ran without a package.json")
	}
	// A setting without code keeps the release.
	if again := e.set(t, nil); again.Release != first.Release {
		t.Fatal("a change of settings made a release")
	}
	// Several files and a package.json: the dependency step runs as the
	// function's user, in its slice, with the release bound read-write.
	files := map[string]string{"index.js": "exports.handler = () => require('./lib/v.js');", "lib/v.js": "module.exports = 'v2';", "package.json": `{"dependencies":{}}`}
	second := e.set(t, files)
	if second.Release == first.Release || len(second.Releases) != 2 || second.Releases[0] != second.Release || second.Output != "added 1 package" {
		t.Fatalf("second %+v", second)
	}
	if len(e.runs) != 1 || e.runs[0].User != "cg-fn-7" || e.runs[0].Slice != "cg-fn-7.slice" ||
		!strings.Contains(strings.Join(e.runs[0].Props, "\n"), "BindPaths=/srv/functions/hello/releases/"+second.Release) ||
		e.runs[0].Dir != filepath.Join(e.Root, "hello", "releases", second.Release) {
		t.Fatalf("dependency step %+v", e.runs)
	}
	code, _ = e.Code(ctx, rpc.FunctionRef{ID: 7, Name: "hello"})
	if len(code.Files) != 3 || code.Files["lib/v.js"] != files["lib/v.js"] {
		t.Fatalf("code %+v", code.Files)
	}
	// A failed dependency step leaves the live release where it was.
	e.runErr = agentrpc.Invalid("installing the function's dependencies failed: npm ERR! nope")
	if res, err := e.Set(ctx, rpc.SetFunctionParams{Function: hello(), Files: files, Env: map[string]string{}}); err == nil || res.Output == "" {
		t.Fatalf("a failed step taken: %+v %v", res, err)
	}
	e.runErr = nil
	if code, _ := e.Code(ctx, rpc.FunctionRef{ID: 7, Name: "hello"}); code.Release != second.Release || len(code.Releases) != 2 {
		t.Fatalf("after a failed step: %+v", code)
	}
	back, err := e.Rollback(ctx, rpc.FunctionRollbackParams{ID: 7, Name: "hello"})
	if err != nil || back.Release != first.Release {
		t.Fatalf("rollback %+v %v", back, err)
	}
	if _, err := e.Rollback(ctx, rpc.FunctionRollbackParams{ID: 7, Name: "hello"}); agentrpc.CodeOf(err) != agentrpc.CodeConflict {
		t.Fatalf("nothing older: %v", err)
	}
	if fwd, err := e.Rollback(ctx, rpc.FunctionRollbackParams{ID: 7, Name: "hello", Release: second.Release}); err != nil || fwd.Release != second.Release {
		t.Fatalf("to a named release %+v %v", fwd, err)
	}
	// Only the newest ten are kept.
	for i := range 12 {
		e.set(t, map[string]string{"index.js": "exports.handler = () => 'v" + string(rune('a'+i)) + "';"})
	}
	if code, _ := e.Code(ctx, rpc.FunctionRef{ID: 7, Name: "hello"}); len(code.Releases) != fn.KeepReleases {
		t.Fatalf("%d releases kept", len(code.Releases))
	}
	// A runtime the server lacks; a path that leaves the release.
	if _, err := e.Set(ctx, rpc.SetFunctionParams{Function: fn.Defaults(state.Function{ID: 8, Name: "py", Runtime: "python"})}); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
		t.Fatalf("python taken: %v", err)
	}
	if _, err := e.Set(ctx, rpc.SetFunctionParams{Function: hello(), Files: map[string]string{"index.js": "x", "../escape.js": "x"}}); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
		t.Fatalf("an escaping path taken: %v", err)
	}

	// Its logs and lineage go with it.
	os.WriteFile(e.accessLog("hello"), []byte("x\n"), 0o640)
	e.acme.CertOnly(ctx, host.CertRequest{Name: fn.FunctionLineage(7), Domains: []string{"api.example.com"}, Email: "a@example.com", Webroot: "/w"})
	if err := e.Delete(ctx, 7, "hello"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(e.SystemdDir, fn.SocketUnit(7)), filepath.Join(e.SystemdDir, fn.ServiceTemplate(7)), e.envPath(7), dir, e.accessLog("hello")} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s outlived the function", p)
		}
	}
	if _, ok := e.accounts.User("cg-fn-7"); ok || e.accounts.HasGroup("cg-fn-7") {
		t.Error("the function's user outlived it")
	}
	if err := e.acme.Delete(ctx, fn.FunctionLineage(7)); !errors.Is(err, host.ErrNoSuchCert) {
		t.Errorf("the lineage outlived the function: %v", err)
	}
	// Deleting again is not an error.
	if err := e.Delete(ctx, 7, "hello"); err != nil {
		t.Fatal(err)
	}
}

func TestInvokeLogsSummary(t *testing.T) {
	e := newEnv(t)
	var got *http.Request
	var gotBody string
	// nginx's internal server: plain HTTP on a unix socket, no TLS.
	sock := filepath.Join(t.TempDir(), "nginx.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(201)
		w.Write([]byte("echo:" + string(b)))
	}))
	srv.Listener = ln
	srv.Start()
	defer srv.Close()
	e.NginxSocket = sock
	res, err := e.Invoke(context.Background(), rpc.InvokeFunctionParams{Name: "hello", Source: "test", Body: "x", TimeoutSec: 5})
	if err != nil || res.Status != 201 || res.Body != "echo:x" {
		t.Fatalf("invoke %+v %v", res, err)
	}
	if got.Host != "cloudground-functions" || got.Method != "POST" || got.URL.Path != "/hello" || got.Header.Get("X-CloudGround-Source") != "test" || got.UserAgent() != "CloudGround-Functions/test" {
		t.Fatalf("request %+v", got)
	}
	if _, err := e.Invoke(context.Background(), rpc.InvokeFunctionParams{Name: "hello", Source: "schedule", Method: "GET", Path: "/sub?x=1", TimeoutSec: 5}); err != nil {
		t.Fatal(err)
	}
	if got.Method != "GET" || got.URL.Path != "/hello/sub" || got.URL.RawQuery != "x=1" || gotBody != "" {
		t.Fatalf("request %s %s?%s %q", got.Method, got.URL.Path, got.URL.RawQuery, gotBody)
	}

	os.MkdirAll(e.LogDir, 0o750)
	access := `203.0.113.9 - fn.example.com [27/Oct/2026:10:00:00 +0000] "GET /hello/x?a=1 HTTP/2.0" 200 12 0.042 cache:- "Mozilla/5.0"
127.0.0.1 - cloudground-functions [27/Oct/2026:10:01:00 +0000] "POST /hello HTTP/1.1" 502 0 1.500 cache:- "CloudGround-Functions/schedule"
`
	os.WriteFile(e.accessLog("hello"), []byte(access), 0o644)
	os.WriteFile(e.outputLog("hello"), []byte("{\"status\":200}\nhello from the handler\n"), 0o644)
	logs, err := e.Logs(context.Background(), rpc.FunctionRef{ID: 7, Name: "hello"})
	if err != nil || len(logs.Invocations) != 2 || len(logs.Output) != 2 {
		t.Fatalf("logs %+v %v", logs, err)
	}
	if first := logs.Invocations[0]; first.Status != 502 || first.Source != "schedule" || first.MS != 1500 || first.Path != "/hello" {
		t.Fatalf("newest %+v", first)
	}
	if second := logs.Invocations[1]; second.Source != "http" || second.MS != 42 || second.Path != "/hello/x" {
		t.Fatalf("oldest %+v", second)
	}
	sum, err := e.Summary(context.Background(), rpc.FunctionsSummaryParams{Names: []string{"hello", "none"}})
	if err != nil || len(sum.Last) != 1 || sum.Last["hello"].Status != 502 {
		t.Fatalf("summary %+v %v", sum, err)
	}
}

func TestRender(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tok, _ := fn.NewToken()
	p := rpc.RenderFunctionsParams{Domain: "fn.example.com", Functions: []rpc.FunctionRoute{
		{ID: 7, Name: "hello", TimeoutSec: 10, Domains: []string{"api.example.com"}},
		{ID: 8, Name: "hook", TimeoutSec: 10, Token: tok},
	}}
	// A lineage certbot has is served; one it has not, the fallback.
	live := filepath.Join(e.CertLiveDir, fn.FunctionLineage(7))
	os.MkdirAll(live, 0o755)
	os.WriteFile(filepath.Join(live, "fullchain.pem"), []byte("c"), 0o644)
	os.WriteFile(filepath.Join(live, "privkey.pem"), []byte("k"), 0o600)
	stale := filepath.Join(e.SitesDir, "cloudground-fn-99.conf")
	os.WriteFile(stale, []byte("old"), 0o600)
	other := filepath.Join(e.SitesDir, "example.org.conf")
	os.WriteFile(other, []byte("a site"), 0o644)
	res, err := e.Render(ctx, p)
	if err != nil || len(res.Files) != 3 {
		t.Fatalf("render %+v %v", res, err)
	}
	for _, name := range []string{"cloudground-functions.conf", "cloudground-functions-internal.conf", "cloudground-fn-7.conf"} {
		uid, _, mode := owner(t, filepath.Join(e.SitesDir, name))
		if uid != 0 || mode != 0o600 {
			t.Errorf("%s: %d %o", name, uid, mode)
		}
	}
	domain, _ := os.ReadFile(filepath.Join(e.SitesDir, "cloudground-functions.conf"))
	if !strings.Contains(string(domain), `"Bearer `+tok+`"`) || !strings.Contains(string(domain), "ssl_certificate /etc/cloudground/certs/fallback.pem;") {
		t.Errorf("domain file:\n%s", domain)
	}
	own, _ := os.ReadFile(filepath.Join(e.SitesDir, "cloudground-fn-7.conf"))
	if !strings.Contains(string(own), "ssl_certificate "+filepath.Join(live, "fullchain.pem")+";") {
		t.Errorf("own file:\n%s", own)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("a stale function file was left")
	}
	if b, _ := os.ReadFile(other); string(b) != "a site" {
		t.Error("a site's file was touched")
	}
	if tests, reloads := e.nginx.Counts(); tests != 1 || reloads != 1 {
		t.Fatalf("tests %d reloads %d", tests, reloads)
	}
	// Nothing changed: no reload.
	if _, err := e.Render(ctx, p); err != nil {
		t.Fatal(err)
	}
	if tests, reloads := e.nginx.Counts(); tests != 1 || reloads != 1 {
		t.Fatalf("an unchanged render reloaded: %d %d", tests, reloads)
	}
	// nginx refuses: everything is put back.
	e.nginx.TestErr = errors.New("emerg")
	if _, err := e.Render(ctx, rpc.RenderFunctionsParams{}); err == nil {
		t.Fatal("a refused render taken")
	}
	if b, _ := os.ReadFile(filepath.Join(e.SitesDir, "cloudground-functions.conf")); string(b) != string(domain) {
		t.Error("the domain file was not put back")
	}
	e.nginx.TestErr = nil
	// Neither a domain nor a function: no file.
	if res, err := e.Render(ctx, rpc.RenderFunctionsParams{}); err != nil || len(res.Files) != 0 {
		t.Fatalf("empty render %+v %v", res, err)
	}
	if ents, _ := os.ReadDir(e.SitesDir); len(ents) != 1 {
		t.Fatalf("files left: %v", ents)
	}
	// A refused token never reaches nginx.
	if _, err := e.Render(ctx, rpc.RenderFunctionsParams{Functions: []rpc.FunctionRoute{{ID: 1, Name: "x", TimeoutSec: 1, Token: "bad token"}}}); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
		t.Fatalf("a bad token: %v", err)
	}
}

func TestCertificates(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	var pre []string
	e.Preflight = func(_ context.Context, d []string) error { pre = d; return nil }
	p := rpc.FunctionsCertParams{Name: fn.DomainLineage, Domains: []string{"fn.example.com"}, Email: "ops@example.com"}
	if err := e.IssueCertificate(ctx, p); err != nil {
		t.Fatal(err)
	}
	if len(pre) != 1 {
		t.Fatal("no preflight")
	}
	if _, reloads := e.nginx.Counts(); reloads != 1 {
		t.Fatal("no reload after the issuance")
	}
	if err := e.IssueCertificate(ctx, rpc.FunctionsCertParams{Name: fn.DomainLineage, Email: "ops@example.com"}); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
		t.Fatalf("no domain: %v", err)
	}
	if err := e.deleteCert(ctx, fn.DomainLineage); err != nil {
		t.Fatal(err)
	}
	if err := e.deleteCert(ctx, fn.DomainLineage); err != nil {
		t.Fatalf("a second delete: %v", err)
	}
}

func TestMigrate(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	s := state.Site{ID: 5, Domain: "old.example.com", SystemUser: "cg-site-5", RootPath: filepath.Join(e.root, "site")}
	// A v0.8.x function: two releases, current the older one, its units
	// and its environment file.
	base := filepath.Join(s.RootPath, "functions", "hello")
	for i, id := range []string{"01JA0000000000000000000001", "01JA0000000000000000000002"} {
		d := filepath.Join(base, "releases", id)
		os.MkdirAll(d, 0o750)
		os.WriteFile(filepath.Join(d, "index.js"), []byte("exports.handler = () => 'v"+string(rune('1'+i))+"';"), 0o640)
	}
	os.Symlink("releases/01JA0000000000000000000001", filepath.Join(base, "current"))
	legacy := fn.LegacyUnitBase(s.SystemUser, "hello")
	os.WriteFile(filepath.Join(e.SystemdDir, legacy+".socket"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(e.SystemdDir, legacy+"@.service"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(e.Dir, "cg-site-5-hello.env"), []byte("A=1"), 0o600)

	f := fn.Defaults(state.Function{ID: 9, Name: "hello", Runtime: "node"})
	res, err := e.Migrate(ctx, rpc.MigrateFunctionParams{Site: s, OldName: "hello", Function: f, Env: map[string]string{"A": "1"}})
	if err != nil || res.Release != "01JA0000000000000000000001" || len(res.Releases) != 2 {
		t.Fatalf("migrate %+v %v", res, err)
	}
	code, err := e.Code(ctx, rpc.FunctionRef{ID: 9, Name: "hello"})
	if err != nil || code.Files["index.js"] != "exports.handler = () => 'v1';" {
		t.Fatalf("code %+v %v", code, err)
	}
	if uid, gid, _ := owner(t, filepath.Join(e.Root, "hello", "releases", res.Release, "index.js")); uid != fnUID || gid != fnGID {
		t.Errorf("a copied file is %d:%d", uid, gid)
	}
	for _, p := range []string{filepath.Join(e.SystemdDir, legacy+".socket"), filepath.Join(e.SystemdDir, legacy+"@.service"), filepath.Join(e.Dir, "cg-site-5-hello.env"), base} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s outlived the migration", p)
		}
	}
	if _, err := os.Stat(filepath.Join(e.SystemdDir, fn.SocketUnit(9))); err != nil {
		t.Errorf("the new socket: %v", err)
	}
	// A function that already has code is never overwritten nor undone.
	os.MkdirAll(filepath.Join(base, "releases", "01JA0000000000000000000003"), 0o750)
	os.Symlink("releases/01JA0000000000000000000003", filepath.Join(base, "current"))
	if _, err := e.Migrate(ctx, rpc.MigrateFunctionParams{Site: s, OldName: "hello", Function: f, Env: map[string]string{}}); agentrpc.CodeOf(err) != agentrpc.CodeConflict {
		t.Fatalf("over an existing function: %v", err)
	}
	if code, _ := e.Code(ctx, rpc.FunctionRef{ID: 9, Name: "hello"}); code.Release != res.Release {
		t.Fatal("the existing function was touched")
	}
}
