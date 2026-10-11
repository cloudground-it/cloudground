package state

import (
	"strings"
	"testing"
)

const (
	repoPlain  = "rest:https://backup:repo-pass-0f3a@backups.example.com/cg"
	repoSealed = "rest:https://backup:{sealed}@backups.example.com/cg"
)

func rawRepos(t *testing.T, s *Store) []string {
	t.Helper()
	var out []string
	rows, err := s.db.Query(`SELECT backup_repository FROM panel_settings UNION ALL SELECT repository FROM backups`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// A repository's URL password is sealed in panel_settings and in backups,
// and every read gives back the URL that was stored (#48).
func TestRepositoryPasswordIsSealed(t *testing.T) {
	s, _ := newStore(t)
	site := seedSite(t, s, "repo.example.com", SiteStatic, SiteActive, 0)
	if _, err := s.UpdatePanelSettings(func(p *PanelSettings) error { p.BackupRepository = repoPlain; return nil }); err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateBackup(Backup{SiteID: site.ID, SnapshotID: "0123456789abcdef", Kind: "full", Repository: repoPlain})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBackup(Backup{SiteID: site.ID, SnapshotID: "fedcba9876543210", Kind: "full", Repository: "/var/backups/cloudground/repo"}); err != nil {
		t.Fatal(err)
	}
	for _, r := range rawRepos(t, s) {
		if strings.Contains(r, "repo-pass-0f3a") {
			t.Fatalf("a row holds the password: %q", r)
		}
	}
	if ps, err := s.PanelSettings(); err != nil || ps.BackupRepository != repoPlain {
		t.Fatalf("settings = %q, %v", ps.BackupRepository, err)
	}
	if got, err := s.GetBackup(b.ID); err != nil || got.Repository != repoPlain {
		t.Fatalf("backup = %q, %v", got.Repository, err)
	}
	list, err := s.ListBackups(site.ID, 0)
	if err != nil || len(list) != 2 || list[1].Repository != repoPlain || list[0].Repository != "/var/backups/cloudground/repo" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if n, err := s.CountBackups(site.ID, repoPlain); err != nil || n != 1 {
		t.Fatalf("count = %d, %v", n, err)
	}
	if err := s.SetBackupVerified(b.ID, VerifyPassed, 10); err != nil {
		t.Fatal(err)
	}
	if got, err := s.LatestVerifiedBackup(site.ID, repoPlain); err != nil || got.ID != b.ID || got.Repository != repoPlain {
		t.Fatalf("latest verified = %+v, %v", got, err)
	}
	if repos, err := s.BackupRepositories(); err != nil || len(repos) != 2 || !strings.Contains(strings.Join(repos, " "), repoPlain) {
		t.Fatalf("repositories = %v, %v", repos, err)
	}
	// A new password for the same repository replaces the old one.
	newer := strings.Replace(repoPlain, "repo-pass-0f3a", "rotated-pass-77", 1)
	if _, err := s.UpdatePanelSettings(func(p *PanelSettings) error { p.BackupRepository = newer; return nil }); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetBackup(b.ID); got.Repository != newer {
		t.Fatalf("after the rotation the record opens to %q", got.Repository)
	}
	if n, err := s.DeleteBackupsBySnapshot(site.ID, newer, []string{"0123456789abcdef"}); err != nil || n != 1 {
		t.Fatalf("delete by snapshot = %d, %v", n, err)
	}
}

// Rows written before the passwords were sealed are sealed on Open, and the
// file is compacted so the plaintext is gone from it; a store opened
// without a key reads the sealed form.
func TestOpenSealsPlaintextRepositoryPasswords(t *testing.T) {
	s, path := newStore(t)
	site := seedSite(t, s, "legacy.example.com", SiteStatic, SiteActive, 0)
	if _, err := s.db.Exec(`UPDATE panel_settings SET backup_repository=? WHERE id=1`, repoPlain); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO backups (site_id, snapshot_id, repository, size_bytes, kind, verify_status, restore_est_ms, created_at)
		VALUES (?, 'abcdef0123456789', ?, 0, 'full', 'passed', 0, ?)`, site.ID, repoPlain, now()); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rawRepos(t, s2) {
		if r != repoSealed {
			t.Fatalf("a row after Open: %q", r)
		}
	}
	if ps, err := s2.PanelSettings(); err != nil || ps.BackupRepository != repoPlain {
		t.Fatalf("settings = %q, %v", ps.BackupRepository, err)
	}
	s2.Close()
	if fileHolds(t, path, "repo-pass-0f3a") {
		t.Fatal("the password is still in the database file")
	}

	bare, err := OpenWith(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer bare.Close()
	if ps, err := bare.PanelSettings(); err != nil || ps.BackupRepository != repoSealed {
		t.Fatalf("without a key: %q, %v", ps.BackupRepository, err)
	}
}

func TestSplitRepo(t *testing.T) {
	for in, want := range map[string][2]string{
		repoPlain:                  {repoSealed, "repo-pass-0f3a"},
		repoSealed:                 {repoSealed, ""},
		"rest:https://host/cg":     {"rest:https://host/cg", ""},
		"rest:https://user@host/":  {"rest:https://user@host/", ""},
		"s3:s3.example.com/bucket": {"s3:s3.example.com/bucket", ""},
		"/var/backups/cloudground": {"/var/backups/cloudground", ""},
	} {
		if stored, pw := splitRepo(in); stored != want[0] || pw != want[1] {
			t.Errorf("splitRepo(%q) = %q, %q", in, stored, pw)
		}
	}
}
