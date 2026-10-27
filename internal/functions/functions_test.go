package functions

import (
	"bytes"
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

func fnc() state.SiteFunction {
	return Defaults(state.SiteFunction{Name: "hello", Runtime: "node"})
}

func TestCheck(t *testing.T) {
	if err := Check(fnc()); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(f *state.SiteFunction){
		"name":     func(f *state.SiteFunction) { f.Name = "Hello" },
		"name len": func(f *state.SiteFunction) { f.Name = strings.Repeat("a", 32) },
		"runtime":  func(f *state.SiteFunction) { f.Runtime = "ruby" },
		"version":  func(f *state.SiteFunction) { f.Version = "18" },
		"memory":   func(f *state.SiteFunction) { f.MemoryMB = 32 },
		"memory+":  func(f *state.SiteFunction) { f.MemoryMB = 4096 },
		"timeout":  func(f *state.SiteFunction) { f.TimeoutSec = 301 },
		"schedule": func(f *state.SiteFunction) { f.Schedule = "@daily" },
		"env":      func(f *state.SiteFunction) { f.Env = map[string]string{"PORT": "1"} },
		"twice":    func(f *state.SiteFunction) { f.Env = map[string]string{"A": "1"}; f.Secrets = []string{"A"} },
	} {
		f := fnc()
		mut(&f)
		if err := Check(f); err == nil {
			t.Errorf("%s: taken", name)
		}
	}
	if err := CheckCode(strings.Repeat("x", MaxCode+1)); err == nil {
		t.Error("oversized code taken")
	}
	if err := CheckCode("ok\xff"); err == nil {
		t.Error("non-UTF-8 code taken")
	}
	if p := Defaults(state.SiteFunction{Runtime: "python"}); p.Version != "3.12" || p.MemoryMB != 128 || p.TimeoutSec != 10 {
		t.Fatalf("defaults %+v", p)
	}
}

func TestUnits(t *testing.T) {
	s := state.Site{ID: 5, Domain: "fn.example.com", SystemUser: "cg-site-5", RootPath: "/srv/sites/fn.example.com"}
	f := fnc()
	f.MemoryMB, f.TimeoutSec = 256, 30
	sock := RenderSocket(s, f)
	for _, want := range []string{"ListenStream=/run/cloudground/fn/cg-site-5/hello.sock\n", "Accept=yes\n", "SocketMode=0660\n", "SocketGroup=cg-site-5\n", "WantedBy=sockets.target"} {
		if !strings.Contains(sock, want) {
			t.Errorf("socket lacks %q:\n%s", want, sock)
		}
	}
	svc := RenderService(s, f)
	for _, want := range []string{
		"User=cg-site-5\n", "Slice=cg-site-5.slice\n", "RuntimeMaxSec=30\n", "MemoryMax=256M\n", "MemorySwapMax=0\n",
		"Environment=CG_TIMEOUT_MS=29750\n", "Environment=CG_HANDLER=/srv/sites/fn.example.com/functions/hello/current/index.js\n",
		"ExecStart=/var/lib/cloudground-runtimes/node/22/bin/node /etc/cloudground-fn/shim/fn.mjs\n",
		"EnvironmentFile=-/etc/cloudground-fn/cg-site-5-hello.env\n", "StandardOutput=append:/var/log/cloudground/fn.example.com/fn-hello.log\n",
		"ProtectSystem=strict\n", "SystemCallFilter=@system-service\n",
	} {
		if !strings.Contains(svc, want) {
			t.Errorf("service lacks %q:\n%s", want, svc)
		}
	}
	p := Defaults(state.SiteFunction{Name: "py", Runtime: "python"})
	if svc := RenderService(s, p); !strings.Contains(svc, "ExecStart=/usr/bin/python3.12 /etc/cloudground-fn/shim/fn.py\n") || !strings.Contains(svc, "/current/main.py\n") {
		t.Errorf("python service:\n%s", svc)
	}
	if SocketUnit("cg-site-5", "hello") != "cloudground-fn-cg-site-5-hello.socket" || ServiceTemplate("cg-site-5", "hello") != "cloudground-fn-cg-site-5-hello@.service" {
		t.Error("unit names")
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
		{"node", ShimNode, "index.js", `exports.handler = async (e) => ({statusCode: 201, body: {p: e.path, q: e.query, b: e.body, s: e.source}});`},
		{"python3", ShimPython, "main.py", "def handler(e):\n    return {\"statusCode\": 201, \"body\": {\"p\": e[\"path\"], \"q\": e[\"query\"], \"b\": e[\"body\"], \"s\": e[\"source\"]}}\n"},
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
		fds, err := socketpair()
		if err != nil {
			t.Skip(err)
		}
		cmd := exec.Command(bin, shim+map[string]string{"node": ".mjs", "python3": ".py"}[c.bin])
		cmd.Env = []string{"CG_FUNCTION=hello", "CG_HANDLER=" + handler, "CG_TIMEOUT_MS=3000", "LISTEN_FDS=1", "PATH=/usr/bin:/bin"}
		cmd.ExtraFiles = []*os.File{fds[1]} // fd 3
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		fds[1].Close()
		conn, _ := net.FileConn(fds[0])
		conn.Write([]byte("POST /.functions/hello/a/b?x=1 HTTP/1.0\r\nHost: x\r\nX-CloudGround-Source: test\r\nContent-Length: 2\r\n\r\nhi"))
		conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		resp, _ := io.ReadAll(conn)
		conn.Close()
		_ = cmd.Wait()
		if !strings.Contains(string(resp), " 201 ") || !strings.Contains(string(resp), `"p":`) || !strings.Contains(strings.ReplaceAll(string(resp), " ", ""), `"p":"/a/b","q":"x=1","b":"hi","s":"test"`) {
			t.Errorf("%s answered:\n%s", c.bin, resp)
		}
		if !strings.Contains(out.String(), `"status":`) && !strings.Contains(out.String(), `"status": 201`) {
			t.Errorf("%s logged %q", c.bin, out.String())
		}
	}
}
