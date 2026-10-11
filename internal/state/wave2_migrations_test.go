package state

import (
	"fmt"
	"path/filepath"
	"testing"
)

// The Phase 4 wave 2 migrations, from a database at 15 (before all of
// them), 16 and 17: 16 moves the TOTP secrets out of users, 17 is a no-op,
// 18 makes deployments a state machine, 19 brings the alerts table and the
// connector-token lookup. Every table, column and index they make is there
// afterwards, and what 16 and 18 move is where they put it.
func TestWave2MigrationsFrom15To19(t *testing.T) {
	if latestSchema < 19 {
		t.Fatalf("schema version %d; this test covers up to 19", latestSchema)
	}
	for _, from := range []int{15, 16, 17} {
		t.Run(fmt.Sprintf("from %d", from), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			old, err := openAtVersion(path, from)
			if err != nil {
				t.Fatal(err)
			}
			exec := func(q string, args ...any) {
				t.Helper()
				if _, err := old.db.Exec(q, args...); err != nil {
					t.Fatalf("%s: %v", q, err)
				}
			}
			exec(`INSERT INTO users (id, email, password_hash, role, created_at) VALUES (1, 'a@example.com', 'x', 'admin', '2026-10-01T00:00:00Z')`)
			if from == 15 {
				exec(`UPDATE users SET totp_secret='JBSWY3DPEHPK3PXP', totp_enabled=1 WHERE id=1`)
			}
			exec(`INSERT INTO sites (id, domain, type, profile, system_user, root_path, status, created_at, updated_at)
				VALUES (7, 'm.example.com', 'php', 'balanced', 'cg-site-7', '/srv/sites/m', 'active', '2026-10-01T00:00:00Z', '2026-10-01T00:00:00Z')`)
			// Two releases left promoted (the newest is the live one), a
			// guarding one with a release and one without.
			for _, d := range []struct {
				id                    int64
				release, path, status string
				promoted              string
			}{
				{1, "r1", "/r/1", "promoted", "2026-10-01T01:00:00Z"},
				{2, "r2", "/r/2", "promoted", "2026-10-01T02:00:00Z"},
				{3, "r3", "/r/3", "guarding", ""},
				{4, "r4", "", "guarding", ""},
			} {
				var promoted any
				if d.promoted != "" {
					promoted = d.promoted
				}
				exec(`INSERT INTO deployments (id, site_id, release_id, path, status, created_at, promoted_at)
					VALUES (?, 7, ?, ?, ?, '2026-10-01T00:00:00Z', ?)`, d.id, d.release, d.path, d.status, promoted)
			}
			old.Close()

			// The schema at 19, before an Open with the key: 16's holding
			// table still holds what it moved.
			mid, err := openAtVersion(path, 19)
			if err != nil {
				t.Fatal(err)
			}
			if from == 15 {
				var held int
				if err := mid.db.QueryRow(`SELECT COUNT(*) FROM totp_import WHERE user_id=1 AND slot='active'`).Scan(&held); err != nil || held != 1 {
					t.Fatalf("totp_import after 16 = %d, %v", held, err)
				}
			}
			mid.Close()

			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			var version int
			if err := s.db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != latestSchema {
				t.Fatalf("schema version = %d, %v", version, err)
			}

			has := func(kind, name string) bool {
				var n int
				s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type=? AND name=?`, kind, name).Scan(&n)
				return n == 1
			}
			columns := func(table string) map[string]bool {
				out := map[string]bool{}
				rows, err := s.db.Query(`SELECT name FROM pragma_table_info(?)`, table)
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()
				for rows.Next() {
					var c string
					rows.Scan(&c)
					out[c] = true
				}
				return out
			}

			// 16: the secrets left users; the holding table is emptied and
			// dropped at Open with the key, into the secret store.
			users := columns("users")
			if users["totp_secret"] || users["totp_pending"] {
				t.Fatalf("users still has its TOTP columns: %v", users)
			}
			if has("table", "totp_import") {
				t.Fatal("totp_import survived an Open with the key")
			}
			if from == 15 {
				if secret, on, err := s.UserTOTP(1); err != nil || !on || secret != "JBSWY3DPEHPK3PXP" {
					t.Fatalf("the TOTP secret after 16 = %q %v %v", secret, on, err)
				}
			}

			// 18: the columns, the live index, the statuses.
			dep := columns("deployments")
			for _, c := range []string{"rollback_to", "regressed", "note"} {
				if !dep[c] {
					t.Errorf("deployments has no %s", c)
				}
			}
			if !has("index", "idx_deployments_live") {
				t.Error("no idx_deployments_live")
			}
			want := map[int64]string{1: "superseded", 2: "promoted", 3: "created", 4: "failed"}
			for id, st := range want {
				var got string
				if err := s.db.QueryRow(`SELECT status FROM deployments WHERE id=?`, id).Scan(&got); err != nil || got != st {
					t.Errorf("deployment %d = %q, %v; want %s", id, got, err, st)
				}
			}
			if _, err := s.db.Exec(`UPDATE deployments SET status='promoted' WHERE id=1`); err == nil {
				t.Error("a second promoted deployment of the site was accepted")
			}

			// 19: the alerts table, the dismissals gone, the token lookup.
			if !has("table", "alerts") || has("table", "alert_dismissals") {
				t.Fatal("alerts table missing or alert_dismissals left")
			}
			al := columns("alerts")
			for _, c := range []string{"id", "key", "public_id", "kind", "site_id", "subject", "level", "fingerprint",
				"message", "text_key", "text_count", "text_args", "detail_key", "detail_args", "at",
				"raised_at", "seen_at", "dismissed_at", "notified_at"} {
				if !al[c] {
					t.Errorf("alerts has no %s", c)
				}
			}
			for _, ix := range []string{"idx_alerts_kind", "idx_alerts_site", "idx_secrets_lookup"} {
				if !has("index", ix) {
					t.Errorf("no index %s", ix)
				}
			}
			if !columns("secrets")["lookup"] {
				t.Error("secrets has no lookup")
			}
			var upgraded int
			s.db.QueryRow(`SELECT COUNT(*) FROM meta WHERE key='alerts_upgraded'`).Scan(&upgraded)
			if upgraded != 1 {
				t.Error("a database with accounts is not marked upgraded to the alerts table")
			}
		})
	}
}
