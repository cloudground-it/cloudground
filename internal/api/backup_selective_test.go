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

// A files restore of chosen paths is refused for bad paths and other modes,
// and otherwise runs behind the same restore-tested safety snapshot, the
// cleaned paths reaching the agent.
func TestSelectiveRestoreTakesTheSafetySnapshotFirst(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := createViaAPI(t, s, admin, staticSite("paths.example.com"))
	setBackupRepo(t, s, "/var/backups/cloudground/repo", "test-repository-password")
	old, err := s.Store.CreateBackup(state.Backup{SiteID: site.ID, SnapshotID: "abcdef1234567890", Repository: "/var/backups/cloudground/repo", Kind: "full"})
	if err != nil {
		t.Fatal(err)
	}
	agent.setResult(agentrpc.RunBackup.Name(), rpc.BackupResult{SnapshotID: "fedcba0987654321", SizeBytes: 42})
	agent.setResult(agentrpc.VerifyBackup.Name(), rpc.VerifyResult{Passed: true})
	path := fmt.Sprintf("/api/backups/%d/restore", old.ID)
	for _, bad := range []map[string]any{
		{"confirm": site.Domain, "mode": "full", "paths": []string{"shared/wp-config.php"}},
		{"confirm": site.Domain, "mode": "files", "paths": []string{"../etc/passwd"}},
		{"confirm": site.Domain, "mode": "files", "paths": []string{"/etc/passwd"}},
		{"confirm": site.Domain, "mode": "files", "paths": []string{".ssh/authorized_keys"}},
		{"confirm": site.Domain, "mode": "files", "paths": []string{"releases"}},
		{"confirm": site.Domain, "mode": "files", "paths": []string{"."}},
	} {
		if resp, body := admin.do("POST", path, bad); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("restore %v = %d %s, want 400", bad, resp.StatusCode, body)
		}
	}
	if agent.called(agentrpc.RunBackup.Name()) {
		t.Fatal("a refused restore took a snapshot")
	}
	body := map[string]any{"confirm": site.Domain, "mode": "files", "paths": []string{"shared//wp-config.php", "releases/r1/index.php"}}
	if resp, b := admin.do("POST", path, body); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("restore = %d %s", resp.StatusCode, b)
	}
	waitForTasks(t, s)
	agent.mu.Lock()
	var order []string
	for _, c := range agent.history {
		if c.method == agentrpc.RunBackup.Name() || c.method == agentrpc.RestoreSnapshot.Name() {
			order = append(order, c.method)
		}
	}
	p, _ := agent.lastParams[agentrpc.RestoreSnapshot.Name()].(rpc.RestoreParams)
	agent.mu.Unlock()
	if strings.Join(order, ",") != "RunBackup,RestoreSnapshot" {
		t.Fatalf("order %v: the safety snapshot must come first", order)
	}
	if strings.Join(p.Paths, ",") != "shared/wp-config.php,releases/r1/index.php" || p.Mode != "files" {
		t.Fatalf("restore params %+v", p)
	}
	if !hasBackup(t, s, site.ID, "pre-restore", "fedcba0987654321") {
		t.Fatal("no verified pre-restore snapshot")
	}
}

