package runtimes

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/state"
)

func app(t state.SiteType) state.Site {
	v := "22"
	if t == state.SitePython {
		v = "3.12"
	}
	return state.Site{ID: 7, Domain: "app.example.com", Type: t, SystemUser: "cg-site-7", RootPath: "/srv/sites/app.example.com",
		Config: state.SiteConfig{App: state.SiteApp{Version: v, Start: DefaultStart(Of(t))}}}
}

func TestVersions(t *testing.T) {
	for _, c := range []struct {
		rt, v string
		ok    bool
	}{{Node, "22", true}, {Node, "18", false}, {Node, "22.1", false}, {Python, "3.12", true}, {Python, "3", false}, {Python, "2.7", false}, {"ruby", "3", false}} {
		if ValidVersion(c.rt, c.v) != c.ok {
			t.Errorf("%s %s", c.rt, c.v)
		}
	}
}

func TestCheckApp(t *testing.T) {
	base := app(state.SiteNode).Config.App
	if err := CheckApp(state.SiteNode, base); err != nil {
		t.Fatal(err)
	}
	if err := CheckApp(state.SitePHP, base); err == nil {
		t.Error("a PHP site took app settings")
	}
	for name, mut := range map[string]func(a *state.SiteApp){
		"version":   func(a *state.SiteApp) { a.Version = "16" },
		"start":     func(a *state.SiteApp) { a.Start = " " },
		"two lines": func(a *state.SiteApp) { a.Start = "node a.js\nrm -rf /" },
		"name":      func(a *state.SiteApp) { a.Env = map[string]string{"lower": "x"} },
		"reserved":  func(a *state.SiteApp) { a.Env = map[string]string{"PORT": "1"} },
		"cg":        func(a *state.SiteApp) { a.Env = map[string]string{"CG_START": "x"} },
		"ld":        func(a *state.SiteApp) { a.Secrets = []string{"LD_PRELOAD"} },
		"newline":   func(a *state.SiteApp) { a.Env = map[string]string{"A": "x\ny"} },
		"twice":     func(a *state.SiteApp) { a.Env = map[string]string{"A": "x"}; a.Secrets = []string{"A"} },
		"port":      func(a *state.SiteApp) { a.Port = 3306 },
		"low port":  func(a *state.SiteApp) { a.Port = 80 },
		"health":    func(a *state.SiteApp) { a.HealthPath = "health" },
	} {
		a := base
		mut(&a)
		if err := CheckApp(state.SiteNode, a); err == nil {
			t.Errorf("%s: taken", name)
		}
	}
}

func TestRenderUnit(t *testing.T) {
	name, unit, err := RenderUnit(app(state.SiteNode))
	if err != nil || name != "cloudground-app-cg-site-7.service" {
		t.Fatal(name, err)
	}
	for _, want := range []string{
		"User=cg-site-7\n", "Slice=cg-site-7.slice\n", "WorkingDirectory=/srv/sites/app.example.com/current\n",
		"Environment=PATH=/var/lib/cloudground-runtimes/node/22/bin:/usr/local/bin:/usr/bin:/bin\n",
		"Environment=PORT=/run/cloudground/app/cg-site-7/app.sock\n", "Environment=NODE_ENV=production\n",
		"EnvironmentFile=-/etc/cloudground-app/cg-site-7.env\n", `Environment="CG_START=node server.js"` + "\n",
		`ExecStart=/bin/sh -c 'eval "exec $$CG_START"'` + "\n", "UMask=0007\n", "Restart=always\n",
		"StandardOutput=append:/var/log/cloudground/app.example.com/app.log\n", "ProtectSystem=strict\n",
		"SystemCallFilter=@system-service\n", "BindPaths=/srv/sites/app.example.com",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
	p := app(state.SitePython)
	p.Config.App.Port = 8000
	_, unit, err = RenderUnit(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Environment=PORT=8000\n", "Environment=VIRTUAL_ENV=/srv/sites/app.example.com/current/.venv\n",
		"Environment=PATH=/srv/sites/app.example.com/current/.venv/bin:"} {
		if !strings.Contains(unit, want) {
			t.Errorf("python unit lacks %q", want)
		}
	}
	if strings.Contains(unit, "CG_SOCKET") {
		t.Error("a port app has a socket")
	}
	if Upstream(p) != "127.0.0.1:8000" || Upstream(app(state.SiteNode)) != "unix:/run/cloudground/app/cg-site-7/app.sock" {
		t.Error("upstreams")
	}
}

func TestQuoting(t *testing.T) {
	if got := QuoteEnv("CG_START", `node "a b".js 100% \x`); got != `"CG_START=node \"a b\".js 100%% \\x"` {
		t.Fatalf("QuoteEnv = %s", got)
	}
	env := RenderEnv(map[string]string{"B": `say "hi" $HOME \ ` + "`id`", "A": "1"})
	if !strings.HasSuffix(env, "A=\"1\"\nB=\"say \\\"hi\\\" \\$HOME \\\\ \\`id\\`\"\n") {
		t.Fatalf("env:\n%s", env)
	}
	// What systemd hands the shell is CG_START as typed; eval runs it as a prompt would.
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	cmd := exec.Command(sh, "-c", `eval "exec $CG_START"`)
	cmd.Env = []string{`CG_START=printf '%s|' "a b" 'c'"'"'d' $X`, "X=x"}
	out, err := cmd.Output()
	if err != nil || string(out) != "a b|c'd|x|" {
		t.Fatalf("eval: %q %v", out, err)
	}
}

func TestPrepareScript(t *testing.T) {
	if s := PrepareScript(app(state.SiteNode)); !strings.Contains(s, "npm ci --omit=dev") {
		t.Error(s)
	}
	if s := PrepareScript(app(state.SitePython)); !strings.Contains(s, "/usr/bin/python3.12 -m venv .venv") || !strings.Contains(s, "-r requirements.txt") {
		t.Error(s)
	}
}

func TestNodeIndex(t *testing.T) {
	idx := []byte(`[{"version":"v24.1.0"},{"version":"v22.9.0"},{"version":"v22.11.0"},{"version":"v22.10.5"},{"version":"v20.18.0"}]`)
	if v, err := PickNode(idx, "22"); err != nil || v != "v22.11.0" {
		t.Fatal(v, err)
	}
	if _, err := PickNode(idx, "18"); err == nil {
		t.Error("no 18 found one")
	}
	sums := strings.Repeat("a", 64) + "  node-v22.11.0-linux-x64.tar.gz\n" + strings.Repeat("b", 64) + "  node-v22.11.0-linux-arm64.tar.gz\n"
	if d, err := ParseShasums(sums, TarballName("v22.11.0", "arm64")); err != nil || d != strings.Repeat("b", 64) {
		t.Fatal(d, err)
	}
	if _, err := ParseShasums("xyz  node-v22.11.0-linux-x64.tar.gz", TarballName("v22.11.0", "x64")); err == nil {
		t.Error("a malformed digest taken")
	}
}
