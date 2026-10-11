package api

import (
	"testing"
	"time"
)

// Two signed-in users of one site starting the same upload name at once:
// one gets it, the other is refused, so it can never replace the upload
// the first imports with its own grants. A user of another site, or a
// session from the panel, does not block it. An export the agent has just
// created is its maker's alone, whoever listed that name before.
func TestStudioFileClaims(t *testing.T) {
	at := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	m := &studioSessions{now: func() time.Time { return at }}
	a := &studioSession{key: "a", mode: studioLogin, siteID: 1}
	b := &studioSession{key: "b", mode: studioLogin, siteID: 1}
	other := &studioSession{key: "o", mode: studioLogin, siteID: 2}
	panel := &studioSession{key: "p", mode: studioPanel, siteID: 1}
	for _, st := range []*studioSession{a, b, other, panel} {
		st.started, st.used = at, at
		m.add(st)
	}
	if !m.claim(a, "imports", "dump.sql") {
		t.Fatal("the first claim was refused")
	}
	if m.claim(b, "imports", "dump.sql") || b.owns("imports", "dump.sql") {
		t.Fatal("a second user of the site took the same upload")
	}
	if !m.claim(a, "imports", "dump.sql") {
		t.Fatal("its owner cannot claim its own upload again")
	}
	if !m.claim(other, "imports", "dump.sql") {
		t.Fatal("a user of another site was refused the name")
	}
	if !m.claim(b, "imports", "other.sql") {
		t.Fatal("a session opened from the panel blocks every name")
	}
	b.addFile("exports", "db-20261010-000000.sql") // from a file since removed
	m.own(a, "exports", "db-20261010-000000.sql")
	if !a.owns("exports", "db-20261010-000000.sql") || b.owns("exports", "db-20261010-000000.sql") {
		t.Fatal("a new export is not its maker's alone")
	}
}