// A download is prepared by a job, then streamed in chunks from the agent,
// audited, and deleted once sent; before it is ready the route says so.
func TestBackupDownloadIsStreamedAndAudited(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := createViaAPI(t, s, admin, staticSite("dl.example.com"))
	setBackupRepo(t, s, "/var/backups/cloudground/repo", "test-repository-password")
	b, _ := s.Store.CreateBackup(state.Backup{SiteID: site.ID, SnapshotID: "abcdef1234567890", Repository: "/var/backups/cloudground/repo", Kind: "full"})

	if resp, body := admin.do("POST", fmt.Sprintf("/api/backups/%d/archive", b.ID), map[string]string{"part": "everything"}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a bad part = %d %s", resp.StatusCode, body)
	}
	agent.setErr(agentrpc.ReadArchiveChunk.Name(), &agentrpc.Error{Code: agentrpc.CodeNotFound, Message: "no such archive"})
	if resp, body := admin.do("GET", fmt.Sprintf("/api/backups/%d/archive?part=files", b.ID), nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("download before it is ready = %d %s", resp.StatusCode, body)
	}
	agent.setResult(agentrpc.ArchiveSnapshot.Name(), rpc.ArchiveResult{Name: rpc.ArchiveName(b.SnapshotID, "files"), Size: 5})
	if resp, body := admin.do("POST", fmt.Sprintf("/api/backups/%d/archive", b.ID), map[string]string{"part": "files"}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("archive = %d %s", resp.StatusCode, body)
	}
	waitForTasks(t, s)
	agent.mu.Lock()
	ap, _ := agent.lastParams[agentrpc.ArchiveSnapshot.Name()].(rpc.ArchiveParams)
	agent.mu.Unlock()
	if ap.Part != "files" || ap.SnapshotID != b.SnapshotID || ap.Site.ID != site.ID {
		t.Fatalf("archive params %+v", ap)
	}
	agent.setErr(agentrpc.ReadArchiveChunk.Name(), nil)
	agent.setResult(agentrpc.ReadArchiveChunk.Name(), rpc.ArchiveChunkResult{Data: []byte("hello"), Size: 5})
	resp, body := admin.do("GET", fmt.Sprintf("/api/backups/%d/archive?part=files", b.ID), nil)
	if resp.StatusCode != http.StatusOK || string(body) != "hello" || resp.Header.Get("Content-Type") != "application/gzip" ||
		!strings.Contains(resp.Header.Get("Content-Disposition"), "dl.example.com-abcdef123456-files.tar.gz") {
		t.Fatalf("download = %d %q %v", resp.StatusCode, body, resp.Header)
	}
	if !agent.called(agentrpc.DeleteArchive.Name()) {
		t.Fatal("the archive was not deleted once sent")
	}
	_, audit := admin.do("GET", "/api/audit", nil)
	if !strings.Contains(string(audit), "backup.download") {
		t.Fatal("the download is not audited")
	}
	// A read-only account cannot download a site's backup.
	if resp, _ := admin.do("POST", "/api/users", map[string]any{"email": "ro@example.com", "password": adminPassword, "role": "readonly"}); resp.StatusCode != http.StatusCreated {
		t.Fatal("create readonly")
	}
	ro := signIn(t, ts, "ro@example.com", adminPassword)
	if resp, _ := ro.do("GET", fmt.Sprintf("/api/backups/%d/archive?part=files", b.ID), nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("read-only download = %d", resp.StatusCode)
	}
}

// The SFTP key is the panel's, sealed, its public half shown; it reaches the
// agent only for an sftp: repository, and B2's secrets as restic's
// variables.
func TestDestinationCredentialsReachTheAgent(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	if _, body := admin.do("GET", "/api/settings/backup-ssh-key", nil); !strings.Contains(string(body), `"public_key":""`) {
		t.Fatalf("no key yet: %s", body)
	}
	resp, body := admin.do("POST", "/api/settings/backup-ssh-key", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "ssh-ed25519 ") {
		t.Fatalf("new key = %d %s", resp.StatusCode, body)
	}
	private, _ := s.Store.Secret(state.SecretBackupSFTPKey)
	if !strings.Contains(private, "OPENSSH PRIVATE KEY") {
		t.Fatal("the private key is not sealed")
	}
	agent.setResult(agentrpc.TestBackup.Name(), map[string]any{"reachable": true})
	if resp, body := admin.do("PUT", "/api/settings", map[string]string{
		"backup_repository": "sftp:backup@203.0.113.9:/srv/repo", "backup_password": "repository-password",
		"backup_b2_account_id": "b2-id", "backup_b2_account_key": "b2-key",
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("settings = %d %s", resp.StatusCode, body)
	}
	agent.mu.Lock()
	tp, _ := agent.lastParams[agentrpc.TestBackup.Name()].(rpc.TestBackupParams)
	agent.mu.Unlock()
	if tp.Env[envSFTPKey] != private || tp.Env["B2_ACCOUNT_ID"] != "b2-id" || tp.Env["B2_ACCOUNT_KEY"] != "b2-key" {
		t.Fatalf("the pairing test's credentials: %v", keysOf(tp.Env))
	}
	if _, body := admin.do("GET", "/api/settings", nil); strings.Contains(string(body), "b2-key") || strings.Contains(string(body), "PRIVATE KEY") {
		t.Fatalf("a secret is shown: %s", body)
	}
	// Another destination carries no key.
	ps, _ := s.Store.PanelSettings()
	ps.BackupRepository = "/var/backups/cloudground/repo"
	if env := s.backupEnvFor(ps, nil); env[envSFTPKey] != "" {
		t.Fatal("the SFTP key went to a local repository")
	}
}

func keysOf(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
