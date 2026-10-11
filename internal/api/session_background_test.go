package api

import (
	"database/sql"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/state"
)

// rawDB opens the panel's database file beside the store, for a test that
// must age a session.
func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func sessionLastSeen(t *testing.T, db *sql.DB) time.Time {
	t.Helper()
	var v string
	if err := db.QueryRow(`SELECT last_seen_at FROM sessions`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	at, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

func setSessionLastSeen(t *testing.T, db *sql.DB, at time.Time) {
	t.Helper()
	if _, err := db.Exec(`UPDATE sessions SET last_seen_at = ?`, at.UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
}

// A request the UI marks as background (a poll, a refetch after an event)
// is authenticated but does not count as a use, so a tab left open reaches
// the idle timeout; any other request counts (SPEC.md §2.2).
func TestBackgroundRequestsDoNotKeepASessionAlive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, _, ts := testServerAt(t, path)
	admin := setupAdmin(t, s, ts)
	db := rawDB(t, path)

	background := func(p string) int {
		t.Helper()
		req, _ := http.NewRequest("GET", ts.URL+p, nil)
		req.Header.Set(backgroundHeader, "1")
		for _, ck := range admin.cookies {
			req.AddCookie(ck)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	idle := time.Now().Add(-90 * time.Minute)
	setSessionLastSeen(t, db, idle)
	for _, p := range []string{"/api/sites", "/api/system/alerts", "/api/session"} {
		if got := background(p); got != http.StatusOK {
			t.Fatalf("background GET %s = %d, want 200: it still authenticates", p, got)
		}
	}
	if got := sessionLastSeen(t, db); !got.Equal(idle) {
		t.Fatalf("background requests moved last_seen from %v to %v", idle, got)
	}

	// A request the person made counts.
	if resp, body := admin.do("GET", "/api/sites", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/sites = %d %s", resp.StatusCode, body)
	}
	if got := sessionLastSeen(t, db); time.Since(got) > time.Minute {
		t.Fatalf("a foreground request left last_seen at %v", got)
	}

	// Past the idle timeout, background polling alone does not save it.
	setSessionLastSeen(t, db, time.Now().Add(-state.SessionIdleTimeout-time.Minute))
	if got := background("/api/sites"); got != http.StatusUnauthorized {
		t.Fatalf("background GET on an idle session = %d, want 401", got)
	}
	if resp, _ := admin.do("GET", "/api/sites", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the idle session still works: %d", resp.StatusCode)
	}
}
