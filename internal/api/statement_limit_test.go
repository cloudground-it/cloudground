package api

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
)

func TestStatementLimiter(t *testing.T) {
	var l statementLimiter
	var held []func()
	take := func(user, site int64) bool {
		r, ok := l.acquire(fmt.Sprint(user), site)
		if ok {
			held = append(held, r)
		}
		return ok
	}
	if !take(1, 10) || !take(1, 11) {
		t.Fatal("an account's first two statements are refused")
	}
	if take(1, 12) {
		t.Fatal("a third statement of one account passes")
	}
	if !take(2, 10) || !take(3, 10) || !take(4, 10) {
		t.Fatal("other accounts on the site are refused")
	}
	if take(5, 10) {
		t.Fatal("a fifth statement on one site passes")
	}
	held[0]()
	held[0]() // twice is once
	if !take(5, 10) {
		t.Fatal("a released statement is still counted")
	}
	if take(6, 10) {
		t.Fatal("a double release freed two")
	}
	for _, r := range held {
		r()
	}
	if len(l.byUser) != 0 || len(l.bySite) != 0 {
		t.Fatalf("counts left: %v %v", l.byUser, l.bySite)
	}
}

// SR-T5: the console and Quarry take at most two statements at once per
// account and four per site; beyond, 429 before the agent is called.
func TestStatementsAreLimited(t *testing.T) {
	s, agent, admin, _, site := dbStudioSetup(t)
	release := make(chan struct{})
	started := make(chan struct{}, 16)
	agent.setHook(func(method string) {
		if method == agentrpc.DBQuery.Name() || method == agentrpc.DBStudioQuery.Name() || method == agentrpc.DBStudioRows.Name() {
			started <- struct{}{}
			<-release
		}
	})
	studio := "/api/quarry/db/" + site.Config.Database.Name + "/query"
	console := fmt.Sprintf("/api/sites/%d/database/query", site.ID)
	// The panel's console and Quarry opened from the panel count
	// against the same account.
	adminStudio := openStudio(t, admin, site.ID)
	var wg sync.WaitGroup
	run := func(c *client, path string) {
		wg.Add(1)
		cc := *c // a copy: the requests run at once
		go func() {
			defer wg.Done()
			sdo(&cc, "POST", path, map[string]any{"sql": "SELECT SLEEP(60)"})
		}()
		select {
		case <-started:
		case <-time.After(10 * time.Second):
			t.Fatal("the statement did not reach the agent")
		}
	}
	run(adminStudio, studio)
	run(admin, console)
	if resp, body := sdo(adminStudio, "POST", studio, map[string]any{"sql": "SELECT 1"}); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("a third statement of one account: %d %s", resp.StatusCode, body)
	}
	if resp, _ := sdo(adminStudio, "POST", "/api/quarry/db/"+site.Config.Database.Name+"/tables/wp_posts/rows", map[string]any{}); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("rows beyond the account's limit: %d", resp.StatusCode)
	}
	other := []*client{
		openStudio(t, signedInAdmin(t, s, admin, "second@example.com"), site.ID),
		openStudio(t, signedInAdmin(t, s, admin, "third@example.com"), site.ID),
	}
	run(other[0], studio)
	run(other[0], studio)
	if resp, body := sdo(other[1], "POST", studio, map[string]any{"sql": "SELECT 1"}); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("a fifth statement on the site: %d %s", resp.StatusCode, body)
	}
	close(release)
	wg.Wait()
	agent.setHook(nil)
	if resp, body := sdo(other[1], "POST", studio, map[string]any{"sql": "SELECT 1"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("after the statements ended: %d %s", resp.StatusCode, body)
	}
}

// signedInAdmin is another administrator's client, sharing c's server.
func signedInAdmin(t *testing.T, s *Server, c *client, email string) *client {
	t.Helper()
	hash, _ := hashPassword("unused-password")
	u, err := s.Store.CreateUser(email, hash, "admin")
	if err != nil {
		t.Fatal(err)
	}
	tok := randomToken(32)
	if err := s.Store.CreateSession(tok, u.ID, time.Hour); err != nil {
		t.Fatal(err)
	}
	return &client{t: t, base: c.base, cookies: []*http.Cookie{{Name: insecureSessionName, Value: tok}}}
}
