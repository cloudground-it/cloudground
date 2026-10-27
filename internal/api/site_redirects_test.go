package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// #83 (SPEC §3.18): the rules are checked and normalised, rendered by the
// agent before they are stored, the canonical host is one of the site's
// names and cannot be removed as an alias, the cache is emptied, and an
// agent refusal stores nothing.
func TestSiteRedirects(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := seedActiveSite(t, s, "moved.example.com", state.SiteWordPress, "8.4", 0)
	site, err := s.Store.MutateSite(site.ID, func(st *state.Site) error {
		st.Aliases = []string{"www.moved.example.com"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Store.MarkApplied(site.ID, site.DesiredGeneration, nil); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/sites/%d/redirects", site.ID)

	resp, body := admin.do("GET", path, nil)
	var got redirectsView
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &got) != nil {
		t.Fatalf("get = %d %s", resp.StatusCode, body)
	}
	if got.Canonical != "" || len(got.Rules) != 0 || got.Rules == nil || fmt.Sprint(got.Names) != "[moved.example.com www.moved.example.com]" ||
		got.Counterpart != "www.moved.example.com" || !got.CounterpartServed {
		t.Fatalf("the starting view is %+v", got)
	}

	rule := func(match, from, to string, status int) map[string]any {
		return map[string]any{"match": match, "from": from, "to": to, "status": status}
	}
	for name, bad := range map[string]map[string]any{
		"canonical not a name": {"canonical": "other.example.com"},
		"injection":            {"rules": []any{rule("exact", "/a; return 200;", "/b", 301)}},
		"target variable":      {"rules": []any{rule("exact", "/a", "/$host", 301)}},
		"status":               {"rules": []any{rule("exact", "/a", "/b", 308)}},
		"loop":                 {"rules": []any{rule("prefix", "/a", "/a/b", 301)}},
		"duplicate":            {"rules": []any{rule("exact", "/a", "/b", 301), rule("exact", "/a", "/c", 301)}},
	} {
		if resp, b := admin.do("PUT", path, bad); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = %d %s", name, resp.StatusCode, b)
		}
	}
	if agent.called(agentrpc.ApplySiteConfig.Name()) {
		t.Fatal("a refused change reached the agent")
	}

	resp, body = admin.do("PUT", path, map[string]any{
		"canonical": " WWW.moved.example.com ",
		"rules":     []any{rule("prefix", "/blog/", "/news/", 301), rule("exact", "/old", "https://example.org/new?x=1", 302)},
	})
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &got) != nil {
		t.Fatalf("put = %d %s", resp.StatusCode, body)
	}
	if got.Canonical != "www.moved.example.com" || len(got.Rules) != 2 || got.Rules[0].From != "/blog" || got.Rules[0].To != "/news" {
		t.Fatalf("answer %+v", got)
	}
	st, _ := s.Store.GetSite(site.ID)
	if st.AppliedGeneration != st.DesiredGeneration || st.Config.Redirects.Canonical != "www.moved.example.com" {
		t.Fatalf("stored %+v, generations %d/%d", st.Config.Redirects, st.AppliedGeneration, st.DesiredGeneration)
	}
	agent.mu.Lock()
	ref, _ := agent.lastParams[agentrpc.ApplySiteConfig.Name()].(rpc.SiteRef)
	agent.mu.Unlock()
	if ref.Site.Config.Redirects.Canonical != "www.moved.example.com" || len(ref.Site.Config.Redirects.Rules) != 2 {
		t.Fatalf("the agent rendered %+v", ref.Site.Config.Redirects)
	}
	if !agent.called(agentrpc.PurgeCache.Name()) {
		t.Fatal("the cache was not emptied")
	}

	// A field left out keeps its value.
	if resp, b := admin.do("PUT", path, map[string]any{"canonical": ""}); resp.StatusCode != http.StatusOK {
		t.Fatalf("canonical off = %d %s", resp.StatusCode, b)
	}
	if st, _ := s.Store.GetSite(site.ID); st.Config.Redirects.Canonical != "" || len(st.Config.Redirects.Rules) != 2 {
		t.Fatalf("after canonical off %+v", st.Config.Redirects)
	}

	// The canonical alias cannot be removed from the site.
	if resp, b := admin.do("PUT", path, map[string]any{"canonical": "www.moved.example.com"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("canonical on = %d %s", resp.StatusCode, b)
	}
	resp, body = admin.do("PATCH", fmt.Sprintf("/api/sites/%d", site.ID), map[string]any{"aliases": []string{}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("removing the canonical alias = %d %s", resp.StatusCode, body)
	}

	// A refused render stores nothing.
	agent.mu.Lock()
	agent.errs[agentrpc.ApplySiteConfig.Name()] = agentrpc.Wrap(agentrpc.CodeUnavailable, "nginx -t failed", nil)
	agent.mu.Unlock()
	if resp, _ := admin.do("PUT", path, map[string]any{"rules": []any{}}); resp.StatusCode < 400 {
		t.Fatalf("a refused render answered %d", resp.StatusCode)
	}
	agent.mu.Lock()
	delete(agent.errs, agentrpc.ApplySiteConfig.Name())
	agent.mu.Unlock()
	if st, _ := s.Store.GetSite(site.ID); len(st.Config.Redirects.Rules) != 2 {
		t.Fatal("a refused render was stored")
	}
	waitForTasks(t, s)
}
