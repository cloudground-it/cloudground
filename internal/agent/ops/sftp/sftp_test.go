package sftp

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

var ctx = context.Background()

const user = "cg-site-1"

// newOps returns handlers on a fresh site tree owned by the test's user and
// a fake accounts database holding the site user.
func newOps(t *testing.T) (*Ops, *host.FakeAccounts, state.Site) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "example.com")
	if err := os.Mkdir(root, 0o750); err != nil {
		t.Fatal(err)
	}
	acc := host.NewFakeAccounts()
	if err := acc.CreateUser(ctx, host.UserSpec{Name: user, System: true}); err != nil {
		t.Fatal(err)
	}
	owner := sitefs.Owner{UID: os.Getuid(), GID: os.Getgid()}
	o := &Ops{
		Accounts: acc,
		Open:     func(s state.Site) (*sitefs.Root, error) { return sitefs.Open(s.RootPath, owner) },
		Port:     func(context.Context) int { return 2222 },
	}
	return o, acc, state.Site{Domain: "example.com", SystemUser: user, RootPath: root}
}

func ed25519Key(t *testing.T, comment string) (string, ssh.PublicKey) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pk)))
	if comment != "" {
		line += " " + comment
	}
	return line, pk
}

func wantCode(t *testing.T, what string, err error, code agentrpc.Code) {
	t.Helper()
	if got := agentrpc.CodeOf(err); err == nil || got != code {
		t.Fatalf("%s: err = %v (%s), want %s", what, err, got, code)
	}
}

func member(t *testing.T, acc *host.FakeAccounts) bool {
	t.Helper()
	ok, err := acc.IsMember(ctx, user, DenyGroup)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// SFTP on/off is membership of the deny group; both are idempotent. Off
// locks the password, on sets it (which unlocks).
func TestSetSFTPTogglesAccessAndPassword(t *testing.T) {
	o, acc, s := newOps(t)
	for i := 0; i < 2; i++ {
		if _, err := o.SetSFTP(ctx, rpc.SFTPParams{Site: s}); err != nil {
			t.Fatalf("disable #%d: %v", i, err)
		}
		if u, _ := acc.User(user); !member(t, acc) || !u.Locked {
			t.Fatalf("disabled: in group %v, locked %v", member(t, acc), u.Locked)
		}
	}
	for i := 0; i < 2; i++ {
		out, err := o.SetSFTP(ctx, rpc.SFTPParams{Site: s, Enable: true, Password: "a-long-enough-pw"})
		if err != nil {
			t.Fatalf("enable #%d: %v", i, err)
		}
		if out["sftp"] != "enabled" || out["user"] != user {
			t.Fatalf("result %v", out)
		}
		if u, _ := acc.User(user); member(t, acc) || u.Locked || u.Password != "a-long-enough-pw" {
			t.Fatalf("enabled: in group %v, user %+v", member(t, acc), u)
		}
	}
	groupadds := 0
	for _, op := range acc.Ops() {
		if strings.HasPrefix(op, "groupadd") {
			groupadds++
		}
	}
	if groupadds != 2 || !acc.HasGroup(DenyGroup) || !acc.HasGroup(KeysGroup) {
		t.Fatalf("groups ensured %d times, want once each", groupadds)
	}
}

func TestSetSFTPRefusesBadPasswords(t *testing.T) {
	o, acc, s := newOps(t)
	o.SetAccess(ctx, user, false)
	for _, pw := range []string{"", "short", "elevenchars"} {
		_, err := o.SetSFTP(ctx, rpc.SFTPParams{Site: s, Enable: true, Password: pw})
		wantCode(t, fmt.Sprintf("password %q", pw), err, agentrpc.CodeInvalid)
	}
	_, err := o.SetSFTP(ctx, rpc.SFTPParams{Site: s, Enable: true, Password: "long-enough-pw\nroot:x"})
	wantCode(t, "password with a newline", err, agentrpc.CodeInvalid)
	if !member(t, acc) {
		t.Fatal("a refused password still opened SFTP")
	}
}

// Enabling must not report success while the user is still denied.
func TestSetAccessReportsAccountErrors(t *testing.T) {
	o, acc, _ := newOps(t)
	if err := o.SetAccess(ctx, "cg-site-9", true); !errors.Is(err, host.ErrNoSuchUser) {
		t.Fatalf("missing user: %v", err)
	}
	o2 := &Ops{Accounts: acc}
	acc.FailNext = errors.New("groupadd: exit status 10")
	if err := o2.SetAccess(ctx, user, false); err == nil {
		t.Fatal("a failed groupadd was swallowed")
	}
	if err := o2.SetAccess(ctx, user, false); err != nil || !member(t, acc) {
		t.Fatalf("the group was not retried: %v", err)
	}
	if err := o.SetAccess(ctx, user, true); err != nil {
		t.Fatal(err)
	}
	if err := o.SetAccess(ctx, user, true); err != nil {
		t.Fatalf("allowing twice: %v", err)
	}
	// The group removed behind the agent's back is made again.
	if err := acc.DeleteGroup(ctx, DenyGroup); err != nil {
		t.Fatal(err)
	}
	if err := o.SetAccess(ctx, user, false); err != nil || !member(t, acc) {
		t.Fatalf("after the group was removed: %v", err)
	}
}

func TestParseKey(t *testing.T) {
	line, pk := ed25519Key(t, "dev@laptop  with\tspaces\x1b[31m")
	k, err := parseKey(line)
	if err != nil {
		t.Fatal(err)
	}
	if k.fingerprint() != ssh.FingerprintSHA256(pk) {
		t.Fatalf("fingerprint %s, ssh-keygen's form %s", k.fingerprint(), ssh.FingerprintSHA256(pk))
	}
	if k.label != "dev@laptop with spaces[31m" || k.typ != "ssh-ed25519" {
		t.Fatalf("parsed %+v", k)
	}
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	epk, _ := ssh.NewPublicKey(&ec.PublicKey)
	if _, err := parseKey(string(ssh.MarshalAuthorizedKey(epk))); err != nil {
		t.Fatalf("ecdsa: %v", err)
	}
	long, _ := ed25519Key(t, strings.Repeat("é", 100))
	if k, err := parseKey(long); err != nil || len(k.label) > maxLabel || !strings.HasPrefix(k.label, "éé") || strings.ContainsRune(k.label, '�') {
		t.Fatalf("long label: %q, %v", k.label, err)
	}

	data := strings.Fields(line)[1]
	bad := []string{
		"",
		"ssh-ed25519",
		"ssh-dss " + data,
		`command="/bin/sh" ssh-ed25519 ` + data,
		"ssh-ed25519 !!!notbase64",
		"ssh-ed25519 a2tra2tra2tra2tra2tra2tra2tra2tra2tra2tra2tra2tra2tra2tra2tr", // not a key
		"ssh-rsa " + data, // the blob is an ed25519 key
		line + "\n" + line,
	}
	for _, b := range bad {
		if _, err := parseKey(b); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
			t.Errorf("parseKey(%q) = %v, want invalid", b, err)
		}
	}
}

