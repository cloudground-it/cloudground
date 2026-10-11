package state

import (
	"errors"
	"testing"
	"time"
)

// A new setup token (panel-api mints one at every start until setup is
// done) invalidates every earlier one: only the newest is valid.
func TestOnlyTheNewestSetupTokenIsValid(t *testing.T) {
	s := testStore(t)
	for _, tok := range []string{"first-start", "second-start", "third-start"} {
		if err := s.CreateSetupToken(tok, 24*time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	for _, tok := range []string{"first-start", "second-start"} {
		if err := s.ConsumeSetupToken(tok); !errors.Is(err, ErrNotFound) {
			t.Errorf("an earlier token %s = %v, want ErrNotFound", tok, err)
		}
	}
	if err := s.ConsumeSetupToken("third-start"); err != nil {
		t.Fatalf("the newest token = %v", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM setup_tokens`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d setup tokens stored (%v), want only the newest", n, err)
	}
}
