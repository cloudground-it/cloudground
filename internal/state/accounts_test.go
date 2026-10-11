package state

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

// fileHolds reports whether the database file or its WAL contains needle.
func fileHolds(t *testing.T, path, needle string) bool {
	t.Helper()
	for _, p := range []string{path, path + "-wal"} {
		b, err := os.ReadFile(p)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(needle)) {
			return true
		}
	}
	return false
}

func usersColumns(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info('users')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		rows.Scan(&c)
		cols = append(cols, c)
	}
	return cols
}

// Migration 16 from a v15 database with a user who has 2FA on, a second one
// mid-enrolment and a third with both: the secrets end up sealed under the
// user's names, the users table has no column for them, the plaintext is
// gone from the file, and the second factor still checks out.
func TestTOTPSecretsMoveIntoTheSecretStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	old, err := openAtVersion(path, 15)
	if err != nil {
		t.Fatal(err)
	}
	const active, pending, both, bothPending = "JBSWY3DPEHPK3PXPACTIVE", "JBSWY3DPEHPK3PXPPENDNG", "KRSXG5BAMJXXIIDBMJXXIBTH", "KRSXG5BAMJXXIIDBNEWPEND"
	for _, u := range []struct {
		email, secret, pending string
		on                     int
	}{
		{"on@example.com", active, "", 1},
		{"enrolling@example.com", "", pending, 0},
		{"both@example.com", both, bothPending, 1},
		{"off@example.com", "", "", 0},
	} {
		if _, err := old.db.Exec(`INSERT INTO users (email, password_hash, role, created_at, totp_secret, totp_pending, totp_enabled, totp_last_step)
			VALUES (?, 'h', 'admin', '2026-10-01T00:00:00Z', ?, ?, ?, 7)`, u.email, u.secret, u.pending, u.on); err != nil {
			t.Fatal(err)
		}
	}
	old.Close()
	if !fileHolds(t, path, active) {
		t.Fatal("fixture: the v15 file does not hold the plaintext secret")
	}

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, c := range usersColumns(t, s) {
		if c == "totp_secret" || c == "totp_pending" {
			t.Fatalf("users still has the column %s", c)
		}
	}
	if held, _ := hasTable(s.db, totpImportTable); held {
		t.Fatal("the holding table survived an Open with the key")
	}
	for _, secret := range []string{active, pending, both, bothPending} {
		if fileHolds(t, path, secret) {
			t.Fatalf("the plaintext secret %s is still in the database file", secret)
		}
	}
	id := func(email string) int64 {
		u, err := s.GetUserByEmail(email)
		if err != nil {
			t.Fatal(err)
		}
		return u.ID
	}
	if got, on, err := s.UserTOTP(id("on@example.com")); got != active || !on || err != nil {
		t.Fatalf("UserTOTP = %q %v %v, want the migrated secret", got, on, err)
	}
	if u, _ := s.GetUserByEmail("on@example.com"); !u.TOTPEnabled {
		t.Fatal("the account lost its 2FA flag")
	}
	// The replay watermark is not reset by the move.
	if fresh, _ := s.MarkTOTPStepUsed(id("on@example.com"), 7); fresh {
		t.Fatal("the replay watermark was reset")
	}
	if got, _ := s.PendingTOTP(id("enrolling@example.com")); got != pending {
		t.Fatalf("pending = %q, want the migrated enrolment", got)
	}
	if _, on, _ := s.UserTOTP(id("enrolling@example.com")); on {
		t.Fatal("an enrolment in progress turned 2FA on")
	}
	if got, _, _ := s.UserTOTP(id("both@example.com")); got != both {
		t.Fatalf("active = %q", got)
	}
	if got, _ := s.PendingTOTP(id("both@example.com")); got != bothPending {
		t.Fatalf("pending = %q", got)
	}
	if err := s.ActivatePendingTOTP(id("enrolling@example.com"), pending); err != nil {
		t.Fatal(err)
	}
	if got, on, _ := s.UserTOTP(id("enrolling@example.com")); got != pending || !on {
		t.Fatalf("after confirming the migrated enrolment: %q %v", got, on)
	}
	// A second Open finds nothing to move.
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.UserTOTP(id("on@example.com")); got != active {
		t.Fatalf("after reopening: %q", got)
	}
}

