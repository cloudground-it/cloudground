package sftp

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

const (
	alias    = "cg-shell-1"
	shellUID = 1234
)

type shellFixture struct {
	o      *Ops
	acc    *host.FakeAccounts
	sd     *host.FakeSystemd
	ex     *host.FakeExec
	s      state.Site
	paths  ShellPaths
	refuse bool // sshd -t fails
}

// newShellOps is newOps with the site shell wired to fakes and temporary
// paths; the site user's uid is shellUID.
func newShellOps(t *testing.T) *shellFixture {
	t.Helper()
	o, acc, s := newOps(t)
	dir := t.TempDir()
	e := &shellFixture{o: o, acc: acc, s: s, sd: host.NewFakeSystemd()}
	e.ex = &host.FakeExec{Respond: func(c host.FakeCall) host.FakeReply {
		if c.Line() == "sshd -t" && e.refuse {
			return host.FakeReply{Code: 255}
		}
		return host.FakeReply{}
	}}
	e.paths = ShellPaths{
		DropIn:     filepath.Join(dir, "11-cloudground-shell.conf"),
		KeysDropIn: filepath.Join(dir, "09-cloudground-sftp-keys.conf"),
		BinDir:     filepath.Join(dir, "bin"),
		SystemdDir: filepath.Join(dir, "systemd"),
		Socket:     filepath.Join(dir, "shell.sock"),
		Passwd:     filepath.Join(dir, "passwd"),
		SitesRoot:  filepath.Dir(s.RootPath),
		CronDeny:   filepath.Join(dir, "cron.deny"),
		AtDeny:     filepath.Join(dir, "at.deny"),
		SSHDRunDir: filepath.Join(dir, "run-sshd"),
	}
	for _, d := range []string{e.paths.BinDir, e.paths.SystemdDir} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(e.paths.BinDir, AgentBinary), []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.passwd(t, fmt.Sprintf("%s:x:%d:%d::%s:/usr/sbin/nologin", user, shellUID, shellUID, s.RootPath))
	owner := sitefs.Owner{UID: shellUID, GID: os.Getgid()}
	o.Open = func(s state.Site) (*sitefs.Root, error) { return sitefs.Open(s.RootPath, owner) }
	o.Systemd, o.Exec, o.ShellPaths = e.sd, e.ex, e.paths
	o.LookupGID = func(string) (int, error) { return os.Getgid(), nil }
	return e
}

