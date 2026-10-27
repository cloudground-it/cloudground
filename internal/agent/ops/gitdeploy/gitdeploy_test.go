package gitdeploy

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

const (
	release = "01JA2B3C4D5E6F7G8H9JKMNPQR"
	sha     = "0123456789abcdef0123456789abcdef01234567"
	key     = "-----BEGIN OPENSSH PRIVATE KEY-----\nSECRETKEYBYTES\n-----END OPENSSH PRIVATE KEY-----"
)

func fixture(t *testing.T, respond func(host.FakeCall) host.FakeReply) (*Ops, *host.FakeExec, rpc.GitSyncParams) {
	t.Helper()
	root := t.TempDir()
	st := state.Site{ID: 12, Domain: "shop.example.com", Type: state.SiteStatic, SystemUser: "cg-site-12", RootPath: root}
	ex := &host.FakeExec{Respond: respond}
	o := &Ops{Exec: ex, Open: func(s state.Site) (*sitefs.Root, error) {
		return sitefs.Open(s.RootPath, sitefs.Owner{UID: os.Getuid(), GID: os.Getgid()})
	}}
	return o, ex, rpc.GitSyncParams{Site: st, Repo: "git@example.com:acme/shop.git", Branch: "main", ReleaseID: release}
}

func fetchOK(c host.FakeCall) host.FakeReply {
	return host.FakeReply{Stdout: "remote: counting objects\nCG-COMMIT " + sha + "\n"}
}

// The fetch is one service as the site user, in the site's slice and
// sandbox, with the repository, the branch and the release as arguments and
// the key on standard input only.
func TestSyncFetchesAsTheSiteUser(t *testing.T) {
	o, ex, p := fixture(t, fetchOK)
	p.DeployKey = key
	res, err := o.Sync(context.Background(), p)
	if err != nil || res.Failed != "" || res.Commit != sha {
		t.Fatalf("%+v %v", res, err)
	}
	work := filepath.Join(p.Site.RootPath, WorkDir)
	if res.SourceDir != filepath.Join(work, "export", release) {
		t.Fatalf("source dir %q", res.SourceDir)
	}
	if fi, err := os.Stat(work); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("work dir %v %v", fi, err)
	}
	calls := ex.Calls()
	if len(calls) != 1 || calls[0].Name != "systemd-run" {
		t.Fatalf("calls %+v", calls)
	}
	line := calls[0].Line()
	for _, want := range []string{"--uid=cg-site-12", "--gid=cg-site-12", "--slice=cg-site-12.slice", "--working-directory=" + work,
		"ProtectSystem=strict", "PrivateTmp=yes", "NoNewPrivileges=yes", "RuntimeMaxSec=1200", "--pipe",
		" cg-git " + work + " git@example.com:acme/shop.git main " + release + " key"} {
		if !strings.Contains(line, want) {
			t.Errorf("argv lacks %q:\n%s", want, line)
		}
	}
	if strings.Contains(line, "SECRETKEYBYTES") {
		t.Fatal("the key is in argv")
	}
	if calls[0].Stdin != key+"\n" {
		t.Fatalf("stdin %q", calls[0].Stdin)
	}
	for _, e := range calls[0].Env {
		if strings.Contains(e, "SECRET") {
			t.Fatalf("env %q", e)
		}
	}
	if !strings.Contains(line, "GIT_ALLOW_PROTOCOL=https:ssh:file") || !strings.Contains(line, "GIT_TERMINAL_PROMPT=0") {
		t.Errorf("the script does not restrict git:\n%s", line)
	}
}

