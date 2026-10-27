package runtimes

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

func mk(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestList(t *testing.T) {
	root := t.TempDir()
	o := &Ops{Dir: filepath.Join(root, "rt"), UsrBin: filepath.Join(root, "bin"), UsrLib: filepath.Join(root, "lib")}
	mk(t, filepath.Join(o.Dir, "node", "22", "bin", "node"), "elf")
	mk(t, filepath.Join(o.Dir, "node", "22", "VERSION"), "v22.11.0\n")
	mk(t, filepath.Join(o.Dir, "node", "20", "VERSION"), "v20.1.0\n") // no binary: not installed
	mk(t, filepath.Join(o.UsrBin, "python3.12"), "elf")
	mk(t, filepath.Join(o.UsrBin, "python3.13"), "elf")
	mk(t, filepath.Join(o.UsrLib, "python3.12", "venv", "__init__.py"), "")
	mk(t, filepath.Join(o.UsrLib, "python3.12", "ensurepip", "__init__.py"), "")
	mk(t, filepath.Join(o.UsrLib, "python3.13", "venv", "__init__.py"), "") // no ensurepip: python3.13-venv missing
	got := o.List()
	if len(got.Node) != 1 || got.Node[0] != (rpc.RuntimeVersion{Version: "22", Full: "v22.11.0"}) {
		t.Fatalf("node %+v", got.Node)
	}
	if len(got.Python) != 1 || got.Python[0].Version != "3.12" || !got.Has("python", "3.12") || got.Has("node", "20") {
		t.Fatalf("python %+v", got.Python)
	}
	if v := o.distroPythons(); strings.Join(v, ",") != "3.13,3.12" {
		t.Fatalf("distro %v", v)
	}
}

func TestInstallUnits(t *testing.T) {
	fe := &host.FakeExec{}
	o := &Ops{Exec: fe, Dir: t.TempDir(), UsrBin: t.TempDir(), AgentBinary: "/usr/local/bin/panel-agent"}
	if _, err := o.Install(context.Background(), rpc.RuntimeInstallParams{Runtime: "node", Version: "22"}); err != nil {
		t.Fatal(err)
	}
	line := fe.Calls()[0].Line()
	for _, want := range []string{"systemd-run", "StateDirectory=cloudground-runtimes", "ProtectSystem=strict", "/usr/local/bin/panel-agent runtime-install node 22", "--uid=root"} {
		if !strings.Contains(line, want) {
			t.Errorf("install unit lacks %q: %s", want, line)
		}
	}
	if _, err := o.Install(context.Background(), rpc.RuntimeInstallParams{Runtime: "python", Version: "3.12"}); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
		t.Fatalf("a python the system lacks: %v", err)
	}
	mk(t, filepath.Join(o.UsrBin, "python3.12"), "elf")
	if _, err := o.Install(context.Background(), rpc.RuntimeInstallParams{Runtime: "python", Version: "3.12"}); err != nil {
		t.Fatal(err)
	}
	if line := fe.Calls()[1].Line(); !strings.Contains(line, "apt-get") || !strings.Contains(line, "python3.12-venv") {
		t.Fatalf("apt: %s", line)
	}
	if _, err := o.Install(context.Background(), rpc.RuntimeInstallParams{Runtime: "ruby", Version: "3"}); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
		t.Fatal("ruby taken")
	}
}

type entry struct {
	name, link string
	typ        byte
	mode       int64
	body       string
}

func tarball(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Linkname: e.link, Typeflag: e.typ, Mode: e.mode, Size: int64(len(e.body))}
		if e.typ != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			_, _ = tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

var good = []entry{
	{name: "node-v22.11.0-linux-x64/", typ: tar.TypeDir, mode: 0o755},
	{name: "node-v22.11.0-linux-x64/bin/", typ: tar.TypeDir, mode: 0o755},
	{name: "node-v22.11.0-linux-x64/bin/node", typ: tar.TypeReg, mode: 0o6755, body: "elf"},
	{name: "node-v22.11.0-linux-x64/lib/node_modules/npm/bin/npm-cli.js", typ: tar.TypeReg, mode: 0o755, body: "js"},
	{name: "node-v22.11.0-linux-x64/bin/npm", typ: tar.TypeSymlink, link: "../lib/node_modules/npm/bin/npm-cli.js"},
}

