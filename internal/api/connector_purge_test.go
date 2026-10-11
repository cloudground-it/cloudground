package api

import (
	"bytes"
	"fmt"
	"net/http"
	"testing"

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
