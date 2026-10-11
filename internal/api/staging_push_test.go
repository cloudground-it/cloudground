package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// The database push refuses in the order SPEC.md §3.7 gives, each refusal
// before any snapshot; a push that passes is audited and queued under both
// sites' locks and the repository's.
func TestStagingPushChecksInOrder(t *testing.T) {
	s, agent, _ := testServer(t)
	svc := s.stagingSvc()
	ctx := context.Background()
	prod := seedActiveSite(t, s, "push.example.com", state.SiteWordPress, "8.4", 0)
	push := func(tables []string, confirm string) (jobs.Task, error) {
		return svc.PushDatabase(ctx, "tester", prod, stagingPush{Tables: tables, Confirm: confirm})
	}
	expect := func(what string, err error, kind problemKind, msg string) {
		t.Helper()
		p := problemOf(t, err)
		if p.kind != kind || p.msg != msg {
			t.Errorf("%s: %v %q, want %v %q", what, p.kind, p.msg, kind, msg)
		}
	}

	// No copy comes first, whatever the body says.
	_, err := push(nil, "")
	expect("no staging", err, probPrecondition, "create a staging site first")

	stg := seedActiveSite(t, s, "staging.push.example.com", state.SiteWordPress, "8.4", 0)
	if _, err := s.Store.MutateSite(stg.ID, func(st *state.Site) error { st.StagingOf = prod.ID; return nil }); err != nil {
		t.Fatal(err)
	}
	agent.mu.Lock()
	agent.results[agentrpc.StagingTables.Name()] = rpc.StagingTablesResult{Tables: []rpc.StagingTable{
		{Name: "wp_posts"}, {Name: "wp_options"}, {Name: "wp_wc_orders", Protected: true, Reason: "live store data"},
	}}
	agent.mu.Unlock()

	_, err = push([]string{"wp_posts"}, "staging.push.example.com")
	expect("wrong confirmation", err, probInvalid, "type the production domain to confirm database promotion")
	_, err = push(nil, prod.Domain)
	expect("no tables", err, probInvalid, "select between 1 and 50 tables")
	many := make([]string, 51)
	for i := range many {
		many[i] = fmt.Sprintf("t%d", i)
	}
	_, err = push(many, prod.Domain)
	expect("51 tables", err, probInvalid, "select between 1 and 50 tables")
	_, err = push([]string{"wp_posts", "wp_nope", "wp_wc_orders"}, prod.Domain)
	expect("a missing table first", err, probInvalid, "staging has no table wp_nope")
	_, err = push([]string{"wp_wc_orders", "wp_nope"}, prod.Domain)
	expect("a protected table first", err, probInvalid, "selection contains protected live data table wp_wc_orders (live store data)")
	_, err = push([]string{"wp_posts"}, prod.Domain)
	expect("no backups configured", err, probPrecondition, "configure backup_repository and backup_password in settings first")
	if agent.called(agentrpc.RunBackup.Name()) {
		t.Fatal("a refused push took a snapshot")
	}

	// Accepted: audited, and queued under the three locks.
	setBackupRepo(t, s, "/var/backups/cloudground/push-repo", "push-password")
	release := holdLocks(t, s, jobs.RepoLock("/var/backups/cloudground/push-repo"))
	defer release()
	task, err := push([]string{"wp_posts", "wp_options"}, prod.Domain)
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.Store.GetJob(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, lock := range []string{jobs.SiteLock(prod.ID), jobs.SiteLock(stg.ID), jobs.RepoLock("/var/backups/cloudground/push-repo")} {
		if !slices.Contains(job.Locks, lock) {
			t.Errorf("the push job's locks %v miss %s", job.Locks, lock)
		}
	}
	if job.Kind != "staging.database-push" || job.SiteID != prod.ID {
		t.Errorf("job = %s on site %d", job.Kind, job.SiteID)
	}
	events, _ := s.Store.ListAuditEvents(10)
	if len(events) == 0 || events[0].Action != "staging.database-push" || events[0].Subject != prod.Domain || events[0].Detail != "wp_posts,wp_options" {
		t.Errorf("audit = %+v", events)
	}
}

// Through HTTP: a body that does not parse is an unconfirmed push, answered
// after the staging check; the tables route answers the agent's list with
// the copy's domain.
func TestStagingPushRoutes(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	prod := seedActiveSite(t, s, "routes.example.com", state.SiteWordPress, "8.4", 0)
	raw := func(body string) (int, string) {
		req, _ := http.NewRequest("POST", fmt.Sprintf("%s/api/sites/%d/staging/database", ts.URL, prod.ID), strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		for _, ck := range c.cookies {
			req.AddCookie(ck)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := raw(`{"tables":`); code != http.StatusPreconditionFailed {
		t.Fatalf("malformed push without staging = %d %s, want 412", code, body)
	}
	if resp, body := c.do("GET", fmt.Sprintf("/api/sites/%d/staging/tables", prod.ID), nil); resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("tables without staging = %d %s, want 412", resp.StatusCode, body)
	}

	stg := seedActiveSite(t, s, "staging.routes.example.com", state.SiteWordPress, "8.4", 0)
	s.Store.MutateSite(stg.ID, func(st *state.Site) error { st.StagingOf = prod.ID; return nil })
	if code, body := raw(`{"tables": ["wp_posts"], "confirm": "routes.example.com"`); code != http.StatusBadRequest || !strings.Contains(body, "type the production domain") {
		t.Fatalf("malformed push = %d %s, want 400 asking for the confirmation", code, body)
	}
	agent.mu.Lock()
	agent.results[agentrpc.StagingTables.Name()] = rpc.StagingTablesResult{}
	agent.mu.Unlock()
	resp, body := c.do("GET", fmt.Sprintf("/api/sites/%d/staging/tables", prod.ID), nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"tables":[]`) || !strings.Contains(string(body), `"staging":"staging.routes.example.com"`) {
		t.Fatalf("tables = %d %s", resp.StatusCode, body)
	}
}
