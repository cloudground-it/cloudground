package functions

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/state"
)

func fnc() state.Function {
	return Defaults(state.Function{ID: 7, Name: "hello", Runtime: "node"})
}

func TestCheck(t *testing.T) {
	if err := Check(fnc()); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(f *state.Function){
		"name":       func(f *state.Function) { f.Name = "Hello" },
		"name len":   func(f *state.Function) { f.Name = strings.Repeat("a", 32) },
		"runtime":    func(f *state.Function) { f.Runtime = "ruby" },
		"version":    func(f *state.Function) { f.Version = "18" },
		"memory":     func(f *state.Function) { f.MemoryMB = 32 },
		"memory+":    func(f *state.Function) { f.MemoryMB = 4096 },
		"timeout":    func(f *state.Function) { f.TimeoutSec = 301 },
		"schedule":   func(f *state.Function) { f.Schedule = "@daily" },
		"env":        func(f *state.Function) { f.Env = map[string]string{"PORT": "1"} },
		"twice":      func(f *state.Function) { f.Env = map[string]string{"A": "1"}; f.Secrets = []string{"A"} },
		"auth":       func(f *state.Function) { f.Auth = "basic" },
		"origin":     func(f *state.Function) { f.CORSOrigins = []string{"https://a.example.com/path"} },
		"origin *":   func(f *state.Function) { f.CORSOrigins = []string{"*", "https://a.example.com"} },
		"origin dup": func(f *state.Function) { f.CORSOrigins = []string{"https://a.example.com", "https://a.example.com"} },
		"domains":    func(f *state.Function) { f.Domains = make([]string, 11) },
	} {
		f := fnc()
		mut(&f)
		if err := Check(f); err == nil {
			t.Errorf("%s: taken", name)
		}
	}
	ok := fnc()
	ok.CORSOrigins = []string{"https://app.example.com", "http://localhost:3000"}
	if err := Check(ok); err != nil {
		t.Errorf("origins: %v", err)
	}
	if p := Defaults(state.Function{Runtime: "python"}); p.Version != "3.12" || p.MemoryMB != 128 || p.TimeoutSec != 10 || p.Auth != "public" {
		t.Fatalf("defaults %+v", p)
	}
}

func TestCheckFiles(t *testing.T) {
	good := map[string]string{"index.js": "exports.handler = () => 1;", "lib/util.js": "x", "package.json": "{}", ".npmrc": "x"}
	if err := CheckFiles("node", good); err != nil {
		t.Fatal(err)
	}
	if !NeedsInstall("node", good) || NeedsInstall("python", good) || !NeedsInstall("python", map[string]string{"requirements.txt": ""}) {
		t.Error("NeedsInstall")
	}
	for name, files := range map[string]map[string]string{
		"empty":        {},
		"no entry":     {"lib.js": "x"},
		"python entry": {"index.js": "x"},
		"dotdot":       {"index.js": "x", "../x.js": "x"},
		"absolute":     {"index.js": "x", "/etc/passwd": "x"},
		"node_modules": {"index.js": "x", "node_modules/a/index.js": "x"},
		"venv":         {"index.js": "x", ".venv/bin/python": "x"},
		"manifest":     {"index.js": "x", ".cg-files": "x"},
		"space":        {"index.js": "x", "a b.js": "x"},
		"empty part":   {"index.js": "x", "a//b.js": "x"},
		"binary":       {"index.js": "x\x00"},
		"not utf-8":    {"index.js": "\xff"},
		"too big":      {"index.js": strings.Repeat("x", MaxCode+1)},
	} {
		rt := "node"
		if name == "python entry" {
			rt = "python"
		}
		if err := CheckFiles(rt, files); err == nil {
			t.Errorf("%s: taken", name)
		}
	}
	many := map[string]string{"index.js": "x"}
	for i := range MaxFiles {
		many[fmt.Sprintf("f%d.js", i)] = "x"
	}
	if err := CheckFiles("node", many); err == nil {
		t.Error("too many files taken")
	}
	if s := Starter("python", TemplateScheduled); !strings.Contains(s["main.py"], "def handler") {
		t.Errorf("python scheduled starter: %v", s)
	}
	if s := Starter("node", ""); CheckFiles("node", s) != nil {
		t.Errorf("node starter: %v", s)
	}
}