// A crash after the secrets were sealed and before the file was compacted:
// the emptied holding table is still there, so the next Open compacts and
// only then drops it.
func TestTOTPCompactionSurvivesACrash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	old, err := openAtVersion(path, 15)
	if err != nil {
		t.Fatal(err)
	}
	old.db.Exec(`INSERT INTO users (email, password_hash, role, created_at, totp_secret, totp_enabled)
		VALUES ('a@example.com', 'h', 'admin', '2026-10-01T00:00:00Z', 'CRASHPLAINTEXTSECRET', 1)`)
	old.Close()
	half, err := openAtVersion(path, latestSchema)
	if err != nil {
		t.Fatal(err)
	}
	if err := half.loadOrCreateKey(filepath.Join(filepath.Dir(path), "secret.key"), path); err != nil {
		t.Fatal(err)
	}
	if held, err := half.moveLegacyTOTP(); !held || err != nil {
		t.Fatalf("move = %v %v", held, err)
	}
	half.Close() // no compaction: the crash
	if held, _ := func() (bool, error) {
		s, err := openAtVersion(path, latestSchema)
		if err != nil {
			return false, err
		}
		defer s.Close()
		return hasTable(s.db, totpImportTable)
	}(); !held {
		t.Fatal("the holding table went before the compaction")
	}

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if held, _ := hasTable(s.db, totpImportTable); held {
		t.Fatal("the holding table survived the compacting Open")
	}
	if fileHolds(t, path, "CRASHPLAINTEXTSECRET") {
		t.Fatal("plaintext left in the file after the crash")
	}
	u, _ := s.GetUserByEmail("a@example.com")
	if got, _, _ := s.UserTOTP(u.ID); got != "CRASHPLAINTEXTSECRET" {
		t.Fatalf("secret = %q", got)
	}
}

// Without the key (host recovery), the migration still runs, and the held
// secrets wait for the panel's Open. Turning 2FA off there removes the held
// secret, so the later Open does not seal it back.
func TestTOTPMigrationWithoutTheKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	old, err := openAtVersion(path, 15)
	if err != nil {
		t.Fatal(err)
	}
	for _, email := range []string{"keep@example.com", "lost@example.com"} {
		old.db.Exec(`INSERT INTO users (email, password_hash, role, created_at, totp_secret, totp_enabled)
			VALUES (?, 'h', 'admin', '2026-10-01T00:00:00Z', ?, 1)`, email, "SECRETOF"+email[:4])
	}
	old.Close()
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "secret.key")); !os.IsNotExist(err) {
		t.Fatal("fixture: a key file exists")
	}

	rec, err := OpenWith(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	lost, _ := rec.GetUserByEmail("lost@example.com")
	if !lost.TOTPEnabled {
		t.Fatal("2FA reads as off before the secret is sealed")
	}
	if _, on, err := rec.UserTOTP(lost.ID); !on || !errors.Is(err, ErrNoSecretKey) {
		t.Fatalf("UserTOTP without a key = %v %v, want on and ErrNoSecretKey", on, err)
	}
	if err := rec.DisableUserTOTP(lost.ID); err != nil {
		t.Fatal(err)
	}
	rec.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	keep, _ := s.GetUserByEmail("keep@example.com")
	if got, on, err := s.UserTOTP(keep.ID); got != "SECRETOFkeep" || !on || err != nil {
		t.Fatalf("held secret = %q %v %v", got, on, err)
	}
	if got, _ := s.Secret(UserSecret(lost.ID, totpActive)); got != "" {
		t.Fatal("a secret turned off during recovery came back")
	}
	if fileHolds(t, path, "SECRETOF") {
		t.Fatal("plaintext left in the file")
	}
}

