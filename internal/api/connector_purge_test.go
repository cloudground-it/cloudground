package api

import (
	"bytes"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// connectorPost sends one purge with the bearer token and returns the
// status.
func connectorPost(t *testing.T, url, token string) int {
	t.Helper()
	req, _ := http.NewRequest("POST", url+"/api/connector/purge", bytes.NewReader([]byte(`{"all":true}`)))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// Wrong connector tokens are limited per address; a right one never is.
// Every site purges from the loopback address, so a limit on the address
// let one site's PHP stop every other site's purges (R-4.3).
func TestConnectorLimitsWrongTokensOnly(t *testing.T) {
	s, agent, ts := testServer(t)
	site := activeSite(t, s, "purge.example.com")
	token := randomToken(24)
	if err := s.Store.SetSecret(state.SiteSecret(site.ID, state.SiteSecretConnectorToken), token); err != nil {
		t.Fatal(err)
	}
	if code := connectorPost(t, ts.URL, ""); code != http.StatusUnauthorized {
		t.Fatalf("no token = %d", code)
	}
	for i := 1; i < connectorFailures; i++ {
		if code := connectorPost(t, ts.URL, fmt.Sprintf("wrong-%d", i)); code != http.StatusUnauthorized {
			t.Fatalf("wrong token %d = %d, want 401", i, code)
		}
	}
	if code := connectorPost(t, ts.URL, "one-too-many"); code != http.StatusTooManyRequests {
		t.Fatalf("a wrong token past the limit = %d, want 429", code)
	}
	if code := connectorPost(t, ts.URL, token); code != http.StatusOK {
		t.Fatalf("the site's own token from a limited address = %d, want 200", code)
	}
	if !agent.called(agentrpc.PurgeCache.Name()) {
		t.Fatal("the valid purge did not reach the agent")
	}
	// The token of a staging copy, or any other secret's value, is not a
	// connector token of the site.
	if err := s.Store.SetSecret(state.SiteSecret(site.ID, state.SiteSecretDBPassword), "db-secret"); err != nil {
		t.Fatal(err)
	}
	s.connectorFailLimiter.Reset("127.0.0.1")
	if code := connectorPost(t, ts.URL, "db-secret"); code != http.StatusUnauthorized {
		t.Fatalf("a database password as a token = %d, want 401", code)
	}
}

// A site calling in a loop is coalesced: one purge at a time, the rest
// waiting for the next, which becomes a purge of the names past the cap.
func TestCDNPurgesCoalescePerSite(t *testing.T) {
	q := &cdnPurges{gap: 50 * time.Millisecond}
	type call struct {
		site int64
		urls int
		all  bool
	}
	var mu sync.Mutex
	var calls []call
	release := make(chan struct{})
	run := func(site state.Site, urls []string, all bool) {
		mu.Lock()
		calls = append(calls, call{site.ID, len(urls), all})
		first := len(calls) == 1
		mu.Unlock()
		if first {
			<-release
		}
	}
	a, b := state.Site{ID: 1}, state.Site{ID: 2}
	urls := func(n, from int) []string {
		var out []string
		for i := range n {
			out = append(out, fmt.Sprintf("https://a.example.com/%d", from+i))
		}
		return out
	}
	waitFor := func(n int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			mu.Lock()
			got := len(calls)
			mu.Unlock()
			if got >= n {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%d calls, want %d", got, n)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	q.add(a, urls(10, 0), false, run)
	waitFor(1) // the first run has begun: what follows waits for the next
	for i := range 40 {
		q.add(a, urls(10, 100+10*i), false, run)
	}
	q.add(b, urls(3, 0), false, run)
	waitFor(2) // site 1's first, and site 2's, which does not wait for site 1
	close(release)
	waitFor(3)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		q.mu.Lock()
		left := len(q.pending)
		q.mu.Unlock()
		if left == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d sites still pending", left)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 3 {
		t.Fatalf("calls %+v, want 3", calls)
	}
	var second call
	for _, c := range calls[1:] {
		if c.site == 1 {
			second = c
		}
	}
	if calls[0] != (call{1, 10, false}) || second != (call{1, 0, true}) {
		t.Fatalf("calls %+v: want 10 URLs, then the names (400 waiting > 300)", calls)
	}
}
