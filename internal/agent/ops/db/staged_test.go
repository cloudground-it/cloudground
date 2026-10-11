package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
)

// sim is a MariaDB server reduced to databases of table names, behind
// FakeMariaDB's hooks: CREATE/DROP DATABASE, RENAME TABLE (all moves or none,
// left to right, a target that exists refused), the information_schema
// lists, and an import that creates the dump's CREATE TABLE/VIEW names.
type sim struct {
	dbs     map[string][]string
	views   map[string][]string
	renames int
}

var simCreate = regexp.MustCompile("CREATE (TABLE|VIEW) `((?:[^`]|``)+)`")

// unquote reads one `name` from s, returning it and the rest.
func unquote(s string) (string, string) {
	s = strings.TrimPrefix(s, "`")
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '`' {
			if i+1 < len(s) && s[i+1] == '`' {
				b.WriteByte('`')
				i++
				continue
			}
			return b.String(), s[i+1:]
		}
		b.WriteByte(s[i])
	}
	return b.String(), ""
}

func (m *sim) exec(q string, _ []any) error {
	switch {
	case strings.HasPrefix(q, "CREATE DATABASE "):
		name, _ := unquote(strings.TrimPrefix(q, "CREATE DATABASE "))
		if _, ok := m.dbs[name]; ok {
			return fmt.Errorf("database %s exists", name)
		}
		m.dbs[name] = nil
	case strings.HasPrefix(q, "DROP DATABASE IF EXISTS "):
		name, _ := unquote(strings.TrimPrefix(q, "DROP DATABASE IF EXISTS "))
		delete(m.dbs, name)
	case strings.HasPrefix(q, "RENAME TABLE "):
		next := map[string][]string{}
		for k, v := range m.dbs {
			next[k] = slices.Clone(v)
		}
		rest := strings.TrimPrefix(q, "RENAME TABLE ")
		for rest != "" {
			fd, r := unquote(rest)
			ft, r := unquote(strings.TrimPrefix(r, "."))
			td, r := unquote(strings.TrimPrefix(r, " TO "))
			tt, r := unquote(strings.TrimPrefix(r, "."))
			rest = strings.TrimPrefix(r, ", ")
			i := slices.Index(next[fd], ft)
			if _, ok := next[td]; i < 0 || !ok || slices.Contains(next[td], tt) {
				return fmt.Errorf("rename %s.%s to %s.%s refused", fd, ft, td, tt)
			}
			next[fd] = slices.Delete(next[fd], i, i+1)
			next[td] = append(next[td], tt)
		}
		m.dbs = next
		m.renames++
	}
	return nil
}

func (m *sim) query(q string, args []any) ([]string, error) {
	db, _ := args[0].(string)
	switch {
	case strings.Contains(q, "SCHEMATA"):
		return []string{"utf8mb4 utf8mb4_unicode_ci"}, nil
	case strings.Contains(q, "TABLE_TYPE <> 'BASE TABLE'"):
		return m.views[db], nil
	case strings.Contains(q, "TABLE_TYPE = 'BASE TABLE'"):
		t := slices.Clone(m.dbs[db])
		sort.Strings(t)
		return t, nil
	}
	return nil, nil
}

func (m *sim) imp(database, sql string) error {
	if _, ok := m.dbs[database]; !ok {
		return errors.New("no database " + database)
	}
	for _, c := range simCreate.FindAllStringSubmatch(sql, -1) {
		name := strings.ReplaceAll(c[2], "``", "`")
		if c[1] == "VIEW" {
			m.views[database] = append(m.views[database], name)
		} else {
			m.dbs[database] = append(m.dbs[database], name)
		}
	}
	return nil
}

func newSim(t *testing.T, live ...string) (*fixture, *sim) {
	f := newFixture(t)
	m := &sim{dbs: map[string][]string{"site_3": live}, views: map[string][]string{}}
	f.m.OnExec, f.m.OnQuery, f.m.OnImport = m.exec, m.query, m.imp
	return f, m
}

