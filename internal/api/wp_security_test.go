package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// #74: two-factor sign-in is set per WordPress site; the agent renders the
// change before it is stored, a refusal stores nothing, and the same mode
// again renders nothing.
func TestWPTwoFactor(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := seedActiveSite(t, s, "shop.example.com", state.SiteWooCommerce, "8.4", 0)
	if err := s.Store.MarkApplied(site.ID, site.DesiredGeneration, nil); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/sites/%d/wp/two-factor", site.ID)
	sent := func() (string, bool) {
		agent.mu.Lock()
		defer agent.mu.Unlock()
		ref, ok := agent.lastParams[agentrpc.ApplySiteConfig.Name()].(rpc.SiteRef)
		return ref.Site.Config.WPTwoFactor, ok
	}

	resp, body := admin.do("PUT", path, map[string]any{"mode": "required"})
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"mode":"required"`) {
		t.Fatalf("required = %d %s", resp.StatusCode, body)
	}
	if m, ok := sent(); !ok || m != state.TwoFactorRequired {
		t.Fatalf("the agent rendered %q (%v)", m, ok)
	}
	st, _ := s.Store.GetSite(site.ID)
	if st.Config.WPTwoFactor != state.TwoFactorRequired || st.AppliedGeneration != st.DesiredGeneration {
		t.Fatalf("stored %q, generations %d/%d", st.Config.WPTwoFactor, st.AppliedGeneration, st.DesiredGeneration)
	}
	if _, b := admin.do("GET", fmt.Sprintf("/api/sites/%d", site.ID), nil); !strings.Contains(string(b), `"wp_two_factor":"required"`) {
		t.Fatalf("GET site does not show the mode: %s", b)
	}

	agent.forget()
	if resp, _ := admin.do("PUT", path, map[string]any{"mode": "required"}); resp.StatusCode != http.StatusOK || agent.called(agentrpc.ApplySiteConfig.Name()) {
		t.Fatalf("the same mode again = %d, rendered %v", resp.StatusCode, agent.called(agentrpc.ApplySiteConfig.Name()))
	}

	for _, bad := range []string{"", "on", "REQUIRED", "required\n"} {
		if resp, body := admin.do("PUT", path, map[string]any{"mode": bad}); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("mode %q = %d %s", bad, resp.StatusCode, body)
		}
	}

	agent.mu.Lock()
	agent.errs[agentrpc.ApplySiteConfig.Name()] = agentrpc.Invalid("PHP refused it")
	agent.mu.Unlock()
	if resp, _ := admin.do("PUT", path, map[string]any{"mode": "off"}); resp.StatusCode < 400 {
		t.Fatalf("a refused render answered %d", resp.StatusCode)
	}
	agent.mu.Lock()
	delete(agent.errs, agentrpc.ApplySiteConfig.Name())
	agent.mu.Unlock()
	if st, _ := s.Store.GetSite(site.ID); st.Config.WPTwoFactor != state.TwoFactorRequired {
		t.Fatalf("a refused render was stored: %q", st.Config.WPTwoFactor)
	}

	if resp, body := admin.do("PUT", path, map[string]any{"mode": "off"}); resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"mode":"off"`) {
		t.Fatalf("off = %d %s", resp.StatusCode, body)
	}
	if st, _ := s.Store.GetSite(site.ID); st.Config.WPTwoFactor != "" {
		t.Fatalf("off stored %q", st.Config.WPTwoFactor)
	}

	static := seedActiveSite(t, s, "static.example.com", state.SiteStatic, "", 0)
	if resp, body := admin.do("PUT", fmt.Sprintf("/api/sites/%d/wp/two-factor", static.ID), map[string]any{"mode": "optional"}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a static site = %d %s", resp.StatusCode, body)
	}
}
