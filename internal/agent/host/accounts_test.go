package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// shadowWithDB returns ShadowAccounts over a FakeExec and account databases
// written to a temp dir.
func shadowWithDB(t *testing.T, respond func(FakeCall) FakeReply) (*ShadowAccounts, *FakeExec) {
	t.Helper()
	dir := t.TempDir()
	passwd := filepath.Join(dir, "passwd")
	group := filepath.Join(dir, "group")
	os.WriteFile(passwd, []byte("root:x:0:0:root:/root:/bin/bash\ncg-site-1:x:998:998::/srv/sites/a:/usr/sbin/nologin\ncg-site-2:x:999:999::/srv/sites/b:/usr/sbin/nologin\n"), 0o644)
	os.WriteFile(group, []byte("root:x:0:\ncg-site-1:x:998:\ncg-sftp-off:x:997:cg-site-2,cg-site-3\nempty:x:996:\n"), 0o644)
	fx := &FakeExec{Respond: respond}
	return &ShadowAccounts{Exec: fx, PasswdFile: passwd, GroupFile: group}, fx
}

func exitWith(code int) func(FakeCall) FakeReply {
	return func(FakeCall) FakeReply { return FakeReply{Code: code, Stderr: "some localized text"} }
}

func TestAccountsArgv(t *testing.T) {
	s, fx := shadowWithDB(t, nil)
	ctx := context.Background()
	steps := []struct {
		do   func() error
		want string
	}{
		{func() error {
			return s.CreateUser(ctx, UserSpec{Name: "cg-site-9", System: true, Home: "/srv/sites/x", Shell: "/usr/sbin/nologin", Groups: []string{"www-data"}})
		}, "useradd --system --home-dir /srv/sites/x --no-create-home --shell /usr/sbin/nologin --groups www-data cg-site-9"},
		{func() error { return s.DeleteUser(ctx, "cg-site-9", true) }, "userdel --remove cg-site-9"},
		{func() error { return s.LockUser(ctx, "cg-site-1") }, "usermod --lock cg-site-1"},
		{func() error { return s.UnlockUser(ctx, "cg-site-1") }, "usermod --unlock cg-site-1"},
		{func() error { return s.ExpireUser(ctx, "cg-site-1", true) }, "usermod --expiredate 1 cg-site-1"},
		{func() error { return s.ExpireUser(ctx, "cg-site-1", false) }, "usermod --expiredate  cg-site-1"},
		{func() error { return s.EnsureGroup(ctx, "cg-sftp-off", true) }, "groupadd --force --system cg-sftp-off"},
		{func() error { return s.CreateGroup(ctx, "g1", false) }, "groupadd g1"},
		{func() error { return s.DeleteGroup(ctx, "g1") }, "groupdel g1"},
		{func() error { return s.AddToGroup(ctx, "cg-site-1", "cg-sftp-off") }, "gpasswd -a cg-site-1 cg-sftp-off"},
		{func() error { return s.RemoveFromGroup(ctx, "cg-site-2", "cg-sftp-off") }, "gpasswd -d cg-site-2 cg-sftp-off"},
	}
	for _, st := range steps {
		if err := st.do(); err != nil {
			t.Fatalf("%s: %v", st.want, err)
		}
		calls := fx.Calls()
		if got := calls[len(calls)-1].Line(); got != st.want {
			t.Fatalf("argv %q, want %q", got, st.want)
		}
	}
}

// The password goes to chpasswd on stdin, never in argv.
func TestAccountsSetPasswordUsesStdin(t *testing.T) {
	s, fx := shadowWithDB(t, nil)
	if err := s.SetPassword(context.Background(), "cg-site-1", "Pa55-word!long"); err != nil {
		t.Fatal(err)
	}
	c := fx.Calls()[0]
	if c.Line() != "chpasswd" || c.Stdin != "cg-site-1:Pa55-word!long\n" {
		t.Fatalf("call %+v", c)
	}
	if err := s.SetPassword(context.Background(), "nobody-here", "Pa55-word!long"); !errors.Is(err, ErrNoSuchUser) {
		t.Fatalf("err = %v", err)
	}
	for _, bad := range []string{"", "a\nb", "a\rb", "a\x00b"} {
		if err := s.SetPassword(context.Background(), "cg-site-1", bad); !errors.Is(err, ErrInvalidPassword) {
			t.Fatalf("%q: err = %v", bad, err)
		}
	}
	if len(fx.Calls()) != 1 {
		t.Fatal("chpasswd ran for a refused request")
	}
}