func dumpFile(t *testing.T, tables ...string) string {
	var b strings.Builder
	for _, tb := range tables {
		fmt.Fprintf(&b, "CREATE TABLE `%s` (id int);\n", strings.ReplaceAll(tb, "`", "``"))
	}
	p := filepath.Join(t.TempDir(), "dump.sql")
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func (m *sim) live() string {
	t := slices.Clone(m.dbs["site_3"])
	sort.Strings(t)
	return strings.Join(t, ",")
}

// A dump with another prefix: the swap adds its tables beside the live
// ones (the site keeps serving until its prefix is switched), the retire
// leaves exactly the dump; Commit drops the old tables and both staging
// databases. Every step is one statement.
func TestStagedReplacementWithANewPrefix(t *testing.T) {
	f, m := newSim(t, "wp_options", "wp_posts", "wp_users", "plugin_log")
	s, err := f.o.Stage(context.Background(), "site_3", dumpFile(t, "xyz_options", "xyz_posts", "xyz_users"))
	if err != nil {
		t.Fatal(err)
	}
	if m.live() != "plugin_log,wp_options,wp_posts,wp_users" {
		t.Fatalf("staging touched the live database: %s", m.live())
	}
	if err := s.Swap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.live() != "plugin_log,wp_options,wp_posts,wp_users,xyz_options,xyz_posts,xyz_users" {
		t.Fatalf("after the swap: %s", m.live())
	}
	if err := s.Retire(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.live() != "xyz_options,xyz_posts,xyz_users" || m.renames != 2 {
		t.Fatalf("after the retire: %s (%d renames)", m.live(), m.renames)
	}
	s.Commit(context.Background())
	if len(m.dbs) != 1 {
		t.Fatalf("databases left: %v", m.dbs)
	}
}

// The same prefix: one statement exchanges every table, so the live
// database is never without one of them; Unretire then Unswap restore the
// original exactly and drop the staging databases.
func TestStagedReplacementSamePrefixAndUndo(t *testing.T) {
	f, m := newSim(t, "wp_options", "wp_posts", "wp_users", "plugin_log")
	s, err := f.o.Stage(context.Background(), "site_3", dumpFile(t, "wp_options", "wp_posts", "wp_users", "wp_termmeta"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Swap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.live() != "plugin_log,wp_options,wp_posts,wp_termmeta,wp_users" || m.renames != 1 {
		t.Fatalf("after the swap: %s (%d)", m.live(), m.renames)
	}
	if got := m.dbs[s.Hold()]; strings.Join(got, ",") != "wp_options,wp_posts,wp_users" {
		t.Fatalf("hold: %v", got)
	}
	if err := s.Retire(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Unretire(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Unswap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.live() != "plugin_log,wp_options,wp_posts,wp_users" || len(m.dbs) != 1 {
		t.Fatalf("after the undo: %s, databases %v", m.live(), m.dbs)
	}
	if !strings.Contains(strings.Join(f.m.Statements(), "\n"), "CREATE DATABASE `cgm_") {
		t.Fatal("no scratch database")
	}
}

// What a staged dump may not be; and a table name with a backquote is
// quoted, not spliced into the statement.
func TestStageRefuses(t *testing.T) {
	f, m := newSim(t, "wp_options")
	p := filepath.Join(t.TempDir(), "v.sql")
	os.WriteFile(p, []byte("CREATE TABLE `t` (id int);\nCREATE VIEW `v` AS SELECT 1;\n"), 0o600)
	if _, err := f.o.Stage(context.Background(), "site_3", p); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
		t.Fatalf("a dump with a view: %v", err)
	}
	if _, err := f.o.Stage(context.Background(), "site_3", dumpFile(t)); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
		t.Fatalf("an empty dump: %v", err)
	}
	if len(m.dbs) != 1 {
		t.Fatalf("a refused stage left databases: %v", m.dbs)
	}
	s, err := f.o.Stage(context.Background(), "site_3", dumpFile(t, "x` TO `mysql`.`user"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Swap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(m.dbs["site_3"], "x` TO `mysql`.`user") {
		t.Fatalf("the hostile name was not moved as one name: %v", m.dbs)
	}
	if _, err := f.o.Stage(context.Background(), "../x", dumpFile(t, "a")); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
		t.Fatal("an invalid live name was staged")
	}
}

// The sweep drops a staging scratch database and keeps a holding one, the
// only copy of a site's previous tables after a crash.
func TestSweepStagingDatabases(t *testing.T) {
	f := newFixture(t)
	f.m.OnQuery = func(q string, _ []any) ([]string, error) {
		if strings.Contains(q, "cgm") {
			return []string{"cgm_0123456789abcdef", "cgh_0123456789abcdef", "cgm_short"}, nil
		}
		return nil, nil
	}
	if err := f.o.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := strings.Join(f.m.Statements(), "\n"); st != "DROP DATABASE IF EXISTS `cgm_0123456789abcdef`\nUNBORROW" {
		t.Fatalf("sweep:\n%s", st)
	}
}
