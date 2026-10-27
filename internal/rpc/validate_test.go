package rpc

import (
	"testing"

	"github.com/cloudground-it/cloudground/internal/state"
)

// The rules of SPEC.md §3, value by value: what passes, and the message
// of what does not.
func TestValidate(t *testing.T) {
	col := &DBNewColumn{Name: "c", Type: "int"}
	idx := &DBNewIndex{Name: "i"}
	cases := []struct {
		v    interface{ Validate() error }
		want string // "" passes
	}{
		{&FirewallRuleParams{Action: "allow", Port: 22}, ""},
		{&FirewallRuleParams{Action: "deny", Port: 443, Proto: "udp", From: "10.0.0.0/8"}, ""},
		{&FirewallRuleParams{Action: "delete", Port: 65535, From: "2001:db8::1"}, ""},
		{&FirewallRuleParams{Action: "allow", Port: 0}, "invalid port 0"},
		{&FirewallRuleParams{Action: "allow", Port: 65536}, "invalid port 65536"},
		{&FirewallRuleParams{Action: "allow", Port: 80, From: "evil; rm"}, `invalid source address "evil; rm"`},
		{&FirewallRuleParams{Action: "reject", Port: 80}, `invalid action "reject"`},
		{&ManageFileParams{Operation: "mkdir"}, ""},
		{&ManageFileParams{Operation: "delete"}, ""},
		{&ManageFileParams{Operation: "rename", DestPath: "b"}, ""},
		{&ManageFileParams{Operation: "rename", DestPath: "  "}, "destination path required"},
		{&ManageFileParams{Operation: "chmod"}, `unsupported file operation "chmod"`},
		{&SSHKeyParams{Action: "list"}, ""},
		{&SSHKeyParams{Action: "add"}, ""},
		{&SSHKeyParams{Action: "rotate"}, `unsupported SSH key action "rotate"`},
		{&SSHKeyParams{Action: "add", Access: "shell"}, ""},
		{&SSHKeyParams{Action: "add", Access: "root"}, `unsupported SSH key access "root"`},
		{&SiteDatabaseParams{Site: state.Site{ID: 1}, Name: "site_1"}, ""},
		{&SiteDatabaseParams{Site: state.Site{ID: 1}, Name: "shop_data"}, ""},
		{&SiteDatabaseParams{Site: state.Site{ID: 1}, Name: "site_2"}, `"site_2" is reserved for the site with that number`},
		{&SiteDatabaseParams{Site: state.Site{ID: 1}, Name: "mysql"}, `"mysql" is reserved`},
		{&DBUserParams{Site: state.Site{ID: 1}, User: "reporting", Grants: []state.DBGrant{{Database: "shop_data", Level: "read"}}}, ""},
		{&DBUserParams{Site: state.Site{ID: 1}, User: "reporting", Grants: []state.DBGrant{{Database: "shop_data", Level: "write"}}}, `invalid grant level "write"`},
		{&DBUserParams{Site: state.Site{ID: 1}, User: "root"}, `"root" is reserved`},
		{&BackupParams{}, ""},
		{&BackupParams{Kind: "database"}, ""},
		{&BackupParams{Kind: "everything"}, "backup kind must be full, files, or database"},
		{&RestoreParams{}, ""},
		{&RestoreParams{Mode: " Files "}, ""},
		{&RestoreParams{Mode: "partial"}, "restore mode must be full, files, or database"},
		{&DeployParams{ReleaseID: "20261009-161248"}, ""},
		{&DeployParams{ReleaseID: "initial"}, `invalid release id "initial"`},
		{&DeployParams{ReleaseID: "../x"}, `invalid release id "../x"`},
		{&ReleaseParams{}, ""},
		{&ReleaseParams{ReleaseID: "initial"}, ""},
		{&ReleaseParams{ReleaseID: "20261009-161248.123-9f2c1a"}, ""},
		{&ReleaseParams{ReleaseID: "x/y"}, `invalid release id "x/y"`},
		{&MigrationParams{ReleaseID: "20261009-161248"}, ""},
		{&MigrationParams{}, "invalid migration release id"},
		{&DeployParams{ReleaseID: "01JA2B3C4D5E6F7G8H9JKMNPQR"}, ""},
		{&DeployParams{ReleaseID: "01ja2b3c4d5e6f7g8h9jkmnpqr"}, `invalid release id "01ja2b3c4d5e6f7g8h9jkmnpqr"`},
		{&ReleaseParams{ReleaseID: "01JA2B3C4D5E6F7G8H9JKMNPQR"}, ""},
		{&MigrationParams{ReleaseID: "01JA2B3C4D5E6F7G8H9JKMNPQR"}, ""},
		{&RemoveReleasesParams{ReleaseIDs: []string{"01JA2B3C4D5E6F7G8H9JKMNPQR", "20261009-161248"}}, ""},
		{&RemoveReleasesParams{ReleaseIDs: []string{"initial"}}, `invalid release id "initial"`},
		{&RemoveReleasesParams{ReleaseIDs: []string{"../x"}}, `invalid release id "../x"`},
		{&GitSyncParams{Site: state.Site{RootPath: "/srv/sites/a.com"}, Repo: "https://example.com/a.git", Branch: "main", ReleaseID: "01JA2B3C4D5E6F7G8H9JKMNPQR"}, ""},
		{&GitSyncParams{Site: state.Site{RootPath: "/srv/sites/a.com"}, Repo: "file:///srv/sites/a.com/tmp/r.git", Branch: "main", ReleaseID: "01JA2B3C4D5E6F7G8H9JKMNPQR", BuildCommand: "make",
			DeployKey: "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----\n"}, ""},
		{&GitSyncParams{Site: state.Site{RootPath: "/srv/sites/a.com"}, Repo: "https://example.com/a.git", Branch: "main", ReleaseID: "initial"}, `invalid release id "initial"`},
		{&GitSyncParams{Site: state.Site{RootPath: "/srv/sites/a.com"}, Repo: "file:///etc", Branch: "main", ReleaseID: "01JA2B3C4D5E6F7G8H9JKMNPQR"}, "a file repository must be inside the site's directory /srv/sites/a.com"},
		{&GitSyncParams{Site: state.Site{RootPath: "/srv/sites/a.com"}, Repo: "https://example.com/a.git", Branch: "-x", ReleaseID: "01JA2B3C4D5E6F7G8H9JKMNPQR"}, "the branch cannot start with - / or ."},
		{&GitSyncParams{Site: state.Site{RootPath: "/srv/sites/a.com"}, Repo: "https://example.com/a.git", Branch: "main", ReleaseID: "01JA2B3C4D5E6F7G8H9JKMNPQR", BuildCommand: "a\nb"}, "the command must be a single line without control characters"},
		{&GitSyncParams{Site: state.Site{RootPath: "/srv/sites/a.com"}, Repo: "https://example.com/a.git", Branch: "main", ReleaseID: "01JA2B3C4D5E6F7G8H9JKMNPQR", DeployKey: "not a key"}, "the deploy key is not one OpenSSH private key"},
		{&ForgetSnapshotParams{SnapshotID: "abcdef12"}, ""},
		{&ForgetSnapshotParams{SnapshotID: "ABCDEF1234567890abcdef1234567890ABCDEF1234567890abcdef1234567890"}, ""},
		{&ForgetSnapshotParams{SnapshotID: "abcdef1"}, "invalid snapshot id"},
		{&ForgetSnapshotParams{SnapshotID: "--prune"}, "invalid snapshot id"},
		{&ForgetSnapshotParams{SnapshotID: "abcdef12 latest"}, "invalid snapshot id"},
		{&ForgetSnapshotParams{SnapshotID: "abcdef1234567890abcdef1234567890abcdef1234567890abcdef12345678901"}, "invalid snapshot id"},
		{&DBStudioExportParams{Format: "sql"}, ""},
		{&DBStudioExportParams{Format: "csv", Tables: []string{"a"}}, ""},
		{&DBStudioExportParams{Format: "csv", Tables: []string{"a", "b"}}, "a CSV export is of one table"},
		{&DBStudioExportParams{Format: "xml"}, "the format is sql or csv"},
		{&DBStudioDeleteParams{Dir: "imports"}, ""},
		{&DBStudioDeleteParams{Dir: "../etc"}, "the folder is exports or imports"},
		{&DBStudioAlterParams{Kind: "add_column", Column: col}, ""},
		{&DBStudioAlterParams{Kind: "add_index", Index: idx}, ""},
		{&DBStudioAlterParams{Kind: "add_index", Column: col}, "give add_column with a column, add_index with an index, or truncate or drop"},
		{&DBStudioAlterParams{Kind: "truncate", Table: "wp_log", Confirm: "wp_log"}, ""},
		{&DBStudioAlterParams{Kind: "drop", Table: "wp_log", Confirm: "WP_LOG"}, "type the table's name to confirm"},
		{&DBStudioAlterParams{Kind: "drop", Table: "wp_log"}, "type the table's name to confirm"},
		{&DBStudioOpenParams{User: "shop_reader", Password: "pw"}, ""},
		{&DBStudioOpenParams{User: "root", Password: "pw"}, "invalid credentials"},
		{&DBStudioOpenParams{User: "shop_reader"}, "invalid credentials"},
		{&DBStudioOpenParams{User: "shop_reader", Password: "a\nb"}, "invalid credentials"},
		{&DBStudioLoginParams{Login: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}, ""},
		{&DBStudioLoginParams{Login: "../x"}, "invalid Quarry session"},
		{&DBStudioImportSQLParams{Login: ""}, "invalid Quarry session"},
	}
	for _, c := range cases {
		err := c.v.Validate()
		got := ""
		if err != nil {
			got = err.Error()
		}
		if got != c.want {
			t.Errorf("%T %+v: got %q, want %q", c.v, c.v, got, c.want)
		}
	}
}

// SR-R1, Fase 9b: every reserved MariaDB name, the agent's own prefixes and
// another site's default name are refused as a site's database, account or
// granted database.
func TestDatabaseParamsRefuseReservedNames(t *testing.T) {
	s := state.Site{ID: 1}
	for _, name := range []string{"mysql", "root", "sys", "performance_schema", "information_schema",
		"debian_sys_maint", "mariadb.sys", "mysql.sys", "test", "site_01", "site_2", "SITE_1", "-x",
		"cgv_0123456789abcdef", "cgm_0123456789abcdef", "cgh_0123456789abcdef", "cgq_0123456789abcdef", ""} {
		if p := (SiteDatabaseParams{Site: s, Name: name}); p.Validate() == nil {
			t.Errorf("database %q passes", name)
		}
		if p := (DBUserParams{Site: s, User: name}); p.Validate() == nil {
			t.Errorf("user %q passes", name)
		}
		if p := (DBUserParams{Site: s, User: "site_1", Grants: []state.DBGrant{{Database: name, Level: "all"}}}); p.Validate() == nil {
			t.Errorf("a grant on %q passes", name)
		}
	}
}