func TestNamesAndAddresses(t *testing.T) {
	f := fnc()
	f.Domains = []string{"api.example.com"}
	if got := URLs(f, "fn.example.com"); strings.Join(got, " ") != "https://fn.example.com/hello https://api.example.com/" {
		t.Errorf("urls %v", got)
	}
	f.Domains = nil
	if got := URLs(f, ""); len(got) != 0 {
		t.Errorf("urls without a domain %v", got)
	}
	if User(7) != "cg-fn-7" || !ValidUser("cg-fn-7") || ValidUser("cg-site-7") || ValidUser("cg-fn-07") || Slice(7) != "cg-fn-7.slice" {
		t.Error("user names")
	}
	if SocketUnit(7) != "cloudground-fn-7.socket" || ServiceTemplate(7) != "cloudground-fn-7@.service" || EnvFile(7) != "/etc/cloudground-fn/7.env" {
		t.Error("unit names")
	}
	if !ValidLineage("cloudground-functions") || !ValidLineage(FunctionLineage(7)) || ValidLineage("cloudground-fn-x") || ValidLineage("example.com") {
		t.Error("lineages")
	}
	tok, err := NewToken()
	if err != nil || !ValidToken(tok) || len(tok) != 47 {
		t.Errorf("token %q %v", tok, err)
	}
	if other, _ := NewToken(); other == tok {
		t.Error("two tokens alike")
	}
}

func TestUnits(t *testing.T) {
	f := fnc()
	f.MemoryMB, f.TimeoutSec = 256, 30
	sock := RenderSocket(f)
	for _, want := range []string{"ListenStream=/run/cloudground/fn/7.sock\n", "Accept=yes\n", "MaxConnections=32\n", "MaxConnectionsPerSource=32\n",
		"SocketMode=0660\n", "SocketUser=root\n", "SocketGroup=www-data\n", "WantedBy=sockets.target"} {
		if !strings.Contains(sock, want) {
			t.Errorf("socket lacks %q:\n%s", want, sock)
		}
	}
	svc := RenderService(f)
	for _, want := range []string{
		"User=cg-fn-7\n", "Group=cg-fn-7\n", "Slice=cg-fn-7.slice\n", "RuntimeMaxSec=30\n", "MemoryMax=256M\n", "MemorySwapMax=0\n",
		"WorkingDirectory=/srv/functions/hello/current\n",
		"Environment=CG_TIMEOUT_MS=29750\n", "Environment=CG_HANDLER=/srv/functions/hello/current/index.js\n",
		"ExecStart=/var/lib/cloudground-runtimes/node/22/bin/node /etc/cloudground-fn/shim/fn.mjs\n",
		"EnvironmentFile=-/etc/cloudground-fn/7.env\n", "StandardOutput=append:/var/log/cloudground/functions/hello.output.log\n",
		"ProtectSystem=strict\n", "SystemCallFilter=@system-service\n",
		"TemporaryFileSystem=/srv/sites:ro /srv/functions:ro /var/log/cloudground:ro\n",
		"BindReadOnlyPaths=/srv/functions/hello\n", "NoNewPrivileges=yes\n", "CapabilityBoundingSet=\n",
	} {
		if !strings.Contains(svc, want) {
			t.Errorf("service lacks %q:\n%s", want, svc)
		}
	}
	// The run writes nowhere: no read-write bind at all.
	if strings.Contains(svc, "BindPaths=") || strings.Contains(svc, "ReadWritePaths=") {
		t.Errorf("the run has a writable bind:\n%s", svc)
	}
	p := Defaults(state.Function{ID: 8, Name: "py", Runtime: "python"})
	if svc := RenderService(p); !strings.Contains(svc, "ExecStart=/usr/bin/python3.12 /etc/cloudground-fn/shim/fn.py\n") || !strings.Contains(svc, "/current/main.py\n") {
		t.Errorf("python service:\n%s", svc)
	}
	inst := strings.Join(InstallSandbox("hello", "01JABCDEFGHJKMNPQRSTVWXYZ0"), "\n")
	if !strings.Contains(inst, "BindPaths=/srv/functions/hello/releases/01JABCDEFGHJKMNPQRSTVWXYZ0") || !strings.Contains(inst, "BindReadOnlyPaths=/srv/functions/hello") {
		t.Errorf("install sandbox:\n%s", inst)
	}
	if sl := RenderParentSlice(); !strings.Contains(sl, "MemoryMax=50%\n") || !strings.Contains(sl, "CPUWeight=50\n") {
		t.Errorf("slice:\n%s", sl)
	}
	if s := PrepareScript("python", "3.12"); !strings.Contains(s, "/usr/bin/python3.12 -m venv .venv") || !strings.Contains(s, "-r requirements.txt") {
		t.Errorf("python prepare:\n%s", s)
	}
	if s := PrepareScript("node", "22"); !strings.Contains(s, "npm ci --omit=dev") {
		t.Errorf("node prepare:\n%s", s)
	}
}