func TestSSHKeysWorkflow(t *testing.T) {
	o, acc, s := newOps(t)
	line, pk := ed25519Key(t, "dev@laptop")
	res, err := o.SSHKeys(ctx, rpc.SSHKeyParams{Site: s, Action: "add", PublicKey: line})
	if err != nil || len(res.Keys) != 1 || res.Keys[0].Label != "dev@laptop" || res.Port != 2222 {
		t.Fatalf("add = %+v, %v", res, err)
	}
	if res.Keys[0].Fingerprint != ssh.FingerprintSHA256(pk) {
		t.Fatalf("fingerprint %s, want %s", res.Keys[0].Fingerprint, ssh.FingerprintSHA256(pk))
	}
	dup := strings.Join(strings.Fields(line)[:2], " ") + " another-label"
	_, err = o.SSHKeys(ctx, rpc.SSHKeyParams{Site: s, Action: "add", PublicKey: dup})
	wantCode(t, "the same key again", err, agentrpc.CodeConflict)

	// SFTP is off: a key change leaves the password locked, never empty.
	if u, _ := acc.User(user); !u.Locked {
		t.Fatal("adding a key with SFTP off left the password unlocked")
	}

	// Lines the agent did not write are kept, and not listed.
	kf := filepath.Join(s.RootPath, ".ssh/authorized_keys")
	b, _ := os.ReadFile(kf)
	other, _ := ed25519Key(t, "")
	os.WriteFile(kf, append(b, []byte("# operator note\nno-pty "+other+"\n")...), 0o640)
	list, err := o.SSHKeys(ctx, rpc.SSHKeyParams{Site: s, Action: "list"})
	if err != nil || len(list.Keys) != 1 {
		t.Fatalf("list = %+v, %v", list, err)
	}

	_, err = o.SSHKeys(ctx, rpc.SSHKeyParams{Site: s, Action: "delete", Fingerprint: "SHA256:nope"})
	wantCode(t, "delete of an unknown key", err, agentrpc.CodeNotFound)
	_, err = o.SSHKeys(ctx, rpc.SSHKeyParams{Site: s, Action: "delete"})
	wantCode(t, "delete without a fingerprint", err, agentrpc.CodeNotFound)
	gone, err := o.SSHKeys(ctx, rpc.SSHKeyParams{Site: s, Action: "delete", Fingerprint: list.Keys[0].Fingerprint})
	if err != nil || len(gone.Keys) != 0 {
		t.Fatalf("delete = %+v, %v", gone, err)
	}
	if b, _ := os.ReadFile(kf); string(b) != "# operator note\nno-pty "+other+"\n" {
		t.Fatalf("authorized_keys after delete: %q", b)
	}
	_, err = o.SSHKeys(ctx, rpc.SSHKeyParams{Site: s, Action: "rotate"})
	if e := agentrpc.AsError(err); e == nil || e.Message != `unsupported SSH key action "rotate"` {
		t.Fatalf("unknown action: %v", err)
	}
	_, err = o.SSHKeys(ctx, rpc.SSHKeyParams{Site: s, Action: "add", PublicKey: "ssh-dss AAAA"})
	wantCode(t, "unsupported key", err, agentrpc.CodeInvalid)
}

