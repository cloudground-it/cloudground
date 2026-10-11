package api

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/state"
)

// R-10.5: a site's own bypass cookies are a PATCH field: trimmed,
// deduplicated and sorted, stored and rendered; a name nginx could read
// as syntax is refused and nothing changes; [] clears them; a static
// site has no page cache to bypass.
func TestPatchBypassCookies(t *testing.T) {
	s, _, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	site := createWPSite(t, s, c, "cookies.example.com")

	code, a := patchSite(t, c, site.ID, map[string]any{"bypass_cookies": []string{" myapp_sid ", "a.b-c", "myapp_sid"}})
	if code != http.StatusAccepted {
		t.Fatalf("PATCH = %d %+v", code, a)
	}
	waitForTasks(t, s)
	got, _ := s.Store.GetSite(site.ID)
	if !slices.Equal(got.Config.BypassCookies, []string{"a.b-c", "myapp_sid"}) {
		t.Fatalf("stored %q", got.Config.BypassCookies)
	}
	for _, bad := range []string{`x"y`, "a b", "a;b", "${x}", strings.Repeat("a", 65), ""} {
		code, a = patchSite(t, c, site.ID, map[string]any{"bypass_cookies": []string{bad}})
		if code != http.StatusBadRequest || a.Fields["bypass_cookies"] == "" {
			t.Errorf("%q: %d %+v", bad, code, a)
		}
	}
	if got2, _ := s.Store.GetSite(site.ID); !slices.Equal(got2.Config.BypassCookies, got.Config.BypassCookies) {
		t.Fatalf("a refused patch changed the cookies: %q", got2.Config.BypassCookies)
	}
	many := make([]string, 21)
	for i := range many {
		many[i] = "c" + strings.Repeat("x", i+1)
	}
	if code, _ = patchSite(t, c, site.ID, map[string]any{"bypass_cookies": many}); code != http.StatusBadRequest {
		t.Errorf("21 cookies: %d", code)
	}
	if code, _ = patchSite(t, c, site.ID, map[string]any{"bypass_cookies": []string{}}); code != http.StatusAccepted {
		t.Fatalf("clear: %d", code)
	}
	waitForTasks(t, s)
	if got, _ = s.Store.GetSite(site.ID); len(got.Config.BypassCookies) != 0 {
		t.Fatalf("not cleared: %q", got.Config.BypassCookies)
	}
	static := seedActiveSite(t, s, "static.example.com", state.SiteStatic, "", 0)
	if code, a = patchSite(t, c, static.ID, map[string]any{"bypass_cookies": []string{"sid"}}); code != http.StatusBadRequest {
		t.Errorf("static site: %d %+v", code, a)
	}
}
