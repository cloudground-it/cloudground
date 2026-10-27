package sites

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/runtimes"
	"github.com/cloudground-it/cloudground/internal/state"
)

func appSite(e *env, id int64, domain string, typ state.SiteType) state.Site {
	s := e.site(id, domain, typ)
	v := "22"
	if typ == state.SitePython {
		v = "3.12"
	}
	s.Config.App = state.SiteApp{Version: v, Start: runtimes.DefaultStart(runtimes.Of(typ))}
	s.PHPVersion = ""
	return s
}

// #28 (SPEC §11): a managed app is bootstrapped with a starter app and its
// dependencies, runs as a unit of its own in the site's slice, sits behind
// nginx on its socket, and goes with its site.
func TestManagedAppLifecycle(t *testing.T) {
	e := newEnv(t)
	e.Paths.AppEnvDir = filepath.Join(e.root, "app-env")
	var prepared []string
	e.PrepareApp = func(_ context.Context, s state.Site, dir string) (string, error) {
		prepared = append(prepared, dir)
		return "ok", nil
	}
	s := appSite(e, 41, "node.example.com", state.SiteNode)
	res := e.create(t, s)

	if len(prepared) != 1 || prepared[0] != filepath.Join(s.RootPath, "releases", "initial") {
		t.Fatalf("prepared %v", prepared)
	}
	if b, err := os.ReadFile(filepath.Join(s.RootPath, "releases", "initial", "server.js")); err != nil || !strings.Contains(string(b), "process.env.PORT") {
		t.Fatalf("starter: %s %v", b, err)
	}
	unit, err := os.ReadFile(e.appUnitPath(s))
	if err != nil || !strings.Contains(string(unit), "Slice=cg-site-41.slice") {
		t.Fatalf("unit: %s %v", unit, err)
	}
	if !e.systemd.Did("restart", runtimes.UnitName(s.SystemUser)) && !e.systemd.Did("start", runtimes.UnitName(s.SystemUser)) {
		t.Fatalf("the app was not started: %v", e.systemd.Ops())
	}
	if exists(e.unitPath(s)) || exists(e.poolPath(s)) {
		t.Fatal("a managed app got a PHP master")
	}
	vhost, _ := os.ReadFile(e.vhostPath(s))
	if !strings.Contains(string(vhost), "upstream cg_app_41 {\n    server unix:/run/cloudground/app/cg-site-41/app.sock;\n}") ||
		!strings.Contains(string(vhost), "proxy_pass http://cg_app_41;") || strings.Contains(string(vhost), "root "+s.RootPath) {
		t.Fatalf("vhost:\n%s", vhost)
	}
	found := false
	for _, f := range res.Files {
		found = found || f.Path == e.appUnitPath(s)
	}
	if !found {
		t.Fatal("the unit is not a managed file")
	}

	// The environment: root's 0600, escaped, refused when a name is reserved.
	if _, err := e.SetAppEnv(context.Background(), rpc.AppEnvParams{Site: s, Env: map[string]string{"API_KEY": `s3cr"et`}}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(e.appEnvPath(s))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("env file %v %v", fi, err)
	}
	if b, _ := os.ReadFile(e.appEnvPath(s)); !strings.Contains(string(b), `API_KEY="s3cr\"et"`) {
		t.Fatalf("env: %s", b)
	}
	if _, err := e.SetAppEnv(context.Background(), rpc.AppEnvParams{Site: s, Env: map[string]string{"PORT": "1"}}); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
		t.Fatalf("PORT taken: %v", err)
	}

	// Control and status.
	st, err := e.AppControl(context.Background(), rpc.AppControlParams{Site: s, Action: "stop"})
	if err != nil || st.Active {
		t.Fatalf("stop: %+v %v", st, err)
	}
	if _, err := e.AppControl(context.Background(), rpc.AppControlParams{Site: s, Action: "nuke"}); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
		t.Fatal("an unknown action taken")
	}
	// ReloadPHP (a promote, a restore) restarts the app.
	before := len(e.systemd.Ops())
	if err := e.ReloadPHP(context.Background(), s); err != nil || len(e.systemd.Ops()) == before {
		t.Fatalf("reload: %v %v", err, e.systemd.Ops()[before:])
	}

	if _, err := e.Delete(context.Background(), rpc.SiteRef{Site: s}); err != nil {
		t.Fatal(err)
	}
	if exists(e.appUnitPath(s)) || exists(e.appEnvPath(s)) {
		t.Fatal("the app's unit or environment outlived the site")
	}
}

// The health probe asks the app on its socket, with the site's Host.
func TestAppStatusProbe(t *testing.T) {
	e := newEnv(t)
	s := appSite(e, 42, "py.example.com", state.SitePython)
	sock := filepath.Join(t.TempDir(), "app.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip("no unix sockets here")
	}
	var gotHost string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		w.WriteHeader(http.StatusTeapot)
	})}
	go srv.Serve(ln)
	defer srv.Close()
	e.AppSocket = func(state.Site) string { return sock }
	e.systemd.SetUnit(host.UnitProps{Name: runtimes.UnitName(s.SystemUser), ActiveState: "inactive"})
	if err := e.systemd.Start(context.Background(), runtimes.UnitName(s.SystemUser)); err != nil {
		t.Fatal(err)
	}
	st, err := e.AppStatus(context.Background(), rpc.SiteRef{Site: s})
	if err != nil || !st.Active || st.Health.Status != http.StatusTeapot || gotHost != "py.example.com" {
		t.Fatalf("status %+v %v host %q", st, err, gotHost)
	}
	ln.Close()
	srv.Close()
	os.Remove(sock)
	if st, _ := e.AppStatus(context.Background(), rpc.SiteRef{Site: s}); st.Health.Error != "the app is not listening" {
		t.Fatalf("down: %+v", st.Health)
	}
}