func TestTOTPLifecycle(t *testing.T) {
	s, path := newStore(t)
	u, _ := s.CreateUser("a@example.com", "h", "admin")
	if _, on, err := s.UserTOTP(u.ID); on || err != nil {
		t.Fatalf("fresh account: %v %v", on, err)
	}
	if err := s.ActivatePendingTOTP(u.ID, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("activate with nothing pending = %v", err)
	}
	if err := s.SetPendingTOTP(u.ID, "PLAINPENDINGSECRETXYZ"); err != nil {
		t.Fatal(err)
	}
	if fileHolds(t, path, "PLAINPENDINGSECRETXYZ") {
		t.Fatal("the pending secret is in the file in plaintext")
	}
	if _, on, _ := s.UserTOTP(u.ID); on {
		t.Fatal("enrolling turned 2FA on")
	}
	s.MarkTOTPStepUsed(u.ID, 100)
	if err := s.ActivatePendingTOTP(u.ID, "PLAINPENDINGSECRETXYZ"); err != nil {
		t.Fatal(err)
	}
	if got, on, _ := s.UserTOTP(u.ID); got != "PLAINPENDINGSECRETXYZ" || !on {
		t.Fatalf("active = %q %v", got, on)
	}
	if got, _ := s.PendingTOTP(u.ID); got != "" {
		t.Fatal("the pending secret stayed after activation")
	}
	if fresh, _ := s.MarkTOTPStepUsed(u.ID, 1); !fresh {
		t.Fatal("activation did not reset the replay watermark")
	}
	if fresh, _ := s.MarkTOTPStepUsed(u.ID, 1); fresh {
		t.Fatal("a step was accepted twice")
	}
	// On with no secret is an error, not "off".
	s.DeleteSecret(UserSecret(u.ID, totpActive))
	if _, on, err := s.UserTOTP(u.ID); !on || err == nil {
		t.Fatalf("missing secret = %v %v", on, err)
	}
	s.SetPendingTOTP(u.ID, "ANOTHERSECRET")
	if err := s.DisableUserTOTP(u.ID); err != nil {
		t.Fatal(err)
	}
	if _, on, err := s.UserTOTP(u.ID); on || err != nil {
		t.Fatalf("after disable: %v %v", on, err)
	}
	if got, _ := s.PendingTOTP(u.ID); got != "" {
		t.Fatal("disable left the pending secret")
	}
}

