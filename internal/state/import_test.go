package state

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// openAtVersion opens a database the way a release whose schema stopped at
// version did: its migrations only, no secret store, nothing done at Open
// beyond them.
func openAtVersion(path string, version int) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if _, err := s.applySchema(version); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// legacyFixture is a state.db as the last pre-v1 release left it: settings
// holding panel configuration, plaintext secrets (some of sites since
// deleted, some "deleted" by blanking), certificate observations and alert
// dismissals, next to sites, users and backups.
type legacyFixture struct {
	path             string
	live, stg, error int64 // site ids; error is a site in status error
	gone             int64 // a site id that was deleted
	userID           int64
	settings         map[string]string
}

const (
	fixtureBackupPassword = "legacy-backup-passphrase-7Kq2"
	fixtureS3Secret       = "legacy-s3-secret-Zx81"
	fixtureCFToken        = "legacy-cf-token-Pp93"
	fixtureWebhook        = "https://hooks.example.com/services/T0/B0/legacy-hook-Wm55"
)

func buildLegacy(t *testing.T) legacyFixture {
	t.Helper()
	f := legacyFixture{path: filepath.Join(t.TempDir(), "state.db")}
	old, err := openAtVersion(f.path, versionStateV1-1)
	if err != nil {
		t.Fatalf("old schema: %v", err)
	}
	defer old.Close()
	exec := func(q string, args ...any) sql.Result {
		t.Helper()
		res, err := old.db.Exec(q, args...)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return res
	}
	site := func(domain, status string, stagingOf int64) int64 {
		res := exec(`INSERT INTO sites (domain, aliases, type, profile, engine, php_version, system_user, root_path, status, staging_of, config, created_at, updated_at)
			VALUES (?, '["www.`+domain+`"]', 'wordpress', 'balanced', 'nginx', '8.4', 'cg-site-x', '/srv/sites/`+domain+`', ?, ?, '{"cache_enabled":true,"php":{"memory_limit_mb":256}}', '2026-01-01T00:00:00Z', '2026-01-02T00:00:00Z')`,
			domain, status, stagingOf)
		id, _ := res.LastInsertId()
		return id
	}
	f.live = site("live.example.com", "active", 0)
	f.stg = site("staging.live.example.com", "active", f.live)
	f.error = site("broken.example.com", "error", 0)
	f.gone = site("gone.example.com", "active", 0)
	exec(`DELETE FROM sites WHERE id=?`, f.gone)

	res := exec(`INSERT INTO users (email, password_hash, role, created_at) VALUES ('admin@example.com', 'hash', 'admin', '2026-01-01T00:00:00Z')`)
	f.userID, _ = res.LastInsertId()
	exec(`INSERT INTO user_sites (user_id, site_id) VALUES (?, ?)`, f.userID, f.live)
	exec(`INSERT INTO backups (site_id, snapshot_id, repository, size_bytes, kind, verify_status, created_at) VALUES (?, 'abc123', '/var/backups/repo', 42, 'full', 'verified', '2026-01-03T00:00:00Z')`, f.live)

	id := func(n int64) string { return fmt.Sprint(n) }
	f.settings = map[string]string{
		"acme_email":           "ops@example.com",
		"panel_domain":         "panel.example.com",
		"probe_target":         "https://127.0.0.1:8443",
		"backup_repository":    "s3:https://s3.example.com/bucket",
		"backup_s3_access_key": "AKIAEXAMPLE",
		"backup_s3_region":     "eu-west-1",
		"require_2fa_admin":    "1",
		"update_check":         "0",
		"backup_password":      fixtureBackupPassword,
		"backup_s3_secret_key": fixtureS3Secret,
		"cloudflare_api_token": fixtureCFToken,
		"notify_webhook":       fixtureWebhook,
		"rendered_version":     "0.9.0/3",

		"site." + id(f.live) + ".db_password":     "live-db-pw-Hh21",
		"site." + id(f.live) + ".connector_token": "live-conn-Jj34",
		"site." + id(f.stg) + ".db_password":      "stg-db-pw-Kk56",
		"site." + id(f.stg) + ".connector_token":  "stg-conn-Ll78",
		"site." + id(f.stg) + ".staging_password": "stg-http-pw-Mm90",
		"site." + id(f.error) + ".db_password":    "", // blanked: how a secret used to be "deleted"
		"site." + id(f.gone) + ".db_password":     "gone-db-pw-Nn12",
		"site." + id(f.gone) + ".connector_token": "gone-conn-Oo34",
		"cert_expiry." + id(f.live):               "2026-12-01T00:00:00Z",
		"cert_error." + id(f.live):                "rate limited",
		"cert_expiry." + id(f.gone):               "2026-11-01T00:00:00Z",
		"cert_error." + id(f.stg):                 "",
		"alert_dismissed.0123456789ab":            "2026-02-01T00:00:00Z",
		"alert_dismissed.ba9876543210":            "2026-02-02T00:00:00Z",
	}
	for k, v := range f.settings {
		exec(`INSERT INTO settings (key, value) VALUES (?, ?)`, k, v)
	}
	return f
}