func TestSyncWithoutKeyAndWithoutBuild(t *testing.T) {
	o, ex, p := fixture(t, fetchOK)
	if _, err := o.Sync(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	c := ex.Calls()
	if len(c) != 1 || c[0].Stdin != "" || !strings.HasSuffix(c[0].Line(), " "+release+" nokey") {
		t.Fatalf("calls %+v", c)
	}
}

// A failed fetch is a result with the output, and runs no build.
func TestSyncFetchFailure(t *testing.T) {
	o, ex, p := fixture(t, func(c host.FakeCall) host.FakeReply {
		return host.FakeReply{Stdout: "fatal: could not read from remote repository\n", Code: 1}
	})
	p.BuildCommand = "make"
	res, err := o.Sync(context.Background(), p)
	if err != nil || res.Failed != "fetch" || res.SourceDir != "" || !strings.Contains(res.Output, "could not read from remote") {
		t.Fatalf("%+v %v", res, err)
	}
	if len(ex.Calls()) != 1 {
		t.Fatalf("a build ran after a failed fetch: %d calls", len(ex.Calls()))
	}
	// A fetch that "succeeds" without naming a commit is a failure too.
	o, _, p = fixture(t, func(host.FakeCall) host.FakeReply { return host.FakeReply{Stdout: "nothing\n"} })
	if res, _ := o.Sync(context.Background(), p); res.Failed != "fetch" {
		t.Fatalf("no commit: %+v", res)
	}
}

// The build is a second service of its own, in the export, without the key.
func TestSyncBuild(t *testing.T) {
	n := 0
	o, ex, p := fixture(t, func(c host.FakeCall) host.FakeReply {
		n++
		if n == 1 {
			return fetchOK(c)
		}
		return host.FakeReply{Stdout: "built\n"}
	})
	p.DeployKey, p.BuildCommand = key, "npm ci && npm run build"
	res, err := o.Sync(context.Background(), p)
	if err != nil || res.Failed != "" || res.Output != "built\n" {
		t.Fatalf("%+v %v", res, err)
	}
	c := ex.Calls()
	if len(c) != 2 {
		t.Fatalf("calls %d", len(c))
	}
	b := c[1]
	if b.Stdin != "" || strings.Contains(b.Line(), "SECRETKEYBYTES") {
		t.Fatalf("the build holds the key: %+v", b)
	}
	if !strings.Contains(b.Line(), "--uid=cg-site-12") || !strings.Contains(b.Line(), "--working-directory="+res.SourceDir) ||
		!strings.Contains(b.Line(), "RuntimeMaxSec=1800") || !strings.HasSuffix(b.Line(), "cg-git-build npm ci && npm run build") ||
		!strings.Contains(b.Line(), "ProtectSystem=strict") {
		t.Fatalf("build argv: %s", b.Line())
	}
}

// A failed build is a result, its export is removed, and the live release
// is nobody's business here.
func TestSyncBuildFailure(t *testing.T) {
	n := 0
	o, _, p := fixture(t, func(c host.FakeCall) host.FakeReply {
		n++
		if n == 1 {
			return fetchOK(c)
		}
		return host.FakeReply{Stdout: "npm ERR! missing script\n", Code: 1}
	})
	p.BuildCommand = "npm run build"
	exp := filepath.Join(p.Site.RootPath, WorkDir, "export", release)
	if err := os.MkdirAll(exp, 0o700); err != nil {
		t.Fatal(err)
	}
	res, err := o.Sync(context.Background(), p)
	if err != nil || res.Failed != "build" || res.SourceDir != "" || !strings.Contains(res.Output, "missing script") {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := os.Stat(exp); !os.IsNotExist(err) {
		t.Fatalf("the failed build's export stays: %v", err)
	}
}

// What the script does, run for real against a local bare repository: the
// branch's tree is exported without .git at the commit the last line names,
// the work survives a second run, another branch and a missing one.
func TestScriptFetchesAndExports(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	run := func(wd string, args ...string) string {
		t.Helper()
		c := exec.Command("git", append([]string{"-c", "user.email=a@b", "-c", "user.name=t", "-c", "init.defaultBranch=main"}, args...)...)
		c.Dir = wd
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	bare, src := filepath.Join(dir, "r.git"), filepath.Join(dir, "src")
	run(dir, "init", "--bare", "-q", bare)
	run(dir, "init", "-q", src)
	if err := os.WriteFile(filepath.Join(src, "index.html"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(src, "add", ".")
	run(src, "commit", "-q", "-m", "one")
	run(src, "push", "-q", bare, "HEAD:refs/heads/main", "HEAD:refs/heads/next")
	commit := run(src, "rev-parse", "HEAD")

	work := filepath.Join(dir, "work")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	sync := func(branch string) (string, error) {
		c := exec.Command("/bin/sh", "-c", fetchScript, "cg-git", work, "file://"+bare, branch, release, "nokey")
		out, err := c.CombinedOutput()
		return string(out), err
	}
	out, err := sync("main")
	if err != nil || !strings.HasSuffix(strings.TrimSpace(out), commitMark+commit) {
		t.Fatalf("%v\n%s", err, out)
	}
	exp := filepath.Join(work, "export", release)
	if b, _ := os.ReadFile(filepath.Join(exp, "index.html")); string(b) != "v1" {
		t.Fatalf("export holds %q", b)
	}
	if _, err := os.Stat(filepath.Join(exp, ".git")); err == nil {
		t.Fatal("the export has a .git")
	}

	// A second commit is fetched into the same clone; the old export goes.
	if err := os.WriteFile(filepath.Join(src, "index.html"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(src, "commit", "-q", "-am", "two")
	run(src, "push", "-q", bare, "HEAD:refs/heads/main")
	if err := os.WriteFile(filepath.Join(work, "export", "stale"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err = sync("main"); err != nil || !strings.HasSuffix(strings.TrimSpace(out), commitMark+run(src, "rev-parse", "HEAD")) {
		t.Fatalf("%v\n%s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(exp, "index.html")); string(b) != "v2" {
		t.Fatalf("export holds %q", b)
	}
	if _, err := os.Stat(filepath.Join(work, "export", "stale")); err == nil {
		t.Fatal("an old export stays")
	}
	if out, err := sync("nope"); err == nil || strings.Contains(out, commitMark) {
		t.Fatalf("a missing branch succeeded:\n%s", out)
	}
}