func TestDeleteUserTakesItsSecrets(t *testing.T) {
	s, _ := newStore(t)
	keep, _ := s.CreateUser("keep@example.com", "h", "admin")
	gone, _ := s.CreateUser("gone@example.com", "h", "readonly")
	s.SetPendingTOTP(gone.ID, "GONESECRET")
	s.ActivatePendingTOTP(gone.ID, "GONESECRET")
	s.SetPendingTOTP(gone.ID, "GONEPENDING")
	s.SetPendingTOTP(keep.ID, "KEEPSECRET")
	if err := s.DeleteUser(gone.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM secrets WHERE name LIKE 'user.%'`).Scan(&n)
	if n != 1 {
		t.Fatalf("%d user secrets left, want only the other account's", n)
	}
	if err := s.DeleteUser(gone.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete = %v", err)
	}
	if err := s.DeleteUser(keep.ID); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("deleting the last admin = %v", err)
	}
}

func TestUpdateUserIsAllOrNothing(t *testing.T) {
	s, _ := newStore(t)
	admin, _ := s.CreateUser("admin@example.com", "h", "admin")
	site, err := s.CreateSite(Site{Domain: "a.example.com", Type: SiteStatic, Profile: ProfileBalanced, Engine: EngineNginx, SystemUser: "cg-site-1", RootPath: "/srv/sites/a.example.com", Status: SiteActive})
	if err != nil {
		t.Fatal(err)
	}
	op, err := s.CreateAccount(NewUser{Email: "op@example.com", PasswordHash: "h", Role: "operator", FirstName: "Op", SiteIDs: []int64{site.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetUser(op.ID); got.FirstName != "Op" || !slices.Equal(got.SiteIDs, []int64{site.ID}) {
		t.Fatalf("created = %+v", got)
	}
	if _, err := s.CreateAccount(NewUser{Email: "op@example.com", PasswordHash: "h", Role: "readonly"}); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("duplicate = %v", err)
	}
	if _, err := s.CreateAccount(NewUser{Email: "bad@example.com", PasswordHash: "h", Role: "operator", SiteIDs: []int64{999}}); !errors.Is(err, ErrBadSite) {
		t.Fatalf("missing site = %v", err)
	}
	if _, err := s.GetUserByEmail("bad@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatal("a refused create left the account behind")
	}
	// A bad site refuses the whole edit: role, flag and names stay.
	if err := s.UpdateUser(op.ID, UserUpdate{Role: "operator", Disabled: true, FirstName: "New", SiteIDs: []int64{site.ID, 999}}); !errors.Is(err, ErrBadSite) {
		t.Fatalf("update with a missing site = %v", err)
	}
	if got, _ := s.GetUser(op.ID); got.Disabled || got.FirstName != "Op" || len(got.SiteIDs) != 1 {
		t.Fatalf("a refused edit half-applied: %+v", got)
	}
	if err := s.UpdateUser(op.ID, UserUpdate{Role: "operator", SiteIDs: []int64{site.ID, site.ID}}); !errors.Is(err, ErrBadSite) {
		t.Fatalf("a site named twice = %v", err)
	}
	if err := s.UpdateUser(admin.ID, UserUpdate{Role: "readonly"}); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("demoting the last admin = %v", err)
	}
	if err := s.UpdateUser(9999, UserUpdate{Role: "readonly"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing account = %v", err)
	}
	if err := s.CreateSession("op-session", op.ID, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateUser(op.ID, UserUpdate{Role: "readonly", Disabled: true, FirstName: "Ada", LastName: "L"}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetUser(op.ID)
	if !got.Disabled || got.Role != "readonly" || got.FirstName != "Ada" || got.SiteIDs != nil {
		t.Fatalf("after the edit: %+v", got)
	}
	if _, err := s.UserForSession("op-session"); err == nil {
		t.Fatal("disabling left the session open")
	}
	list, _ := s.ListUsers()
	if len(list) != 2 || list[0].ID != admin.ID || list[1].SiteIDs != nil {
		t.Fatalf("list = %+v", list)
	}
}

func TestSessionsIdleAndRevocation(t *testing.T) {
	s, _ := newStore(t)
	u, _ := s.CreateUser("a@example.com", "h", "admin")
	for _, tok := range []string{"one", "two", "three"} {
		if err := s.CreateSession(tok, u.ID, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := s.UserForSession("one"); err != nil || got.ID != u.ID {
		t.Fatalf("live session: %v", err)
	}
	// Idle for over two hours: gone, however long it still had.
	s.db.Exec(`UPDATE sessions SET last_seen_at = ? WHERE token_hash = ?`, stamp(time.Now().Add(-SessionIdleTimeout-time.Minute)), TokenHash("two"))
	if _, err := s.PeekSessionUser("two"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("idle session = %v", err)
	}
	// Peeking does not count as a use.
	before := stamp(time.Now().Add(-30 * time.Minute))
	s.db.Exec(`UPDATE sessions SET last_seen_at = ? WHERE token_hash = ?`, before, TokenHash("three"))
	s.PeekSessionUser("three")
	var seen string
	s.db.QueryRow(`SELECT last_seen_at FROM sessions WHERE token_hash = ?`, TokenHash("three")).Scan(&seen)
	if seen != before {
		t.Fatal("PeekSessionUser recorded a use")
	}
	s.UserForSession("three")
	s.db.QueryRow(`SELECT last_seen_at FROM sessions WHERE token_hash = ?`, TokenHash("three")).Scan(&seen)
	if seen == before {
		t.Fatal("UserForSession did not record a use")
	}
	if err := s.RevokeOtherSessions(u.ID, "one"); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListSessionsForUser(u.ID); len(list) != 1 || list[0].TokenHash != TokenHash("one") {
		t.Fatalf("after revoking the others: %+v", list)
	}
	if err := s.SetUserPassword(u.ID, "h2"); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListSessionsForUser(u.ID); len(list) != 0 {
		t.Fatal("a password change left a session")
	}
	if err := s.SetUserPassword(9999, "h"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing account = %v", err)
	}
}
