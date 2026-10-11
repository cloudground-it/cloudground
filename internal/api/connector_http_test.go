package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// The listener sites reach (ConnectorRoutes) serves the connector's routes
// and the health check, and none of the panel's.
func TestConnectorListenerServesOnlyTheConnector(t *testing.T) {
	s, _, _ := testServer(t)
	ts := httptest.NewServer(s.ConnectorRoutes())
	defer ts.Close()
	get := func(path string) int {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, path := range []string{"/", "/api/bootstrap", "/api/login", "/api/sites"} {
		if code := get(path); code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s on the connector listener = %d, want it absent", path, code)
		}
	}
	if code := get("/healthz"); code != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want 200", code)
	}
}

// A purge needs the site's own connector token, and only URLs of that site
// reach the agent.
func TestConnectorPurgeNeedsTheSiteToken(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := createViaAPI(t, s, admin, staticSite("blog.example.com"))
	token, err := s.Store.Secret(state.SiteSecret(site.ID, state.SiteSecretConnectorToken))
	if err != nil || token == "" {
		t.Fatalf("the site has no connector token (%v)", err)
	}
	purge := func(bearer string, body any) int {
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", ts.URL+"/api/connector/purge", bytes.NewReader(raw))
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	everything := map[string]any{"all": true}
	for _, bearer := range []string{"", "not-the-token"} {
		if code := purge(bearer, everything); code != http.StatusUnauthorized {
			t.Errorf("purge with token %q = %d, want 401", bearer, code)
		}
	}
	if code := purge(token, map[string]any{"urls": []string{"https://blog.example.com/post/"}}); code != http.StatusOK {
		t.Fatalf("purge of the site's URL = %d", code)
	}
	if !agent.called(agentrpc.PurgeCache.Name()) {
		t.Fatal("the purge did not reach the agent")
	}

	agent.forget()
	if code := purge(token, map[string]any{"urls": []string{"https://victim.example.net/"}}); code != http.StatusOK {
		t.Fatalf("purge of another site's URL = %d, want 200 with nothing removed", code)
	}
	if agent.called(agentrpc.PurgeCache.Name()) {
		t.Fatal("another site's URL reached the agent")
	}
}
