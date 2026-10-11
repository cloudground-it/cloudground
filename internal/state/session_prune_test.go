package state

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// Starting a session deletes the expired and idle sessions of every
// account, and the account list shows only the sessions that still work.
// An account keeps at most MaxSessionsPerUser sessions: the newest one ends
// the oldest.
func TestSessionsArePrunedAndCapped(t *testing.T) {
	s := testStore(t)
	a, _ := s.CreateUser("a@example.com", "hash", "admin")
	b, _ := s.CreateUser("b@example.com", "hash", "admin")
	setSeen := func(token string, ago time.Duration) {
		t.Helper()
		if _, err := s.db.Exec(`UPDATE sessions SET last_seen_at = ? WHERE token_hash = ?`, stamp(time.Now().Add(-ago)), TokenHash(token)); err != nil {
			t.Fatal(err)
		}
	}
	count := func(userID int64) int {
		t.Helper()
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id = ?`, userID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, tok := range []string{"b-live", "b-idle", "b-expired"} {
		ttl := time.Hour
		if tok == "b-expired" {
			ttl = -time.Minute
		}
		if err := s.CreateSession(tok, b.ID, ttl); err != nil {
			t.Fatal(err)
		}
	}
	setSeen("b-idle", SessionIdleTimeout+time.Minute)

	// The list leaves the idle and the expired one out, while both are
	// still stored.
	list, err := s.ListSessionsForUser(b.ID)
	if err != nil || len(list) != 1 || list[0].TokenHash != TokenHash("b-live") {
		t.Fatalf("b's sessions = %+v, %v; want only b-live", list, err)
	}
	if n := count(b.ID); n != 3 {
		t.Fatalf("b has %d stored sessions before a sign-in, want 3", n)
	}
	// Another account's sign-in tidies b's too.
	if err := s.CreateSession("a-0", a.ID, time.Hour); err != nil {
		t.Fatal(err)
	}
	if n := count(b.ID); n != 1 {
		t.Fatalf("b has %d stored sessions after a's sign-in, want 1", n)
	}

	for i := 1; i <= MaxSessionsPerUser; i++ {
		if err := s.CreateSession(fmt.Sprintf("a-%d", i), a.ID, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	if n := count(a.ID); n != MaxSessionsPerUser {
		t.Fatalf("a has %d sessions, want %d", n, MaxSessionsPerUser)
	}
	if _, err := s.PeekSessionUser("a-0"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the oldest session survived the cap: %v", err)
	}
	if _, err := s.PeekSessionUser(fmt.Sprintf("a-%d", MaxSessionsPerUser)); err != nil {
		t.Fatalf("the newest session was ended: %v", err)
	}
	if _, err := s.PeekSessionUser("b-live"); err != nil {
		t.Fatalf("a's cap ended b's session: %v", err)
	}
}