// A key change with SFTP on does not touch the password.
func TestSSHKeysLeaveAnEnabledPasswordAlone(t *testing.T) {
	o, acc, s := newOps(t)
	if _, err := o.SetSFTP(ctx, rpc.SFTPParams{Site: s, Enable: true, Password: "a-long-enough-pw"}); err != nil {
		t.Fatal(err)
	}
	s.Config.SFTPEnabled = true
	line, _ := ed25519Key(t, "")
	if _, err := o.SSHKeys(ctx, rpc.SSHKeyParams{Site: s, Action: "add", PublicKey: line}); err != nil {
		t.Fatal(err)
	}
	if u, _ := acc.User(user); u.Locked || u.Password != "a-long-enough-pw" {
		t.Fatalf("user after a key change with SFTP on: %+v", u)
	}
}

// The old agent showed fingerprints in the URL-safe alphabet; a delete with
// that form still finds the key.
func TestDeleteAcceptsTheOldFingerprintForm(t *testing.T) {
	o, _, s := newOps(t)
	var line string
	var k pubKey
	for {
		line, _ = ed25519Key(t, "")
		k, _ = parseKey(line)
		if k.fingerprint() != k.legacyFingerprint() {
			break
		}
	}
	if _, err := o.SSHKeys(ctx, rpc.SSHKeyParams{Site: s, Action: "add", PublicKey: line}); err != nil {
		t.Fatal(err)
	}
	res, err := o.SSHKeys(ctx, rpc.SSHKeyParams{Site: s, Action: "delete", Fingerprint: k.legacyFingerprint()})
	if err != nil || len(res.Keys) != 0 {
		t.Fatalf("delete by the old form = %+v, %v", res, err)
	}
}

