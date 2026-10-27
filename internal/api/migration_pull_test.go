package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

// The dry run reaches the agent with the request; the pull copies, then
// imports behind the safety snapshot, cleans up whatever happens, and never
// stores the password.
func TestPullMigration(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := createViaAPI(t, s, admin, staticSite("pull.example.com"))
	setBackupRepo(t, s, "/var/backups/cloudground/repo", "test-repository-password")
	base := map[string]any{"host": "203.0.113.20", "port": 2222, "user": "old", "password": "s3cret-pass", "path": "/var/www/old"}

	for _, bad := range []map[string]any{
		{"host": "-oProxyCommand=x", "user": "old", "password": "p", "path": "/x"},
		{"host": "203.0.113.20", "user": "old", "path": "/x"},
		{"host": "203.0.113.20", "user": "old", "password": "p", "private_key": "k", "path": "/x"},
		{"host": "203.0.113.20", "user": "old", "password": "p", "path": "/x", "db": "file"},
		{"host": "203.0.113.20", "user": "old", "password": "p", "path": "/x", "db": "everything"},
	} {
		if resp, body := admin.do("POST", fmt.Sprintf("/api/sites/%d/migration/pull/estimate", site.ID), bad); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("estimate %v = %d %s", bad, resp.StatusCode, body)
		}
	}
	agent.setResult(agentrpc.PullEstimate.Name(), rpc.PullEstimateResult{Files: 12, Bytes: 3400, HostKey: "SHA256:abc", Fits: true})
	resp, body := admin.do("POST", fmt.Sprintf("/api/sites/%d/migration/pull/estimate", site.ID), base)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"files":12`) {
		t.Fatalf("estimate = %d %s", resp.StatusCode, body)
	}

	pull := map[string]any{}
	for k, v := range base {
		pull[k] = v
	}
	pull["confirm"] = "wrong.example.com"
	if resp, _ := admin.do("POST", fmt.Sprintf("/api/sites/%d/migration/pull", site.ID), pull); resp.StatusCode != http.StatusBadRequest {
		t.Fatal("a pull without the domain confirmed was accepted")
	}
	pull["confirm"] = site.Domain
	pull["host_key"] = "SHA256:abc"
	agent.setResult(agentrpc.PullMigration.Name(), rpc.PullResult{Archive: "tmp/cg-pull-x.tar.gz", Files: 12, Bytes: 3400, HostKey: "SHA256:abc"})
	agent.setResult(agentrpc.RunBackup.Name(), rpc.BackupResult{SnapshotID: "fedcba0987654321", SizeBytes: 42})
	agent.setResult(agentrpc.VerifyBackup.Name(), rpc.VerifyResult{Passed: true})
	agent.setResult(agentrpc.ImportMigration.Name(), rpc.MigrationResult{Files: 12})
	resp, body = admin.do("POST", fmt.Sprintf("/api/sites/%d/migration/pull", site.ID), pull)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("pull = %d %s", resp.StatusCode, body)
	}
	waitForTasks(t, s)

	agent.mu.Lock()
	var order []string
	for _, c := range agent.history {
		switch c.method {
		case agentrpc.PullMigration.Name(), agentrpc.RunBackup.Name(), agentrpc.ImportMigration.Name(), agentrpc.PullCleanup.Name():
			order = append(order, c.method)
		}
	}
	pp, _ := agent.lastParams[agentrpc.PullMigration.Name()].(rpc.PullParams)
	ip, _ := agent.lastParams[agentrpc.ImportMigration.Name()].(rpc.MigrationParams)
	agent.mu.Unlock()
	if strings.Join(order, ",") != "PullMigration,RunBackup,ImportMigration,PullCleanup" {
		t.Fatalf("order %v: copy, safety snapshot, import, cleanup", order)
	}
	if pp.Password != "s3cret-pass" || pp.HostKey != "SHA256:abc" || pp.ReleaseID == "" || ip.Archive != "tmp/cg-pull-x.tar.gz" || ip.ReleaseID != pp.ReleaseID {
		t.Fatalf("pull %+v import %+v", pp, ip)
	}
	if !hasBackup(t, s, site.ID, "pre-migration", "fedcba0987654321") {
		t.Fatal("no verified pre-migration snapshot")
	}
	// The password is nowhere the panel keeps things.
	jobs, err := s.Store.ListJobs(50, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if strings.Contains(j.Payload+j.LogHead+j.LogTail+j.Message+j.Error, "s3cret-pass") {
			t.Fatalf("the password is in job %d", j.ID)
		}
	}
	_, audit := admin.do("GET", "/api/audit", nil)
	if strings.Contains(string(audit), "s3cret-pass") || !strings.Contains(string(audit), "migration.pull") {
		t.Fatalf("audit: %.400s", audit)
	}

	// A failed copy still cleans up and imports nothing.
	agent.setErr(agentrpc.PullMigration.Name(), &agentrpc.Error{Code: agentrpc.CodeInvalid, Message: "the sign-in was refused"})
	before := len(agent.calls)
	if resp, _ := admin.do("POST", fmt.Sprintf("/api/sites/%d/migration/pull", site.ID), pull); resp.StatusCode != http.StatusAccepted {
		t.Fatal("second pull refused")
	}
	waitForTasks(t, s)
	agent.mu.Lock()
	after := append([]string{}, agent.calls[before:]...)
	agent.mu.Unlock()
	if strings.Contains(strings.Join(after, ","), agentrpc.ImportMigration.Name()) || !strings.Contains(strings.Join(after, ","), agentrpc.PullCleanup.Name()) {
		t.Fatalf("calls after a failed copy: %v", after)
	}

	// An operator cannot pull.
	if resp, _ := admin.do("POST", "/api/users", map[string]any{"email": "op@example.com", "password": adminPassword, "role": "operator", "site_ids": []int64{site.ID}}); resp.StatusCode != http.StatusCreated {
		t.Fatal("create operator")
	}
	op := signIn(t, ts, "op@example.com", adminPassword)
	if resp, _ := op.do("POST", fmt.Sprintf("/api/sites/%d/migration/pull/estimate", site.ID), base); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("operator estimate = %d", resp.StatusCode)
	}
}
