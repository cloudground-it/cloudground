package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/state"
	"github.com/cloudground-it/cloudground/internal/version"
)

// The update button sends nothing; a request naming a source, or asking to
// skip the attestation, is refused: where root's binaries come from is
// built into the agent.
func TestSelfUpdateTakesNothingFromTheRequest(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	if resp, body := admin.do("POST", "/api/panel/update", map[string]string{}); resp.StatusCode != http.StatusOK {
		t.Fatalf("update = %d %s", resp.StatusCode, body)
	}
	if !agent.called(agentrpc.SelfUpdate.Name()) {
		t.Fatal("the agent was not asked to update")
	}
	for _, field := range []string{"base_url", "allow_unattested"} {
		if resp, body := admin.do("POST", "/api/panel/update", map[string]any{field: "https://evil.example"}); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("update with %s = %d %s, want 400", field, resp.StatusCode, body)
		}
	}
}

// Versions compare by number, part by part; a dev build is never told to
// update; a source build's describe string reads as its tag.
func TestReleaseVersionsCompareNumerically(t *testing.T) {
	for _, c := range []struct {
		candidate, running string
		newer              bool
	}{
		{"v0.2.2", "v0.2.1", true},
		{"v0.2.1", "v0.2.1", false},
		{"v0.2.0", "v0.2.1", false},
		{"v0.2.10", "v0.2.9", true},
		{"v0.3.0", "v0.2.99", true},
		{"v1.0.0", "v0.9.9", true},
		{"v0.2.2", "dev", false},
		{"v0.2.2", "v0.1.2-14-g96d73a3", true},
	} {
		if got := version.Newer(c.candidate, c.running); got != c.newer {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.candidate, c.running, got, c.newer)
		}
	}
}

// "What's new" links to the CHANGELOG heading as GitHub renders its id
// ("## v0.2.1" is id="v021").
func TestChangelogAnchorIsGitHubs(t *testing.T) {
	for tag, id := range map[string]string{"v0.2.1": "v021", "v0.2.10": "v0210", "v1.0.0": "v100"} {
		if got := changelogAnchor(tag); got != id {
			t.Errorf("changelogAnchor(%q) = %q, want %q", tag, got, id)
		}
	}
}

// The release check is gentle with the feed: after a failure it backs off,
// a forced refresh goes out at most once per refreshGap, and while one
// check runs no other starts.
func TestReleaseCheckBacksOff(t *testing.T) {
	var requests atomic.Int32
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.NotFound(w, r)
	}))
	defer feed.Close()
	savedURL, savedKey := version.UpdateURL, version.UpdatePublicKey
	version.UpdateURL, version.UpdatePublicKey = feed.URL, "test-key"
	defer func() { version.UpdateURL, version.UpdatePublicKey = savedURL, savedKey }()

	s := &Server{}
	check := func(force bool) string {
		_, _, msg := s.updates.latest(force, fetchRelease)
		return msg
	}
	expect := func(when string, n int32) {
		t.Helper()
		if got := requests.Load(); got != n {
			t.Fatalf("%s: %d requests to the feed, want %d", when, got, n)
		}
	}
	if msg := check(false); msg == "" {
		t.Fatal("a failing feed reported no problem")
	}
	expect("the first check", 1)
	check(false)
	check(true)
	expect("within the backoff", 1)

	s.updates.mu.Lock()
	s.updates.at = time.Now().Add(-2 * refreshGap)
	s.updates.mu.Unlock()
	check(true)
	expect("a forced refresh after the gap", 2)

	s.updates.mu.Lock()
	s.updates.busy, s.updates.at = true, time.Time{}
	s.updates.mu.Unlock()
	check(true)
	expect("a check while one is running", 2)
}

// An update restarts both services: while a restore runs it is refused,
// naming the task, and the agent is not asked (R-6.5).
func TestSelfUpdateWaitsForARunningRestore(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	j, _, err := s.Store.CreateJob(state.Job{Kind: "backup.restore", Status: state.JobQueued})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.StartJob(j.ID); err != nil {
		t.Fatal(err)
	}
	resp, body := admin.do("POST", "/api/panel/update", map[string]string{})
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), "backup.restore") {
		t.Fatalf("update during a restore = %d %s, want 409 naming the task", resp.StatusCode, body)
	}
	if agent.called(agentrpc.SelfUpdate.Name()) {
		t.Fatal("the agent was asked to update during a restore")
	}
}