func TestDue(t *testing.T) {
	at := func(s string) time.Time {
		tm, err := time.Parse("2006-01-02 15:04 Mon", s)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	for _, c := range []struct {
		sched, at string
		want      bool
	}{
		{"* * * * *", "2026-10-27 10:13 Tue", true},
		{"*/15 * * * *", "2026-10-27 10:30 Tue", true},
		{"*/15 * * * *", "2026-10-27 10:31 Tue", false},
		{"0 9-17/4 * * *", "2026-10-27 13:00 Tue", true},
		{"0 9-17/4 * * *", "2026-10-27 15:00 Tue", false},
		{"5,10 * * * *", "2026-10-27 10:10 Tue", true},
		{"0 0 1 * *", "2026-11-01 00:00 Sun", true},
		{"0 0 * * 0", "2026-11-01 00:00 Sun", true},
		{"0 0 * * 7", "2026-11-01 00:00 Sun", true},
		{"0 0 * * 1-5", "2026-11-01 00:00 Sun", false},
		// Both day fields restricted: either is enough.
		{"0 0 15 * 2", "2026-10-27 00:00 Tue", true},
		{"0 0 15 * 3", "2026-10-27 00:00 Tue", false},
		{"0 0 * 12 *", "2026-10-27 00:00 Tue", false},
		{"bad", "2026-10-27 00:00 Tue", false},
	} {
		if got := Due(c.sched, at(c.at)); got != c.want {
			t.Errorf("Due(%q, %s) = %v", c.sched, c.at, got)
		}
	}
}

// The shims run for real when the runtime is here, on a socketpair
// standing for the accepted connection (fd 3).
func TestShims(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		bin, shim, file, code string
	}{
		{"node", ShimNode, "index.js", `const h = require("./helper.js"); exports.handler = async (e) => ({statusCode: 201, body: {p: e.path, q: e.query, b: e.body, s: e.source, m: h.mark, x: e.headers["x-cloudground-prefix"] || "none"}});`},
		{"python3", ShimPython, "main.py", "import helper\ndef handler(e):\n    return {\"statusCode\": 201, \"body\": {\"p\": e[\"path\"], \"q\": e[\"query\"], \"b\": e[\"body\"], \"s\": e[\"source\"], \"m\": helper.mark, \"x\": e[\"headers\"].get(\"x-cloudground-prefix\", \"none\")}}\n"},
	} {
		bin, err := exec.LookPath(c.bin)
		if err != nil {
			t.Logf("%s not here: skipped", c.bin)
			continue
		}
		shim := filepath.Join(dir, "shim-"+c.file)
		handler := filepath.Join(dir, c.file)
		if err := os.WriteFile(shim+map[string]string{"node": ".mjs", "python3": ".py"}[c.bin], []byte(c.shim), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(handler, []byte(c.code), 0o644); err != nil {
			t.Fatal(err)
		}
		helper := map[string][2]string{"node": {"helper.js", `exports.mark = "M";`}, "python3": {"helper.py", "mark = \"M\"\n"}}[c.bin]
		if err := os.WriteFile(filepath.Join(dir, helper[0]), []byte(helper[1]), 0o644); err != nil {
			t.Fatal(err)
		}
		fds, err := socketpair()
		if err != nil {
			t.Skip(err)
		}
		cmd := exec.Command(bin, shim+map[string]string{"node": ".mjs", "python3": ".py"}[c.bin])
		cmd.Env = []string{"CG_HANDLER=" + handler, "CG_TIMEOUT_MS=3000", "LISTEN_FDS=1", "PATH=/usr/bin:/bin"}
		cmd.ExtraFiles = []*os.File{fds[1]} // fd 3
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		fds[1].Close()
		conn, _ := net.FileConn(fds[0])
		conn.Write([]byte("POST /hello/a/b?x=1 HTTP/1.0\r\nHost: x\r\nX-CloudGround-Prefix: /hello\r\nX-CloudGround-Source: test\r\nContent-Length: 2\r\n\r\nhi"))
		conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		resp, _ := io.ReadAll(conn)
		conn.Close()
		_ = cmd.Wait()
		if !strings.Contains(string(resp), " 201 ") || !strings.Contains(string(resp), `"p":`) || !strings.Contains(strings.ReplaceAll(string(resp), " ", ""), `"p":"/a/b","q":"x=1","b":"hi","s":"test","m":"M","x":"none"`) {
			t.Errorf("%s answered:\n%s", c.bin, resp)
		}
		if !strings.Contains(out.String(), `"status":`) && !strings.Contains(out.String(), `"status": 201`) {
			t.Errorf("%s logged %q", c.bin, out.String())
		}
	}
}
