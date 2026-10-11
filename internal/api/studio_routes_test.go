package api

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// The Quarry policies (SPEC.md §1.3, §6.4) through the real router, the
// other way round from TestRouteTableMatrix: every Quarry row for every
// kind of Quarry session, and every panel row for a Quarry session
// alone. They state who is admitted independently of router.go.
func TestStudioRouteMatrix(t *testing.T) {
	const (
		asNobody     = "no session"
		asPanelOnly  = "panel session only"
		asDBUser     = "database user"
		asStudioAdm  = "opened by an admin"
		asStudioRead = "opened by a read-only account"
	)
	studioAdmits := map[policy]map[string]bool{
		policyStudio:     {asDBUser: true, asStudioAdm: true, asStudioRead: true},
		policyStudioData: {asDBUser: true, asStudioAdm: true},
	}
	s, agent, admin, ro, site := dbStudioSetup(t)
	if _, err := s.Store.AddDBUser(site.ID, "reader", state.DBUserSecret(site.ID, "reader"), "reader-password-1",
		[]state.DBGrant{{Database: site.Config.Database.Name, Level: "read"}}); err != nil {
		t.Fatal(err)
	}
	agent.setResult(agentrpc.DBStudioOpen.Name(), rpc.DBStudioOpenResult{Login: strings.Repeat("ef", 32)})
	clients := map[string]func() *client{
		asNobody:    func() *client { return &client{t: t, base: admin.base} },
		asPanelOnly: func() *client { return &client{t: t, base: admin.base, cookies: admin.cookies} },
		asDBUser: func() *client {
			c := &client{t: t, base: admin.base}
			resp, _ := signInStudio(c, "reader", "reader-password-1")
			return &client{t: t, base: admin.base, cookies: []*http.Cookie{cookieOf(resp, insecureStudioCookie)}}
		},
		asStudioAdm:  func() *client { return openStudio(t, admin, site.ID) },
		asStudioRead: func() *client { return openStudio(t, ro, site.ID) },
	}
	routes := (&Server{}).routeTable()
	// Deletes, then logout, last: they end what the others use.
	rank := func(rt route) int {
		switch {
		case strings.HasSuffix(rt.Path, "/logout"):
			return 2
		case rt.Method == http.MethodDelete:
			return 1
		}
		return 0
	}
	sort.SliceStable(routes, func(i, j int) bool { return rank(routes[i]) < rank(routes[j]) })
	path := func(rt route) string {
		return strings.NewReplacer("{db}", site.Config.Database.Name, "{table}", "wp_posts", "{dir}", "imports",
			"{name}", "x.sql", "{id}", "1").Replace(rt.Path)
	}
	for who, mk := range clients {
		c := mk()
		for _, rt := range routes {
			if !rt.Policy.studio() {
				continue
			}
			resp, body := sdo(c, rt.Method, path(rt), map[string]any{})
			var want int
			var msg string
			switch {
			case who == asNobody || who == asPanelOnly:
				want, msg = http.StatusUnauthorized, msgStudioSignIn
			case !studioAdmits[rt.Policy][who]:
				want, msg = http.StatusForbidden, msgStudioReadOnly
			}
			refused := (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) &&
				(strings.Contains(string(body), msgStudioSignIn) || strings.Contains(string(body), msgStudioReadOnly) || strings.Contains(string(body), msgStudioPanel))
			switch {
			case want != 0 && (resp.StatusCode != want || !strings.Contains(string(body), msg)):
				t.Errorf("%s %s: %d %s, want %d %q", who, rt.pattern(), resp.StatusCode, body, want, msg)
			case want == 0 && refused:
				t.Errorf("%s %s: refused (%d %s)", who, rt.pattern(), resp.StatusCode, body)
			}
		}
	}
	// A Quarry session alone is no panel session: every panel row that
	// signs its caller in refuses it.
	db := clients[asDBUser]()
	for _, rt := range routes {
		if !rt.Policy.needsSession() || rt.Method == "" {
			continue
		}
		p := strings.NewReplacer("{id}", "1", "{name}", "x", "{fingerprint}", "x", "{table}", "x", "{dir}", "x").Replace(rt.Path)
		if resp, body := sdo(db, rt.Method, p, map[string]any{}); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("a Quarry session reached %s: %d %s", rt.pattern(), resp.StatusCode, body)
		}
	}
	waitForTasks(t, s)
}
