package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// A restic repository keeps the password it was created with: a new
// backup_password is tested against the repository first, and one that
// does not open it is refused and not stored (else the panel would lose
// every backup at the next run). Clearing the repository is never tested.
func TestBackupPasswordIsTestedBeforeItIsStored(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	save := func(body map[string]string, want int) {
		t.Helper()
		if resp, out := admin.do("PUT", "/api/settings", body); resp.StatusCode != want {
			t.Fatalf("PUT settings %v = %d %s, want %d", body, resp.StatusCode, out, want)
		}
	}
	stored := func() string {
		v, _ := s.Store.Secret(state.SecretBackupPassword)
		return v
	}

	save(map[string]string{"backup_repository": "/var/backups/cloudground/repo", "backup_password": "correct-horse"}, http.StatusOK)
	if !agent.called(agentrpc.TestBackup.Name()) {
		t.Fatal("the first repository and password were not tested")
	}
	agent.setErr(agentrpc.TestBackup.Name(), errors.New("wrong password or no key found"))
	save(map[string]string{"backup_password": "a-different-password"}, http.StatusBadRequest)
	if got := stored(); got != "correct-horse" {
		t.Fatalf("stored %q after a failed test", got)
	}
	agent.setErr(agentrpc.TestBackup.Name(), nil)
	save(map[string]string{"backup_password": "a-different-password"}, http.StatusOK)
	if got := stored(); got != "a-different-password" {
		t.Fatalf("stored %q, want the password that passed", got)
	}
	agent.setErr(agentrpc.TestBackup.Name(), errors.New("unreachable"))
	save(map[string]string{"backup_repository": ""}, http.StatusOK)
}

// The repository is checked by internal/backuprepo before the agent is
// asked: a loopback rest: endpoint, an sftp: host that ssh would read as an
// option and a folder outside the backup roots are refused and not stored;
// the administrator's flag lets a private endpoint through.
func TestBackupRepositoryIsValidated(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	for _, repo := range []string{"rest:https://127.0.0.1:8000/", "sftp:-oProxyCommand=x@h:/r", "/etc/cloudground", "rest:http://example.com/"} {
		resp, out := admin.do("PUT", "/api/settings", map[string]string{"backup_repository": repo, "backup_password": "correct-horse"})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%q: %d %s", repo, resp.StatusCode, out)
		}
	}
	if agent.called(agentrpc.TestBackup.Name()) {
		t.Error("a refused repository reached the agent")
	}
	if ps, _ := s.Store.PanelSettings(); ps.BackupRepository != "" {
		t.Errorf("stored %q", ps.BackupRepository)
	}
	s.BackupPrivateEndpoint = true
	if resp, out := admin.do("PUT", "/api/settings", map[string]string{"backup_repository": "rest:https://127.0.0.1:8000/", "backup_password": "correct-horse"}); resp.StatusCode != http.StatusOK {
		t.Errorf("with the flag: %d %s", resp.StatusCode, out)
	}
}

// Settings read back what was saved; a secret is stored but shown masked;
// an unknown key is refused.
func TestSettingsReadBackAndMaskSecrets(t *testing.T) {
	s, _, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	if resp, body := admin.do("PUT", "/api/settings", map[string]string{"acme_email": "ops@example.com", "backup_password": "supersecret"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT settings = %d %s", resp.StatusCode, body)
	}
	_, body := admin.do("GET", "/api/settings", nil)
	var shown map[string]string
	if err := json.Unmarshal(body, &shown); err != nil {
		t.Fatal(err)
	}
	if shown["acme_email"] != "ops@example.com" {
		t.Errorf("acme_email = %q", shown["acme_email"])
	}
	if strings.Contains(shown["backup_password"], "supersecret") {
		t.Error("the backup password is shown")
	}
	if v, _ := s.Store.Secret(state.SecretBackupPassword); v != "supersecret" {
		t.Errorf("stored password %q", v)
	}
	if resp, _ := admin.do("PUT", "/api/settings", map[string]string{"evil": "x"}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("an unknown setting = %d, want 400", resp.StatusCode)
	}
}

// The settings page saves back what it loaded, read-only keys included:
// refusing that broke every save from an untouched form. Changing a
// read-only key is still refused.
func TestSettingsSaveWhatTheyLoad(t *testing.T) {
	s, _, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	resp, body := admin.do("GET", "/api/settings", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET settings = %d %s", resp.StatusCode, body)
	}
	var form map[string]string
	if err := json.Unmarshal(body, &form); err != nil {
		t.Fatal(err)
	}
	if _, ok := form["panel_domain"]; !ok {
		t.Fatal("the read-only panel_domain is not in what the page loads")
	}
	if resp, body := admin.do("PUT", "/api/settings", form); resp.StatusCode != http.StatusOK {
		t.Fatalf("saving the loaded form = %d %s, want 200", resp.StatusCode, body)
	}
	form["panel_domain"] = "panel.example.com"
	if resp, body := admin.do("PUT", "/api/settings", form); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("changing panel_domain = %d %s, want 400", resp.StatusCode, body)
	}
}

// With update_check off the update status says so and fetches nothing.
func TestUpdateCheckTurnedOff(t *testing.T) {
	s, _, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	if resp, body := admin.do("PUT", "/api/settings", map[string]string{"update_check": "0"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("turn the check off = %d %s", resp.StatusCode, body)
	}
	resp, body := admin.do("GET", "/api/system/update", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update status = %d %s", resp.StatusCode, body)
	}
	var status map[string]any
	if err := json.Unmarshal(body, &status); err != nil {
		t.Fatal(err)
	}
	if status["check_enabled"] != false || status["latest"] != nil {
		t.Fatalf("status with the check off = %v: want check_enabled false and no latest", status)
	}
}
