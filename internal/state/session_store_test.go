package state

import (
	"errors"
	"testing"
	"time"
)

// A session token resolves to its account until it expires; an unknown
// one does not; a setup token is good once.
func TestSessionTokensAndSetupTokenOnce(t *testing.T) {
	s := testStore(t)
	u, err := s.CreateUser("owner@example.com", "hash", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession("live-token", u.ID, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession("stale-token", u.ID, -time.Minute); err != nil {
		t.Fatal(err)
	}
	if got, err := s.UserForSession("live-token"); err != nil || got.ID != u.ID || got.Email != "owner@example.com" {
		t.Fatalf("live session = %+v, %v", got, err)
	}
	for _, tok := range []string{"stale-token", "never-issued"} {
		if _, err := s.UserForSession(tok); !errors.Is(err, ErrNotFound) {
			t.Errorf("session %s: %v, want ErrNotFound", tok, err)
		}
	}

	if err := s.CreateSetupToken("first-run", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.ConsumeSetupToken("first-run"); err != nil {
		t.Fatalf("first use: %v", err)
	}
	if err := s.ConsumeSetupToken("first-run"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second use: %v, want ErrNotFound", err)
	}
}

// A session unused for SessionIdleTimeout ends. Peeking (the event
// stream's re-checks) does not count as a use; resolving does.
func TestIdleSessionEnds(t *testing.T) {
	s := testStore(t)
	u, err := s.CreateUser("idle@example.com", "hash", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession("idle-token", u.ID, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	lastSeen := func() time.Time {
		var v string
		if err := s.db.QueryRow(`SELECT last_seen_at FROM sessions WHERE token_hash = ?`, hashToken("idle-token")).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return parseTime(v)
	}
	unusedFor := func(d time.Duration) {
		at := time.Now().UTC().Add(-d).Format(time.RFC3339Nano)
		if _, err := s.db.Exec(`UPDATE sessions SET last_seen_at = ? WHERE token_hash = ?`, at, hashToken("idle-token")); err != nil {
			t.Fatal(err)
		}
	}

	unusedFor(SessionIdleTimeout - 15*time.Minute)
	if _, err := s.PeekSessionUser("idle-token"); err != nil {
		t.Fatalf("a session within the idle limit was refused: %v", err)
	}
	if time.Since(lastSeen()) < SessionIdleTimeout-16*time.Minute {
		t.Fatal("a peek counted as a use")
	}
	if _, err := s.UserForSession("idle-token"); err != nil {
		t.Fatal(err)
	}
	if time.Since(lastSeen()) > time.Minute {
		t.Fatal("resolving the session did not record the use")
	}
	unusedFor(SessionIdleTimeout + time.Minute)
	if _, err := s.UserForSession("idle-token"); err == nil {
		t.Fatal("an idle session was accepted")
	}
}
