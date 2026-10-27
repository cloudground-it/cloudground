package functions

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	root    string
	site    state.Site
	systemd *host.FakeSystemd
}

func newEnv(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	mk := func(p string) string {
		d := filepath.Join(root, p)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		return d
	}
	s := state.Site{ID: 5, Domain: "fn.example.com", SystemUser: "cg-site-5", RootPath: mk("srv/fn.example.com"), Type: state.SiteStatic}
	sd := host.NewFakeSystemd()
	clock := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	e := &env{root: root, site: s, systemd: sd, Ops: &Ops{
		Systemd: sd, SystemdDir: mk("systemd"), Dir: mk("etc-fn"), LogRoot: mk("logs"),
		Open: func(st state.Site) (*sitefs.Root, error) {
			return sitefs.Open(st.RootPath, sitefs.Owner{UID: os.Getuid(), GID: os.Getgid()})
		},
		Installed: func(rt, v string) bool { return rt == "node" && v == "22" },
		Now:       func() time.Time { clock = clock.Add(time.Second); return clock },
	}}
	sd.UnitDir = e.SystemdDir
	return e
}

func (e *env) set(t *testing.T, code string) rpc.FunctionResult {
	t.Helper()
	res, err := e.Set(context.Background(), rpc.SetFunctionParams{Site: e.site, Function: fn.Defaults(state.SiteFunction{Name: "hello", Runtime: "node"}),
		Code: code, Env: map[string]string{"TOKEN": "t0p"}})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestSetReleasesRollbackDelete(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	first := e.set(t, "")
	if first.Release == "" || len(first.Releases) != 1 {
		t.Fatalf("first %+v", first)
	}
	code, err := e.Code(ctx, rpc.FunctionRef{Site: e.site, Name: "hello"})
	if err != nil || !strings.Contains(code.Code, "exports.handler") {
		t.Fatalf("starter code %q %v", code.Code, err)
	}
	for _, f := range []string{fn.SocketUnit("cg-site-5", "hello"), fn.ServiceTemplate("cg-site-5", "hello")} {
		if _, err := os.Stat(filepath.Join(e.SystemdDir, f)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	if !e.systemd.Did("start", fn.SocketUnit("cg-site-5", "hello")) && !e.systemd.Did("restart", fn.SocketUnit("cg-site-5", "hello")) {
		t.Fatalf("socket not started: %v", e.systemd.Ops())
	}
	fi, err := os.Stat(e.envPath("cg-site-5", "hello"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("env %v %v", fi, err)
	}
	for _, shim := range []string{"fn.mjs", "fn.py"} {
		if fi, err := os.Stat(filepath.Join(e.Dir, "shim", shim)); err != nil || fi.Mode().Perm() != 0o644 {
			t.Fatalf("shim %s: %v %v", shim, fi, err)
		}
	}
	// A setting without code keeps the release.
	if again := e.set(t, ""); again.Release != first.Release {
		t.Fatal("a change of settings made a release")
	}
	second := e.set(t, "exports.handler = () => 'v2';")
	if second.Release == first.Release || len(second.Releases) != 2 || second.Releases[0] != second.Release {
		t.Fatalf("second %+v", second)
	}
	back, err := e.Rollback(ctx, rpc.FunctionRollbackParams{Site: e.site, Name: "hello"})
	if err != nil || back.Release != first.Release {
		t.Fatalf("rollback %+v %v", back, err)
	}
	if _, err := e.Rollback(ctx, rpc.FunctionRollbackParams{Site: e.site, Name: "hello"}); agentrpc.CodeOf(err) != agentrpc.CodeConflict {
		t.Fatalf("nothing older: %v", err)
	}
	if fwd, err := e.Rollback(ctx, rpc.FunctionRollbackParams{Site: e.site, Name: "hello", Release: second.Release}); err != nil || fwd.Release != second.Release {
		t.Fatalf("to a named release %+v %v", fwd, err)
	}
	// Only the newest ten are kept.
	for i := range 12 {
		e.set(t, "exports.handler = () => 'v"+string(rune('a'+i))+"';")
	}
	if code, _ := e.Code(ctx, rpc.FunctionRef{Site: e.site, Name: "hello"}); len(code.Releases) != fn.KeepReleases {
		t.Fatalf("%d releases kept", len(code.Releases))
	}
	// A runtime the server lacks.
	if _, err := e.Set(ctx, rpc.SetFunctionParams{Site: e.site, Function: fn.Defaults(state.SiteFunction{Name: "py", Runtime: "python"})}); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
		t.Fatalf("python taken: %v", err)
	}

	if err := e.Delete(ctx, e.site, "hello"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(e.SystemdDir, fn.SocketUnit("cg-site-5", "hello")), e.envPath("cg-site-5", "hello"), filepath.Join(e.site.RootPath, "functions", "hello")} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s outlived the function", p)
		}
	}
}

func TestInvokeAndLogs(t *testing.T) {
	e := newEnv(t)
	var got *http.Request
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		b, _ := io.ReadAll(r.Body)
		w.WriteHeader(201)
		w.Write([]byte("echo:" + string(b)))
	}))
	defer srv.Close()
	e.NginxAddr = strings.TrimPrefix(srv.URL, "https://")
	res, err := e.Invoke(context.Background(), rpc.InvokeFunctionParams{Site: e.site, Name: "hello", Source: "test", Body: "x", TimeoutSec: 5})
	if err != nil || res.Status != 201 || res.Body != "echo:x" {
		t.Fatalf("invoke %+v %v", res, err)
	}
	if got.Host != "fn.example.com" || got.URL.Path != "/.functions/hello" || got.Header.Get("X-CloudGround-Source") != "test" || got.UserAgent() != "CloudGround-Functions/test" {
		t.Fatalf("request %+v", got)
	}

	logDir := filepath.Join(e.LogRoot, "fn.example.com")
	os.MkdirAll(logDir, 0o755)
	access := `203.0.113.9 - fn.example.com [27/Oct/2026:10:00:00 +0000] "GET /.functions/hello/x?a=1 HTTP/2.0" 200 12 0.042 cache:- "Mozilla/5.0"
203.0.113.9 - fn.example.com [27/Oct/2026:10:00:01 +0000] "GET /about HTTP/2.0" 200 12 0.001 cache:- "Mozilla/5.0"
127.0.0.1 - fn.example.com [27/Oct/2026:10:01:00 +0000] "POST /.functions/hello HTTP/1.1" 502 0 1.500 cache:- "CloudGround-Functions/schedule"
203.0.113.9 - fn.example.com [27/Oct/2026:10:02:00 +0000] "GET /.functions/hello2 HTTP/2.0" 200 12 0.001 cache:- "Mozilla/5.0"
`
	os.WriteFile(filepath.Join(logDir, "access.log"), []byte(access), 0o644)
	os.WriteFile(filepath.Join(logDir, "fn-hello.log"), []byte("{\"status\":200}\nhello from the handler\n"), 0o644)
	logs, err := e.Logs(context.Background(), rpc.FunctionRef{Site: e.site, Name: "hello"})
	if err != nil || len(logs.Invocations) != 2 {
		t.Fatalf("logs %+v %v", logs, err)
	}
	if first := logs.Invocations[0]; first.Status != 502 || first.Source != "schedule" || first.MS != 1500 {
		t.Fatalf("newest %+v", first)
	}
	if logs.Invocations[1].Source != "http" || logs.Invocations[1].MS != 42 || len(logs.Output) != 2 {
		t.Fatalf("logs %+v", logs)
	}
}
