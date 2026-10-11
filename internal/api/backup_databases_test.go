package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// #15: a backup record keeps the databases its snapshot holds (what the
// restore dialog offers), null for a record from before; a restore of some
// of them reaches the agent with those alone; one the snapshot does not
// hold is refused before anything runs.
func TestRestoreChoosesAmongTheSnapshotsDatabases(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	site := createViaAPI(t, s, c, staticSite("dbs.example.com"))
	base := fmt.Sprintf("/api/sites/%d", site.ID)
	for _, name := range []string{"dbs_data", "dbs_logs"} {
		if resp, body := c.do("POST", base+"/databases", map[string]string{"name": name}); resp.StatusCode != http.StatusAccepted {
			t.Fatalf("create %s = %d %s", name, resp.StatusCode, body)
		}
	}
	waitForTasks(t, s)
	setBackupRepo(t, s, "/var/backups/repo", "correct-horse")
	agent.setResult(agentrpc.RunBackup.Name(), rpc.BackupResult{SnapshotID: "0123456789abcdef", SizeBytes: 9, Databases: []string{"dbs_data", "dbs_logs"}})
	agent.setResult(agentrpc.VerifyBackup.Name(), rpc.VerifyResult{Passed: true, Detail: "ok"})
	if resp, body := c.do("POST", base+"/backups", map[string]string{}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("backup = %d %s", resp.StatusCode, body)
	}
	waitForTasks(t, s)
	old, err := s.Store.CreateBackup(state.Backup{SiteID: site.ID, SnapshotID: "fedcba9876543210", Repository: "/var/backups/repo", Kind: "full"})
	if err != nil {
		t.Fatal(err)
	}

	_, body := c.do("GET", base+"/backups", nil)
	var rows []struct {
		ID        int64     `json:"id"`
		Snapshot  string    `json:"snapshot_id"`
		Databases *[]string `json:"databases"`
	}
	if err := json.Unmarshal(body, &rows); err != nil || len(rows) != 2 {
		t.Fatalf("list %s (%v)", body, err)
	}
	if rows[0].ID != old.ID || rows[0].Databases != nil {
		t.Fatalf("a record from before lists databases %v, want null: %s", rows[0].Databases, body)
	}
	taken := rows[1]
	if taken.Databases == nil || !slices.Equal(*taken.Databases, []string{"dbs_data", "dbs_logs"}) {
		t.Fatalf("the backup's databases = %v, want both", taken.Databases)
	}

	// A database made after the backup is the site's, not the snapshot's.
	if resp, body := c.do("POST", base+"/databases", map[string]string{"name": "dbs_late"}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create dbs_late = %d %s", resp.StatusCode, body)
	}
	waitForTasks(t, s)
	agent.forget()
	restore := fmt.Sprintf("/api/backups/%d/restore", taken.ID)
	resp, body := c.do("POST", restore, map[string]any{"confirm": site.Domain, "mode": "database", "databases": []string{"dbs_late"}})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "holds no database") || agent.called(agentrpc.RunBackup.Name()) {
		t.Fatalf("restore of a database the snapshot lacks = %d %s", resp.StatusCode, body)
	}
	if resp, body := c.do("POST", restore, map[string]any{"confirm": site.Domain, "mode": "database", "databases": []string{"dbs_logs"}}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("restore of one database = %d %s", resp.StatusCode, body)
	}
	waitForTasks(t, s)
	if p, ok := agent.last(agentrpc.RestoreSnapshot.Name()).(rpc.RestoreParams); !ok || p.Mode != "database" || !slices.Equal(p.Databases, []string{"dbs_logs"}) {
		t.Fatalf("RestoreSnapshot params %+v", agent.last(agentrpc.RestoreSnapshot.Name()))
	}
	// A record from before leaves the check to the agent.
	if resp, body := c.do("POST", fmt.Sprintf("/api/backups/%d/restore", old.ID), map[string]any{"confirm": site.Domain, "databases": []string{"dbs_late"}}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("restore from a record from before = %d %s", resp.StatusCode, body)
	}
	waitForTasks(t, s)
}