func TestAccountsInvalidNamesNeverReachATool(t *testing.T) {
	s, fx := shadowWithDB(t, nil)
	ctx := context.Background()
	for _, bad := range []string{"-o", "--help", "Root", "a b", "a:b", "", strings.Repeat("a", 33), "../x"} {
		errs := []error{
			s.CreateUser(ctx, UserSpec{Name: bad}),
			s.DeleteUser(ctx, bad, false),
			s.LockUser(ctx, bad),
			s.EnsureGroup(ctx, bad, true),
			s.AddToGroup(ctx, "cg-site-1", bad),
			s.RemoveFromGroup(ctx, bad, "cg-sftp-off"),
		}
		for _, err := range errs {
			if !errors.Is(err, ErrInvalidName) {
				t.Fatalf("%q: err = %v", bad, err)
			}
		}
	}
	if len(fx.Calls()) != 0 {
		t.Fatalf("tools ran: %v", fx.Calls())
	}
}

// Exit codes as documented and measured (SPEC.md), never stderr.
func TestAccountsExitCodes(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		code int
		do   func(s *ShadowAccounts) error
		want error
	}{
		{9, func(s *ShadowAccounts) error { return s.CreateUser(ctx, UserSpec{Name: "u"}) }, ErrUserExists},
		{6, func(s *ShadowAccounts) error { return s.CreateUser(ctx, UserSpec{Name: "u", Groups: []string{"g"}}) }, ErrNoSuchGroup},
		{4, func(s *ShadowAccounts) error { return s.CreateUser(ctx, UserSpec{Name: "u"}) }, ErrIDTaken},
		{19, func(s *ShadowAccounts) error { return s.CreateUser(ctx, UserSpec{Name: "u"}) }, ErrInvalidName},
		{12, func(s *ShadowAccounts) error { return s.CreateUser(ctx, UserSpec{Name: "u", CreateHome: true}) }, ErrHome},
		{6, func(s *ShadowAccounts) error { return s.DeleteUser(ctx, "u", false) }, ErrNoSuchUser},
		{8, func(s *ShadowAccounts) error { return s.DeleteUser(ctx, "u", false) }, ErrUserBusy},
		{6, func(s *ShadowAccounts) error { return s.LockUser(ctx, "u") }, ErrNoSuchUser},
		{6, func(s *ShadowAccounts) error { return s.ExpireUser(ctx, "u", true) }, ErrNoSuchUser},
		{9, func(s *ShadowAccounts) error { return s.CreateGroup(ctx, "g", true) }, ErrGroupExists},
		{6, func(s *ShadowAccounts) error { return s.DeleteGroup(ctx, "g") }, ErrNoSuchGroup},
		{8, func(s *ShadowAccounts) error { return s.DeleteGroup(ctx, "g") }, ErrPrimaryGroup},
	}
	for _, c := range cases {
		s, _ := shadowWithDB(t, exitWith(c.code))
		err := c.do(s)
		if !errors.Is(err, c.want) {
			t.Errorf("exit %d: err = %v, want %v", c.code, err, c.want)
		}
	}
	// An unmapped code stays the raw *ExitError.
	s, _ := shadowWithDB(t, exitWith(1))
	err := s.DeleteUser(ctx, "u", false)
	if ExitCode(err) != 1 || errors.Is(err, ErrNoSuchUser) {
		t.Fatalf("err = %v", err)
	}
}