func (e *shellFixture) passwd(t *testing.T, lines ...string) {
	t.Helper()
	if err := os.WriteFile(e.paths.Passwd, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (e *shellFixture) set(t *testing.T, enable bool) (map[string]string, error) {
	t.Helper()
	return e.o.SetShell(ctx, rpc.ShellParams{Site: e.s, Enable: enable})
}

func (e *shellFixture) inDeny(t *testing.T, name string) bool {
	t.Helper()
	ok, err := e.acc.IsMember(ctx, name, DenyGroup)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// The cg-site-shell binary the installer put at the gate's name is kept;
// only a missing name or something else there becomes the link to
// panel-agent (R-10.5).
func TestGateBinaryIsKept(t *testing.T) {
	e := newShellOps(t)
	gate := filepath.Join(e.paths.BinDir, GateName)
	if err := os.WriteFile(gate, []byte("the gate binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := e.set(t, true); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(gate); err != nil || string(b) != "the gate binary" {
		t.Fatalf("the gate binary was replaced: %q %v", b, err)
	}
	if fi, _ := os.Lstat(gate); fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("the gate binary became a link")
	}
	// A non-executable file is not the gate: replaced by the link.
	os.Remove(gate)
	if err := os.WriteFile(gate, []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.o.ensureGateLink(); err != nil {
		t.Fatal(err)
	}
	if l, err := os.Readlink(gate); err != nil || l != AgentBinary {
		t.Fatalf("gate link %q %v", l, err)
	}
}

func TestSetShellEnable(t *testing.T) {
	e := newShellOps(t)
	res, err := e.set(t, true)
	if err != nil {
		t.Fatal(err)
	}
	if res["shell"] != "enabled" || res["user"] != alias {
		t.Fatalf("result %v", res)
	}
	// The gate link, relative, to panel-agent.
	if l, err := os.Readlink(filepath.Join(e.paths.BinDir, GateName)); err != nil || l != AgentBinary {
		t.Fatalf("gate link %q %v", l, err)
	}
	// The sshd drop-in, checked by sshd -t, sshd reloaded only if it runs.
	if b, err := os.ReadFile(e.paths.DropIn); err != nil || string(b) != shellDropIn {
		t.Fatalf("drop-in %q %v", b, err)
	}
	if !e.ex.Ran("sshd -t") {
		t.Fatal("sshd -t did not run")
	}
	// sshd -t's privilege separation directory, made when missing.
	if fi, err := os.Stat(e.paths.SSHDRunDir); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o755 {
		t.Fatalf("sshd run dir %v %v", fi, err)
	}
	// The login: the site user's uid, its group, the gate as its shell.
	u, ok := e.acc.User(alias)
	if !ok {
		t.Fatal("no shell login")
	}
	want := host.UserSpec{Name: alias, System: true, Home: e.s.RootPath, Shell: filepath.Join(e.paths.BinDir, GateName),
		PrimaryGroup: user, Groups: []string{ShellGroup}, UID: shellUID, NonUnique: true}
	if fmt.Sprint(u.Spec) != fmt.Sprint(want) {
		t.Fatalf("spec %+v, want %+v", u.Spec, want)
	}
	// src/, the site user's, for checkouts.
	if fi, err := os.Stat(filepath.Join(e.s.RootPath, SourceDir)); err != nil || !fi.IsDir() || fi.Sys().(*syscall.Stat_t).Uid != shellUID {
		t.Fatalf("src: %v %v", fi, err)
	}
	// SFTP access is off on the site: the shell login is denied too.
	if !e.inDeny(t, alias) {
		t.Fatal("the shell login is not in the deny group while SFTP is off")
	}
	// The user manager of the uid is masked.
	mask := filepath.Join(e.paths.SystemdDir, fmt.Sprintf("user@%d.service", shellUID))
	if l, err := os.Readlink(mask); err != nil || l != os.DevNull {
		t.Fatalf("mask %q %v", l, err)
	}
	if !slices.Contains(e.sd.Ops(), "daemon-reload") {
		t.Fatal("no daemon-reload after the mask")
	}

	// A repeated enable repairs and changes nothing; the drop-in is not
	// rewritten (no second sshd -t).
	e.passwd(t, fmt.Sprintf("%s:x:%d:%d::/r:/bin/false", user, shellUID, shellUID), fmt.Sprintf("%s:x:%d:%d::/r:/x", alias, shellUID, shellUID))
	n := len(e.ex.Calls())
	if _, err := e.set(t, true); err != nil {
		t.Fatalf("second enable: %v", err)
	}
	if len(e.ex.Calls()) != n {
		t.Fatalf("second enable ran %v", e.ex.Calls()[n:])
	}
	// A login of that name with another uid is not this site's.
	e.passwd(t, fmt.Sprintf("%s:x:%d:%d::/r:/x", alias, 4321, 4321))
	_, err = e.set(t, true)
	wantCode(t, "a foreign login", err, agentrpc.CodeConflict)
}

func TestSetShellWithSFTPOn(t *testing.T) {
	e := newShellOps(t)
	e.s.Config.SFTPEnabled = true
	if _, err := e.set(t, true); err != nil {
		t.Fatal(err)
	}
	if e.inDeny(t, alias) {
		t.Fatal("SFTP on, yet the shell login is denied")
	}
	// Turning SFTP off denies the shell login and ends its shells.
	e.sd.SetUnit(host.UnitProps{Name: alias + "-77.service", ActiveState: "active"})
	if _, err := e.o.SetSFTP(ctx, rpc.SFTPParams{Site: e.s, Enable: false}); err != nil {
		t.Fatal(err)
	}
	if !e.inDeny(t, alias) {
		t.Fatal("SFTP off left the shell login in")
	}
	if !e.sd.Did("stop", alias+"-77.service") {
		t.Fatalf("the open shell was not stopped: %v", e.sd.Ops())
	}
	if _, err := e.o.SetSFTP(ctx, rpc.SFTPParams{Site: e.s, Enable: true, Password: "a-long-password-1"}); err != nil {
		t.Fatal(err)
	}
	if e.inDeny(t, alias) {
		t.Fatal("SFTP on again, yet the shell login is denied")
	}
}

func TestSetShellDisable(t *testing.T) {
	e := newShellOps(t)
	if _, err := e.set(t, true); err != nil {
		t.Fatal(err)
	}
	e.passwd(t, fmt.Sprintf("%s:x:%d:%d::/r:/bin/false", user, shellUID, shellUID), fmt.Sprintf("%s:x:%d:%d::/r:/x", alias, shellUID, shellUID))
	e.sd.SetUnit(host.UnitProps{Name: alias + "-91.service", ActiveState: "active"})
	res, err := e.set(t, false)
	if err != nil || res["shell"] != "disabled" {
		t.Fatalf("%v %v", res, err)
	}
	if !e.sd.Did("stop", alias+"-91.service") {
		t.Fatal("the open shell was not stopped")
	}
	if _, ok := e.acc.User(alias); ok {
		t.Fatal("the shell login is still there")
	}
	if !slices.Contains(e.acc.Ops(), "userdel --force "+alias) {
		t.Fatalf("not removed with --force: %v", e.acc.Ops())
	}
	if _, ok := e.acc.User(user); !ok {
		t.Fatal("the site user went with its shell login")
	}
	mask := filepath.Join(e.paths.SystemdDir, fmt.Sprintf("user@%d.service", shellUID))
	if _, err := os.Lstat(mask); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the mask stayed: %v", err)
	}
	// Disabling again, or removing the site's shell, finds nothing to do.
	if _, err := e.set(t, false); err != nil {
		t.Fatal(err)
	}
	if err := e.o.RemoveShell(ctx, e.s); err != nil {
		t.Fatal(err)
	}
}

func TestSetShellNeverKeepsARefusedDropIn(t *testing.T) {
	e := newShellOps(t)
	e.refuse = true
	if _, err := e.set(t, true); err == nil {
		t.Fatal("enabled with sshd refusing its configuration")
	}
	if _, err := os.Lstat(e.paths.DropIn); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the refused drop-in stayed: %v", err)
	}
	if _, ok := e.acc.User(alias); ok {
		t.Fatal("a shell login was made with sshd refusing its configuration")
	}
}

func TestMaskLeavesAnOperatorsUnit(t *testing.T) {
	e := newShellOps(t)
	mask := filepath.Join(e.paths.SystemdDir, fmt.Sprintf("user@%d.service", shellUID))
	if err := os.WriteFile(mask, []byte("[Service]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.o.maskUserManager(ctx, shellUID, true); err != nil {
		t.Fatal(err)
	}
	if err := e.o.maskUserManager(ctx, shellUID, false); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(mask); err != nil || string(b) != "[Service]\n" {
		t.Fatalf("the operator's unit was touched: %q %v", b, err)
	}
}

func TestShellKeyTypes(t *testing.T) {
	o, _, s := newOps(t)
	sftpKey, _ := ed25519Key(t, "laptop")
	shellKey, _ := ed25519Key(t, "deploy")
	if _, err := o.SSHKeys(ctx, rpc.SSHKeyParams{Site: s, Action: "add", PublicKey: sftpKey}); err != nil {
		t.Fatal(err)
	}
	res, err := o.SSHKeys(ctx, rpc.SSHKeyParams{Site: s, Action: "add", PublicKey: shellKey, Access: AccessShell})
	if err != nil {
		t.Fatal(err)
	}
	if res.ShellUser != alias {
		t.Fatalf("shell user %q", res.ShellUser)
	}
	access := map[string]string{}
	for _, k := range res.Keys {
		access[k.Label] = k.Access
	}
	if access["laptop"] != AccessSFTP || access["deploy"] != AccessShell {
		t.Fatalf("access %v", access)
	}
	// Every key signs in to SFTP; only the shell key is in shell_keys.
	ak, _ := os.ReadFile(filepath.Join(s.RootPath, keysFile))
	sk, _ := os.ReadFile(filepath.Join(s.RootPath, shellKeysFile))
	if !strings.Contains(string(ak), sftpKey) || !strings.Contains(string(ak), shellKey) {
		t.Fatalf("authorized_keys %q", ak)
	}
	if strings.TrimSpace(string(sk)) != shellKey {
		t.Fatalf("shell_keys %q", sk)
	}
	if fi, err := os.Stat(filepath.Join(s.RootPath, shellKeysFile)); err != nil || fi.Mode().Perm() != keysFileMode {
		t.Fatalf("shell_keys mode %v %v", fi, err)
	}
	// Deleting the shell key takes it out of both files.
	var fp string
	for _, k := range res.Keys {
		if k.Label == "deploy" {
			fp = k.Fingerprint
		}
	}
	if _, err := o.SSHKeys(ctx, rpc.SSHKeyParams{Site: s, Action: "delete", Fingerprint: fp}); err != nil {
		t.Fatal(err)
	}
	sk, _ = os.ReadFile(filepath.Join(s.RootPath, shellKeysFile))
	ak, _ = os.ReadFile(filepath.Join(s.RootPath, keysFile))
	if len(sk) != 0 || strings.Contains(string(ak), shellKey) {
		t.Fatalf("after delete: shell_keys %q, authorized_keys %q", sk, ak)
	}
	// An unknown type is refused before anything is written.
	other, _ := ed25519Key(t, "x")
	_, err = o.SSHKeys(ctx, rpc.SSHKeyParams{Site: s, Action: "add", PublicKey: other, Access: "root"})
	wantCode(t, "unknown access", err, agentrpc.CodeInvalid)
}

func TestShellKeysFileIsNeverFollowed(t *testing.T) {
	o, _, s := newOps(t)
	if err := os.Mkdir(filepath.Join(s.RootPath, ".ssh"), 0o750); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(target, []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(s.RootPath, shellKeysFile)); err != nil {
		t.Fatal(err)
	}
	key, _ := ed25519Key(t, "deploy")
	if _, err := o.SSHKeys(ctx, rpc.SSHKeyParams{Site: s, Action: "add", PublicKey: key, Access: AccessShell}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != "keep\n" {
		t.Fatalf("written through the link: %q", b)
	}
	if fi, err := os.Lstat(filepath.Join(s.RootPath, shellKeysFile)); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("shell_keys is not a regular file: %v %v", fi, err)
	}
}

func waitSocket(t *testing.T, path string) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if fi, err := os.Stat(path); err == nil && fi.Mode()&os.ModeSocket != 0 && fi.Mode().Perm() == 0o660 {
			return
		}
	}
	t.Fatal("the gate socket did not appear")
}
