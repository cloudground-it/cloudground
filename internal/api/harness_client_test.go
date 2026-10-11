package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/state"
)

// client is a browser for the tests: it keeps the cookies the panel last
// set and sends them with every request.
type client struct {
	t       *testing.T
	base    string
	cookies []*http.Cookie
}

// do sends one request, the body (when not nil) as JSON, and returns the
// response with its whole body read. Any transport failure ends the test.
func (c *client) do(method, path string, body any) (*http.Response, []byte) {
	c.t.Helper()
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		payload = b
	}
	req, err := http.NewRequest(method, c.base+path, bytes.NewReader(payload))
	if err != nil {
		c.t.Fatal(err)
	}
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, ck := range c.cookies {
		req.AddCookie(ck)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	if set := resp.Cookies(); len(set) > 0 {
		c.cookies = set
	}
	return resp, data
}

// The first account every test signs in with.
const (
	adminEmail    = "admin@example.com"
	adminPassword = "a-long-password!"
)

// setupAdmin creates the first admin through /api/setup with a fresh setup
// token and returns a client signed in as them.
func setupAdmin(t *testing.T, s *Server, ts *httptest.Server) *client {
	t.Helper()
	if err := s.Store.CreateSetupToken("boot-token", time.Hour); err != nil {
		t.Fatal(err)
	}
	c := &client{t: t, base: ts.URL}
	resp, body := c.do("POST", "/api/setup", map[string]string{
		"email": adminEmail, "password": adminPassword, "token": "boot-token",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("setup: %d %s", resp.StatusCode, body)
	}
	return c
}

// signIn is a new client signed in with email and password.
func signIn(t *testing.T, ts *httptest.Server, email, password string) *client {
	t.Helper()
	c := &client{t: t, base: ts.URL}
	if resp, body := c.do("POST", "/api/login", map[string]string{"email": email, "password": password}); resp.StatusCode != http.StatusOK {
		t.Fatalf("sign in as %s: %d %s", email, resp.StatusCode, body)
	}
	return c
}

// createViaAPI creates a site through the API with the fields given (domain
// and type at least), waits for its job, and returns its record.
func createViaAPI(t *testing.T, s *Server, c *client, req map[string]any) state.Site {
	t.Helper()
	resp, body := c.do("POST", "/api/sites", req)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create %v: %d %s", req["domain"], resp.StatusCode, body)
	}
	waitForTasks(t, s)
	site, err := s.Store.GetSiteByDomain(fmt.Sprint(req["domain"]))
	if err != nil {
		t.Fatalf("site %v after its create: %v", req["domain"], err)
	}
	return site
}

// wordpressSite is the body of a WordPress create on domain.
func wordpressSite(domain string) map[string]any {
	return map[string]any{
		"domain": domain, "type": "wordpress", "title": "Test site",
		"admin_email": "owner@" + domain, "admin_user": "owner", "admin_password": "wordpress-admin-pass",
	}
}

// staticSite is the body of a static create on domain.
func staticSite(domain string) map[string]any {
	return map[string]any{"domain": domain, "type": "static"}
}