// gpasswd exits 3 for three different conditions; the databases decide.
func TestAccountsGpasswdExit3IsClassifiedFromTheDatabases(t *testing.T) {
	ctx := context.Background()
	s, _ := shadowWithDB(t, exitWith(3))
	if err := s.RemoveFromGroup(ctx, "cg-site-1", "cg-sftp-off"); !errors.Is(err, ErrNotMember) {
		t.Fatalf("not a member: %v", err)
	}
	if err := s.RemoveFromGroup(ctx, "cg-ghost", "cg-sftp-off"); !errors.Is(err, ErrNoSuchUser) {
		t.Fatalf("no such user: %v", err)
	}
	if err := s.AddToGroup(ctx, "cg-site-1", "no-group"); !errors.Is(err, ErrNoSuchGroup) {
		t.Fatalf("no such group: %v", err)
	}
	if err := s.AddToGroup(ctx, "cg-ghost", "cg-sftp-off"); !errors.Is(err, ErrNoSuchUser) {
		t.Fatalf("add no such user: %v", err)
	}
	// Exit 3 when the databases show nothing wrong stays a raw ExitError.
	err := s.RemoveFromGroup(ctx, "cg-site-2", "cg-sftp-off")
	if ExitCode(err) != 3 || errors.Is(err, ErrNotMember) {
		t.Fatalf("member, exit 3: %v", err)
	}
}

func TestAccountsIsMember(t *testing.T) {
	s, _ := shadowWithDB(t, nil)
	ctx := context.Background()
	if ok, err := s.IsMember(ctx, "cg-site-3", "cg-sftp-off"); !ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	if ok, err := s.IsMember(ctx, "cg-site-1", "cg-sftp-off"); ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	if ok, err := s.IsMember(ctx, "cg-site-1", "empty"); ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	if _, err := s.IsMember(ctx, "cg-site-1", "nogroup"); !errors.Is(err, ErrNoSuchGroup) {
		t.Fatalf("%v", err)
	}
}

// The fake fails where the real adapter does, with the same errors.
func TestFakeAccountsMatchesTheContract(t *testing.T) {
	f := NewFakeAccounts()
	ctx := context.Background()
	if err := f.EnsureGroup(ctx, "cg-sftp-off", true); err != nil {
		t.Fatal(err)
	}
	if err := f.EnsureGroup(ctx, "cg-sftp-off", true); err != nil {
		t.Fatal("EnsureGroup is not idempotent")
	}
	if err := f.CreateGroup(ctx, "cg-sftp-off", true); !errors.Is(err, ErrGroupExists) {
		t.Fatalf("%v", err)
	}
	if err := f.AddToGroup(ctx, "cg-site-1", "cg-sftp-off"); !errors.Is(err, ErrNoSuchUser) {
		t.Fatalf("%v", err)
	}
	if err := f.CreateUser(ctx, UserSpec{Name: "cg-site-1", System: true}); err != nil {
		t.Fatal(err)
	}
	if err := f.CreateUser(ctx, UserSpec{Name: "cg-site-1"}); !errors.Is(err, ErrUserExists) {
		t.Fatalf("%v", err)
	}
	if err := f.RemoveFromGroup(ctx, "cg-site-1", "cg-sftp-off"); !errors.Is(err, ErrNotMember) {
		t.Fatalf("%v", err)
	}
	for i := 0; i < 2; i++ { // gpasswd -a is idempotent (measured)
		if err := f.AddToGroup(ctx, "cg-site-1", "cg-sftp-off"); err != nil {
			t.Fatal(err)
		}
	}
	if ok, _ := f.IsMember(ctx, "cg-site-1", "cg-sftp-off"); !ok {
		t.Fatal("not a member after AddToGroup")
	}
	if err := f.AddToGroup(ctx, "cg-site-1", "nogroup"); !errors.Is(err, ErrNoSuchGroup) {
		t.Fatalf("%v", err)
	}
	if err := f.SetPassword(ctx, "cg-site-1", "Secretpass123"); err != nil {
		t.Fatal(err)
	}
	if err := f.LockUser(ctx, "cg-site-1"); err != nil {
		t.Fatal(err)
	}
	if u, _ := f.User("cg-site-1"); !u.Locked || u.Password != "Secretpass123" {
		t.Fatalf("%+v", u)
	}
	if err := f.DeleteGroup(ctx, "cg-site-1"); !errors.Is(err, ErrPrimaryGroup) {
		t.Fatalf("%v", err)
	}
	if err := f.DeleteUser(ctx, "cg-site-1", false); err != nil {
		t.Fatal(err)
	}
	if err := f.LockUser(ctx, "cg-site-1"); !errors.Is(err, ErrNoSuchUser) {
		t.Fatalf("%v", err)
	}
	if err := f.CreateUser(ctx, UserSpec{Name: "-o"}); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("%v", err)
	}
}