func TestUnpack(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out")
	if err := Unpack(bytes.NewReader(tarball(t, good...)), dir); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "bin", "node"))
	if err != nil || fi.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 || fi.Mode().Perm() != 0o755 {
		t.Fatalf("node: %v %v", fi.Mode(), err)
	}
	if l, _ := os.Readlink(filepath.Join(dir, "bin", "npm")); l != "../lib/node_modules/npm/bin/npm-cli.js" {
		t.Fatalf("link %q", l)
	}
	for name, bad := range map[string]entry{
		"absolute":    {name: "/etc/passwd", typ: tar.TypeReg, mode: 0o644, body: "x"},
		"dotdot":      {name: "top/../../x", typ: tar.TypeReg, mode: 0o644, body: "x"},
		"escaping":    {name: "top/bin/evil", typ: tar.TypeSymlink, link: "../../../etc"},
		"absolute ln": {name: "top/bin/evil", typ: tar.TypeSymlink, link: "/etc"},
		"device":      {name: "top/dev", typ: tar.TypeChar},
		"hard link":   {name: "top/h", typ: tar.TypeLink, link: "top/bin/node"},
	} {
		d := filepath.Join(t.TempDir(), "out")
		if err := Unpack(bytes.NewReader(tarball(t, bad)), d); err == nil {
			t.Errorf("%s: unpacked", name)
		}
	}
}

func TestNodeInstaller(t *testing.T) {
	tgz := tarball(t, good...)
	sum := sha256.Sum256(tgz)
	digest := hex.EncodeToString(sum[:])
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.json":
			w.Write([]byte(`[{"version":"v22.11.0"},{"version":"v22.10.0"},{"version":"v24.0.0"}]`))
		case "/v22.11.0/SHASUMS256.txt":
			w.Write([]byte(digest + "  node-v22.11.0-linux-x64.tar.gz\n"))
		case "/v22.11.0/node-v22.11.0-linux-x64.tar.gz":
			w.Write(tgz)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	n := NodeInstaller{Base: srv.URL, Dir: dir, Arch: "x64", Client: srv.Client()}
	v, err := n.Install(context.Background(), "22")
	if err != nil || v != "v22.11.0" {
		t.Fatal(v, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "node", "22", "VERSION")); string(b) != "v22.11.0\n" {
		t.Fatalf("VERSION %q", b)
	}
	// Installing again replaces it, and leaves nothing beside.
	if _, err := n.Install(context.Background(), "22"); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "node")); len(entries) != 1 {
		t.Fatalf("left beside: %v", entries)
	}
	// A digest that does not match: refused, nothing unpacked.
	digest = strings.Repeat("0", 64)
	other := t.TempDir()
	n.Dir = other
	if _, err := n.Install(context.Background(), "22"); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("a wrong digest = %v", err)
	}
	if _, err := os.Stat(filepath.Join(other, "node", "22")); !os.IsNotExist(err) {
		t.Fatal("unpacked after a wrong digest")
	}
}

func TestPrepare(t *testing.T) {
	root := t.TempDir()
	fe := &host.FakeExec{}
	o := &Ops{Exec: fe, Dir: filepath.Join(root, "rt"), UsrBin: filepath.Join(root, "bin"), UsrLib: filepath.Join(root, "lib")}
	s := state.Site{ID: 7, Domain: "app.example.com", Type: state.SiteNode, SystemUser: "cg-site-7", RootPath: "/srv/sites/app.example.com",
		Config: state.SiteConfig{App: state.SiteApp{Version: "22", Start: "node server.js"}}}
	if _, err := o.Prepare(context.Background(), s, "/srv/sites/app.example.com/releases/X"); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
		t.Fatalf("no runtime: %v", err)
	}
	mk(t, filepath.Join(o.Dir, "node", "22", "bin", "node"), "elf")
	mk(t, filepath.Join(o.Dir, "node", "22", "VERSION"), "v22.11.0\n")
	if _, err := o.Prepare(context.Background(), s, "/srv/sites/app.example.com/releases/X"); err != nil {
		t.Fatal(err)
	}
	line := fe.Calls()[0].Line()
	for _, want := range []string{"--uid=cg-site-7", "--slice=cg-site-7.slice", "--working-directory=/srv/sites/app.example.com/releases/X",
		"RuntimeMaxSec=1200", "ProtectSystem=strict", "npm ci --omit=dev", "PATH=/var/lib/cloudground-runtimes/node/22/bin"} {
		if !strings.Contains(line, want) {
			t.Errorf("prepare lacks %q:\n%s", want, line)
		}
	}
	failing := &host.FakeExec{Respond: func(host.FakeCall) host.FakeReply { return host.FakeReply{Code: 1, Stdout: "npm ERR! missing script"} }}
	o.Exec = failing
	if _, err := o.Prepare(context.Background(), s, "/x"); agentrpc.CodeOf(err) != agentrpc.CodeInvalid || !strings.Contains(err.Error(), "dependencies failed") {
		t.Fatalf("a failing step = %v", err)
	}
}