// secretsOf are the plaintext values the import must remove from the file.
func (f legacyFixture) secretValues() []string {
	var out []string
	for k, v := range f.settings {
		if v != "" && (strings.HasPrefix(k, "site.") || k == "backup_password" || k == "backup_s3_secret_key" || k == "cloudflare_api_token" || k == "notify_webhook") {
			out = append(out, v)
		}
	}
	return out
}

func TestImportFromLegacySettings(t *testing.T) {
	f := buildLegacy(t)
	s, err := Open(f.path)
	if err != nil {
		t.Fatalf("open with the v1 code: %v", err)
	}
	defer s.Close()

	// Panel settings, typed.
	ps, err := s.PanelSettings()
	if err != nil {
		t.Fatal(err)
	}
	want := PanelSettings{
		ACMEEmail: "ops@example.com", PanelDomain: "panel.example.com", ProbeTarget: "https://127.0.0.1:8443",
		BackupRepository: "s3:https://s3.example.com/bucket", BackupS3AccessKey: "AKIAEXAMPLE", BackupS3Region: "eu-west-1",
		Require2FAAdmin: true, UpdateCheck: false, UpdateChannel: "stable", VulnerabilityCheck: true,
	}
	if ps != want {
		t.Errorf("panel settings = %+v\nwant %+v", ps, want)
	}

	// Secrets, sealed and readable.
	for name, v := range map[string]string{
		SecretBackupPassword:                          fixtureBackupPassword,
		SecretBackupS3SecretKey:                       fixtureS3Secret,
		SecretCloudflareToken:                         fixtureCFToken,
		SecretNotifyWebhook:                           fixtureWebhook,
		SiteSecret(f.live, SiteSecretDBPassword):      "live-db-pw-Hh21",
		SiteSecret(f.live, SiteSecretConnectorToken):  "live-conn-Jj34",
		SiteSecret(f.stg, SiteSecretDBPassword):       "stg-db-pw-Kk56",
		SiteSecret(f.stg, SiteSecretConnectorToken):   "stg-conn-Ll78",
		SiteSecret(f.stg, SiteSecretStagingPassword):  "stg-http-pw-Mm90",
		SiteSecret(f.error, SiteSecretDBPassword):     "",
		SiteSecret(f.gone, SiteSecretDBPassword):      "",
		SiteSecret(f.gone, SiteSecretConnectorToken):  "",
		SiteSecret(f.error, SiteSecretConnectorToken): "",
	} {
		got, err := s.Secret(name)
		if err != nil || got != v {
			t.Errorf("secret %s = %q, %v; want %q", name, got, err, v)
		}
	}
	var nSecrets int
	s.db.QueryRow(`SELECT COUNT(*) FROM secrets`).Scan(&nSecrets)
	if nSecrets != 9 {
		t.Errorf("%d secrets stored, want 9 (orphans and blanks dropped)", nSecrets)
	}

	// Certificate observations of existing sites; the deleted site's dropped.
	certs, err := s.CertStates()
	if err != nil {
		t.Fatal(err)
	}
	if c := certs[f.live]; !c.Expiry.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) || c.Error != "rate limited" {
		t.Errorf("live cert state = %+v", c)
	}
	if _, ok := certs[f.stg]; ok {
		t.Errorf("a blank cert_error made a site_status row: %+v", certs[f.stg])
	}
	if len(certs) != 1 {
		t.Errorf("cert states = %+v, want only the live site's", certs)
	}

	// Migration 11 moved the dismissals out of settings; migration 19
	// replaced them with the alerts table, which starts empty: the old ones
	// named hashes of computed alerts (SPEC.md, Migrations).
	var alerts int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM alerts`).Scan(&alerts); err != nil || alerts != 0 {
		t.Errorf("alerts after the import = %d, %v", alerts, err)
	}
	if v, err := s.RenderedVersion(); err != nil || v != "0.9.0/3" {
		t.Errorf("rendered version = %q, %v", v, err)
	}

	// Nothing else is lost.
	sites, err := s.ListSites()
	if err != nil || len(sites) != 3 {
		t.Fatalf("sites = %d, %v", len(sites), err)
	}
	live, err := s.GetSite(f.live)
	if err != nil || live.Domain != "live.example.com" || len(live.Aliases) != 1 || !live.Config.CacheEnabled || live.Config.PHP.MemoryLimitMB != 256 {
		t.Errorf("live site = %+v, %v", live, err)
	}
	if live.DesiredGeneration != 1 || live.AppliedGeneration != 1 || live.LastApplyError != "" {
		t.Errorf("an active site imports as desired 1 applied 1, got %d/%d %q", live.DesiredGeneration, live.AppliedGeneration, live.LastApplyError)
	}
	if broken, _ := s.GetSite(f.error); broken.AppliedGeneration != 0 {
		t.Errorf("a site in error imports as applied %d, want 0", broken.AppliedGeneration)
	}
	if stg, _ := s.StagingSiteFor(f.live); stg.ID != f.stg {
		t.Errorf("staging link lost: %+v", stg)
	}
	if ok, err := s.UserCanAccessSite(f.userID, f.live); err != nil || !ok {
		t.Errorf("user's site assignment lost (%v, %v)", ok, err)
	}
	if b, err := s.ListBackups(f.live, 10); err != nil || len(b) != 1 || b[0].SnapshotID != "abc123" || b[0].VerifyStatus != "verified" {
		t.Errorf("backups = %+v, %v", b, err)
	}

	// settings is empty: every key moved or was an orphan.
	var left []string
	rows, _ := s.db.Query(`SELECT key FROM settings`)
	for rows.Next() {
		var k string
		rows.Scan(&k)
		left = append(left, k)
	}
	rows.Close()
	if len(left) != 0 {
		t.Errorf("settings still holds %v", left)
	}

	// No plaintext secret survives anywhere in the files.
	s.Close()
	for _, suffix := range []string{"", "-wal"} {
		raw, err := os.ReadFile(f.path + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range f.secretValues() {
			if bytes.Contains(raw, []byte(v)) {
				t.Errorf("state.db%s still contains the plaintext secret %q", suffix, v)
			}
		}
	}
}

func TestImportIsIdempotent(t *testing.T) {
	f := buildLegacy(t)
	s, err := Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := func(s *Store) string {
		var b strings.Builder
		for _, q := range []string{
			`SELECT name, hex(sealed), updated_at, COALESCE(lookup, '') FROM secrets ORDER BY name`,
			`SELECT ` + panelCols + ` FROM panel_settings`,
			`SELECT site_id, cert_expiry, cert_error FROM site_status ORDER BY site_id`,
			`SELECT key, public_id, dismissed_at FROM alerts ORDER BY key`,
			`SELECT key, value FROM meta ORDER BY key`,
			`SELECT ` + siteCols + ` FROM sites ORDER BY id`,
			`SELECT key, value FROM settings ORDER BY key`,
			`SELECT MAX(version) FROM schema_migrations`,
		} {
			rows, err := s.db.Query(q)
			if err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			cols, _ := rows.Columns()
			for rows.Next() {
				vals := make([]any, len(cols))
				ptrs := make([]any, len(cols))
				for i := range vals {
					ptrs[i] = &vals[i]
				}
				rows.Scan(ptrs...)
				fmt.Fprintln(&b, vals...)
			}
			rows.Close()
		}
		return b.String()
	}
	first := snapshot(s)
	s.Close()

	again, err := Open(f.path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer again.Close()
	if second := snapshot(again); second != first {
		t.Fatalf("a second Open changed the state:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

// The panel API user cannot regenerate a key for secrets sealed with
// another: that would orphan all of them.
func TestSecretKeyLostOrWrongIsRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetSecret(SecretBackupPassword, "pw"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	keyPath := filepath.Join(dir, "secret.key")
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("key file mode %04o, want 0600", info.Mode().Perm())
	}
	key, _ := os.ReadFile(keyPath)

	os.Remove(keyPath)
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("open with the key gone = %v, want a refusal", err)
	}
	if _, err := os.Stat(keyPath); err == nil {
		t.Fatal("a new key was created for a database sealed with another")
	}
	os.WriteFile(keyPath, []byte(strings.Repeat("ab", secretKeyLen)+"\n"), 0o600)
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "not the key") {
		t.Fatalf("open with a wrong key = %v, want a refusal", err)
	}
	os.WriteFile(keyPath, key, 0o600)
	s, err = Open(path)
	if err != nil {
		t.Fatalf("open with the key restored: %v", err)
	}
	defer s.Close()
	if v, err := s.Secret(SecretBackupPassword); err != nil || v != "pw" {
		t.Fatalf("secret after restoring the key = %q, %v", v, err)
	}
}

// Opened without a key (recover-admin), the store runs, but has no secrets
// and leaves the legacy ones in place for the next Open with a key.
func TestOpenWithoutKeyLeavesLegacySecrets(t *testing.T) {
	f := buildLegacy(t)
	s, err := OpenWith(f.path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Secret(SecretBackupPassword); !errors.Is(err, ErrNoSecretKey) {
		t.Errorf("Secret without a key = %v, want ErrNoSecretKey", err)
	}
	if ps, err := s.PanelSettings(); err != nil || ps.ACMEEmail != "ops@example.com" {
		t.Errorf("panel settings without a key = %+v, %v", ps, err)
	}
	s.Close()
	if _, err := os.Stat(filepath.Join(filepath.Dir(f.path), "secret.key")); err == nil {
		t.Error("a keyless open created a key file")
	}
	s, err = Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if v, err := s.Secret(SecretBackupPassword); err != nil || v != fixtureBackupPassword {
		t.Errorf("after the keyed open: %q, %v", v, err)
	}
}