// The key files are root's and the site group's: .ssh 0750, the file 0640.
// A symlink planted as authorized_keys is replaced, never written through.
func TestSSHKeysNeverWriteThroughALink(t *testing.T) {
	o, _, s := newOps(t)
	victim := filepath.Join(t.TempDir(), "root_authorized_keys")
	os.WriteFile(victim, []byte("ssh-ed25519 AAAA root@host\n"), 0o600)
	dir := filepath.Join(s.RootPath, ".ssh")
	os.Mkdir(dir, 0o777)
	if err := os.Symlink(victim, filepath.Join(dir, "authorized_keys")); err != nil {
		t.Fatal(err)
	}
	line, _ := ed25519Key(t, "")
	if _, err := o.SSHKeys(ctx, rpc.SSHKeyParams{Site: s, Action: "add", PublicKey: line}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "ssh-ed25519 AAAA root@host\n" {
		t.Fatalf("the link's target was written: %q", b)
	}
	fi, err := os.Lstat(filepath.Join(dir, "authorized_keys"))
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != keysFileMode {
		t.Fatalf("authorized_keys: %v %v", fi, err)
	}
	dfi, _ := os.Lstat(dir)
	if dfi.Mode().Perm() != sshDirMode {
		t.Fatalf(".ssh mode %v", dfi.Mode().Perm())
	}
	if os.Getuid() == 0 {
		for _, p := range []string{dir, filepath.Join(dir, "authorized_keys")} {
			st, _ := os.Lstat(p)
			if sys := st.Sys().(*syscall.Stat_t); sys.Uid != 0 || int(sys.Gid) != os.Getgid() {
				t.Fatalf("%s owned by %d:%d, want root and the site group", p, sys.Uid, sys.Gid)
			}
		}
	}

	// A file in .ssh's place is refused, not followed or replaced.
	o2, _, s2 := newOps(t)
	os.WriteFile(filepath.Join(s2.RootPath, ".ssh"), []byte("x"), 0o644)
	_, err = o2.SSHKeys(ctx, rpc.SSHKeyParams{Site: s2, Action: "list"})
	wantCode(t, ".ssh is a file", err, agentrpc.CodeConflict)
}

// Concurrent adds on one site all land: each change reads and rewrites the
// whole file, under the site's key lock.
func TestConcurrentAddsKeepEveryKey(t *testing.T) {
	o, _, s := newOps(t)
	const n = 16
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		line, _ := ed25519Key(t, fmt.Sprintf("k%d", i))
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := o.SSHKeys(ctx, rpc.SSHKeyParams{Site: s, Action: "add", PublicKey: line})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	res, err := o.SSHKeys(ctx, rpc.SSHKeyParams{Site: s, Action: "list"})
	if err != nil || len(res.Keys) != n {
		t.Fatalf("%d keys after %d concurrent adds (%v)", len(res.Keys), n, err)
	}
}

func inKeys(t *testing.T, acc *host.FakeAccounts) bool {
	t.Helper()
	ok, err := acc.IsMember(ctx, user, KeysGroup)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// #47: keys only locks the password and joins the keys-only group before
// access opens; a password with it is refused and changes nothing; a
// password afterwards leaves the group and unlocks.
func TestSetSFTPKeysOnly(t *testing.T) {
	o, acc, s := newOps(t)
	if _, err := o.SetSFTP(ctx, rpc.SFTPParams{Site: s, Enable: true, Password: "a-long-enough-pw"}); err != nil {
		t.Fatal(err)
	}
	_, err := o.SetSFTP(ctx, rpc.SFTPParams{Site: s, Enable: true, KeysOnly: true, Password: "a-long-enough-pw"})
	wantCode(t, "keys only with a password", err, agentrpc.CodeInvalid)
	if u, _ := acc.User(user); u.Locked || inKeys(t, acc) {
		t.Fatalf("a refused keys-only call changed the account: %+v, keys-only %v", u, inKeys(t, acc))
	}
	for i := 0; i < 2; i++ {
		out, err := o.SetSFTP(ctx, rpc.SFTPParams{Site: s, Enable: true, KeysOnly: true})
		if err != nil {
			t.Fatalf("keys only #%d: %v", i, err)
		}
		if out["sftp"] != "enabled" || out["auth"] != "keys" {
			t.Fatalf("result %v", out)
		}
		if u, _ := acc.User(user); !u.Locked || !inKeys(t, acc) || member(t, acc) {
			t.Fatalf("keys only: %+v, keys-only %v, denied %v", u, inKeys(t, acc), member(t, acc))
		}
	}
	out, err := o.SetSFTP(ctx, rpc.SFTPParams{Site: s, Enable: true, Password: "another-long-pw"})
	if err != nil || out["auth"] != "password" {
		t.Fatalf("back to a password: %v %v", out, err)
	}
	if u, _ := acc.User(user); u.Locked || u.Password != "another-long-pw" || inKeys(t, acc) || member(t, acc) {
		t.Fatalf("password: %+v, keys-only %v, denied %v", u, inKeys(t, acc), member(t, acc))
	}
}

// #47: the keys-only drop-in is written and checked with sshd -t before the
// password is locked; one sshd refuses leaves no file and the login as it
// was.
func TestKeysOnlyDropIn(t *testing.T) {
	e := newShellOps(t)
	if _, err := e.o.SetSFTP(ctx, rpc.SFTPParams{Site: e.s, Enable: true, Password: "a-long-enough-pw"}); err != nil {
		t.Fatal(err)
	}
	e.refuse = true
	if _, err := e.o.SetSFTP(ctx, rpc.SFTPParams{Site: e.s, Enable: true, KeysOnly: true}); err == nil {
		t.Fatal("keys only went through with sshd refusing the drop-in")
	}
	if _, err := os.Stat(e.paths.KeysDropIn); !os.IsNotExist(err) {
		t.Fatalf("the refused drop-in is still there: %v", err)
	}
	if u, _ := e.acc.User(user); u.Locked || inKeys(t, e.acc) {
		t.Fatalf("a refused drop-in changed the login: %+v", u)
	}
	e.refuse = false
	if _, err := e.o.SetSFTP(ctx, rpc.SFTPParams{Site: e.s, Enable: true, KeysOnly: true}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(e.paths.KeysDropIn)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Match Group " + KeysGroup + "\n", "    PasswordAuthentication no\n", "    KbdInteractiveAuthentication no\n"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("drop-in lacks %q:\n%s", want, b)
		}
	}
	if filepath.Base(e.paths.KeysDropIn) >= "10-cloudground-sftp.conf" || filepath.Base(DefaultShellPaths().KeysDropIn) >= "10-cloudground-sftp.conf" {
		t.Error("the keys-only drop-in must sort before 10-cloudground-sftp.conf")
	}
}
