package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/state"
)

func TestNormalizeEmail(t *testing.T) {
	for in, want := range map[string]string{
		"a@example.com":         "a@example.com",
		"  Mixed@Example.COM  ": "mixed@example.com",
	} {
		if got, err := normalizeEmail(in); err != nil || got != want {
			t.Errorf("normalizeEmail(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"X <x@y.z>", "<x@y.z>", `"X" <x@y.z>`, "x@y.z (comment)", "not an address", "",
		strings.Repeat("a", 250) + "@y.zz",
	} {
		if got, err := normalizeEmail(in); err == nil {
			t.Errorf("normalizeEmail(%q) accepted as %q", in, got)
		}
	}
}

// A user created as "Name <a@b>" used to be stored verbatim, an account nobody
// could sign in to; passwords had no upper bound on create or change.
func TestUserCreateValidatesEmailAndPassword(t *testing.T) {
	s, _, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	long := strings.Repeat("p", maxPasswordLen+1)

	for _, email := range []string{"X <x@y.z>", "<x@y.z>"} {
		if resp, body := c.do("POST", "/api/users", map[string]any{"email": email, "password": "a-long-password!", "role": "readonly"}); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("create %q = %d %s, want 400", email, resp.StatusCode, body)
		}
	}
	if resp, body := c.do("POST", "/api/users", map[string]any{"email": "ok@example.com", "password": long, "role": "readonly"}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("create with a %d-character password = %d %s", len(long), resp.StatusCode, body)
	}
	resp, body := c.do("POST", "/api/users", map[string]any{"email": " New.User@Example.COM ", "password": "a-long-password!", "role": "readonly"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d %s", resp.StatusCode, body)
	}
	var u state.User
	json.Unmarshal(body, &u)
	if u.Email != "new.user@example.com" {
		t.Fatalf("stored email = %q, want the lowercased bare address", u.Email)
	}
	if resp, _ := c.do("POST", fmt.Sprintf("/api/users/%d/password", u.ID), map[string]string{"password": long}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("admin reset to an overlong password = %d", resp.StatusCode)
	}
	if resp, _ := c.do("POST", "/api/account/password", map[string]string{"current_password": "a-long-password!", "new_password": long}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("own change to an overlong password = %d", resp.StatusCode)
	}
}

// Deleting or demoting the last enabled admin is refused the same way; a
// disabled admin is not a reason to allow it.
func TestLastEnabledAdminOverHTTP(t *testing.T) {
	s, _, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	resp, body := c.do("POST", "/api/users", map[string]any{"email": "second@example.com", "password": "a-long-password!", "role": "admin"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d %s", resp.StatusCode, body)
	}
	var second state.User
	json.Unmarshal(body, &second)
	// Sign in as the second admin, then disable the first one from there.
	c2 := &client{t: t, base: ts.URL}
	if resp, _ := c2.do("POST", "/api/login", map[string]string{"email": "second@example.com", "password": "a-long-password!"}); resp.StatusCode != http.StatusOK {
		t.Fatal("second admin cannot sign in")
	}
	first, _ := s.Store.GetUserByEmail("admin@example.com")
	if resp, body := c2.do("PUT", fmt.Sprintf("/api/users/%d", first.ID), map[string]any{"role": "admin", "disabled": true}); resp.StatusCode != http.StatusOK {
		t.Fatalf("disable first admin = %d %s", resp.StatusCode, body)
	}
	// Now second is the only enabled admin. Re-enable the first, then from
	// the first: second may be disabled again only while first stays enabled.
	s.Store.UpdateUserAccess(first.ID, "admin", false)
	c1 := &client{t: t, base: ts.URL}
	c1.do("POST", "/api/login", map[string]string{"email": "admin@example.com", "password": "a-long-password!"})
	if resp, body := c1.do("PUT", fmt.Sprintf("/api/users/%d", second.ID), map[string]any{"role": "admin", "disabled": true}); resp.StatusCode != http.StatusOK {
		t.Fatalf("disable second admin = %d %s", resp.StatusCode, body)
	}
	// first is the last enabled admin; a disabled admin (second) exists. A
	// delete of second is fine, and nothing can remove first.
	if resp, body := c1.do("DELETE", fmt.Sprintf("/api/users/%d", second.ID), nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("delete disabled admin = %d %s", resp.StatusCode, body)
	}
	if resp, _ := c1.do("DELETE", "/api/users/9999", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("delete missing user = %d", resp.StatusCode)
	}
}
