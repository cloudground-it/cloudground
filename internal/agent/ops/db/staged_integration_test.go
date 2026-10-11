//go:build hostintegration

// The staged replacement against a real MariaDB (see integration_test.go
// for how to run it).
package db

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
)

func TestIntegrationStagedReplacement(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root in a throwaway container")
	}
	socket := os.Getenv("CG_TEST_MYSQL_SOCKET")
	if socket == "" {
		t.Skip("CG_TEST_MYSQL_SOCKET not set")
	}
	work := t.TempDir()
	m := host.NewMariaDB(socket, work, host.OSExec{Timeout: time.Minute})
	defer m.Close()
	o := &Ops{MariaDB: m, Socket: socket, WorkDir: work, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	rows := func(q string, args ...any) string {
		t.Helper()
		r, err := m.QueryStrings(ctx, q, args...)
		must(err)
		return strings.Join(r, ",")
	}
	tables := func(db string) string {
		return rows("SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? ORDER BY TABLE_NAME", db)
	}
	staging := func() string {
		return rows("SELECT SCHEMA_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME LIKE 'cgm\\_%' OR SCHEMA_NAME LIKE 'cgh\\_%'")
	}
	reset := func() {
		must(m.Exec(ctx, "DROP DATABASE IF EXISTS site_9003"))
		must(m.Exec(ctx, "CREATE DATABASE site_9003 CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"))
		for _, s := range []string{
			"CREATE TABLE site_9003.wp_options (option_name VARCHAR(64), option_value TEXT)",
			"INSERT INTO site_9003.wp_options VALUES ('siteurl', 'https://live.example')",
			"CREATE TABLE site_9003.wp_posts (id INT)", "CREATE TABLE site_9003.wp_users (id INT)",
			"CREATE TABLE site_9003.plugin_log (id INT)",
		} {
			must(m.Exec(ctx, s))
		}
	}
	t.Cleanup(func() { m.Exec(ctx, "DROP DATABASE IF EXISTS site_9003") })
	dump := filepath.Join(t.TempDir(), "dump.sql")
	must(os.WriteFile(dump, []byte("CREATE TABLE `xyz_options` (option_name VARCHAR(64), option_value TEXT);\n"+
		"INSERT INTO `xyz_options` VALUES ('siteurl', 'https://migrated.example');\n"+
		"CREATE TABLE `xyz_posts` (id INT);\nCREATE TABLE `xyz_users` (id INT);\n"+
		"CREATE TABLE `odd``name` (id INT);\n"), 0o600))

	// Undo: everything as it was, no staging database left.
	reset()
	s, err := o.Stage(ctx, "site_9003", dump)
	must(err)
	if got := tables("site_9003"); got != "plugin_log,wp_options,wp_posts,wp_users" {
		t.Fatalf("staging touched the live database: %s", got)
	}
	if v, err := s.Value(ctx, "xyz_options", "option_value", "option_name", "siteurl"); err != nil || v != "https://migrated.example" {
		t.Fatalf("staged siteurl %q %v", v, err)
	}
	must(s.Swap(ctx))
	if got := tables("site_9003"); got != "odd`name,plugin_log,wp_options,wp_posts,wp_users,xyz_options,xyz_posts,xyz_users" {
		t.Fatalf("after the swap: %s", got)
	}
	must(s.Retire(ctx))
	if got := tables("site_9003"); got != "odd`name,xyz_options,xyz_posts,xyz_users" {
		t.Fatalf("after the retire: %s", got)
	}
	must(s.Unretire(ctx))
	must(s.Unswap(ctx))
	if got := tables("site_9003"); got != "plugin_log,wp_options,wp_posts,wp_users" {
		t.Fatalf("after the undo: %s", got)
	}
	if v := rows("SELECT option_value FROM site_9003.wp_options"); v != "https://live.example" {
		t.Fatalf("the original rows: %q", v)
	}
	if got := staging(); got != "" {
		t.Fatalf("staging databases left: %s", got)
	}

	// Commit: the dump and nothing else, no staging database left.
	s, err = o.Stage(ctx, "site_9003", dump)
	must(err)
	must(s.Swap(ctx))
	must(s.Retire(ctx))
	s.Commit(ctx)
	if got := tables("site_9003"); got != "odd`name,xyz_options,xyz_posts,xyz_users" {
		t.Fatalf("after the commit: %s", got)
	}
	if got := staging(); got != "" {
		t.Fatalf("staging databases left: %s", got)
	}

	// The same prefix: one statement exchanges the tables.
	reset()
	same := filepath.Join(t.TempDir(), "same.sql")
	must(os.WriteFile(same, []byte("CREATE TABLE `wp_options` (option_name VARCHAR(64), option_value TEXT);\n"+
		"INSERT INTO `wp_options` VALUES ('siteurl', 'https://migrated.example');\n"), 0o600))
	s, err = o.Stage(ctx, "site_9003", same)
	must(err)
	must(s.Swap(ctx))
	if v := rows("SELECT option_value FROM site_9003.wp_options"); v != "https://migrated.example" {
		t.Fatalf("after the swap the live options are %q", v)
	}
	must(s.Unswap(ctx))
	if v := rows("SELECT option_value FROM site_9003.wp_options"); v != "https://live.example" {
		t.Fatalf("after the undo the live options are %q", v)
	}
	if got := staging(); got != "" {
		t.Fatalf("staging databases left: %s", got)
	}
}
