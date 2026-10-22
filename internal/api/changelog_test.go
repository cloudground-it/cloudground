package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cloudground-it/cloudground/internal/changelog"
	"github.com/cloudground-it/cloudground/internal/version"
)

// #106 (SPEC §2.9): the embedded changelog is served with what the account
// has not seen; marking it seen records the running release, nothing a
// client names, and is per account.
func TestChangelogSeenPerAccount(t *testing.T) {
	s, _, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	entries := notes()
	if len(entries) == 0 {
		t.Fatal("the embedded changelog has no release")
	}
	saved := version.Version
	version.Version = entries[0].Version
	defer func() { version.Version = saved }()

	type answer struct {
		Current string            `json:"current"`
		Seen    string            `json:"seen"`
		Unseen  []string          `json:"unseen"`
		Entries []changelog.Entry `json:"entries"`
	}
	read := func() answer {
		t.Helper()
		resp, body := admin.do("GET", "/api/changelog", nil)
		var a answer
		if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &a) != nil {
			t.Fatalf("GET /api/changelog = %d %s", resp.StatusCode, body)
		}
		return a
	}
	if err := s.Store.SetUserWhatsNewSeen(1, ""); err != nil { // setup marked it with the test build's "dev"
		t.Fatal(err)
	}
	a := read()
	if a.Current != entries[0].Version || a.Seen != "" || len(a.Unseen) != 1 || a.Unseen[0] != entries[0].Version || len(a.Entries) != len(entries) {
		t.Fatalf("before: %+v", a)
	}
	// A body that names a release changes nothing: the running one is recorded.
	resp, body := admin.do("POST", "/api/account/whats-new", map[string]string{"seen": "v99.0.0"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("seen = %d %s", resp.StatusCode, body)
	}
	a = read()
	if a.Seen != entries[0].Version || len(a.Unseen) != 0 {
		t.Fatalf("after: %+v", a)
	}

	// A new release comes: the account is told of it, and of the ones it skipped.
	if len(entries) > 2 {
		if err := s.Store.SetUserWhatsNewSeen(1, entries[2].Version); err != nil {
			t.Fatal(err)
		}
		a = read()
		if len(a.Unseen) != 2 || a.Unseen[0] != entries[0].Version || a.Unseen[1] != entries[1].Version {
			t.Fatalf("skipped releases: %+v", a.Unseen)
		}
	}

	// A build that is no release has nothing to tell.
	version.Version = "dev"
	if a = read(); len(a.Unseen) != 0 || a.Unseen == nil {
		t.Fatalf("dev build: %+v", a)
	}
}
