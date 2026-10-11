package state

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The schema the migrations build is history: installed panels ran them,
// so what each version leaves behind never changes. These tests pin it.
//
// testdata/schema.golden is the database after the migrations alone, on an
// empty file or on one stopped at any earlier version; testdata/schema_open.golden
// is the same after a full Open with a key (which drops migration 16's
// holding table once its rows are sealed). Refresh both with
// CG_GOLDEN_UPDATE=1 only for a new migration, never to absorb a change to
// an old one.

const (
	goldenMigrated = "testdata/schema.golden"
	goldenOpened   = "testdata/schema_open.golden"
)

func TestSchemaGoldenFromEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := openAtVersion(path, latestSchema)
	if err != nil {
		t.Fatal(err)
	}
	got := dumpSchema(t, s.db)
	s.Close()
	compareGolden(t, goldenMigrated, got)

	opened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	compareGolden(t, goldenOpened, dumpSchema(t, opened.db))
}

// A panel can be upgraded from any release: stopping at each earlier version
// and running the rest must reach the same schema as running them all at once.
func TestSchemaGoldenFromEveryVersion(t *testing.T) {
	migrated := readGolden(t, goldenMigrated)
	opened := readGolden(t, goldenOpened)
	for from := 0; from < latestSchema; from++ {
		t.Run(fmt.Sprintf("from %d", from), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			old, err := openAtVersion(path, from)
			if err != nil {
				t.Fatal(err)
			}
			old.Close()

			mid, err := openAtVersion(path, latestSchema)
			if err != nil {
				t.Fatal(err)
			}
			got := dumpSchema(t, mid.db)
			mid.Close()
			if got != migrated {
				t.Errorf("migrations from %d differ from the golden:\n%s", from, firstDifference(migrated, got))
			}

			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if got := dumpSchema(t, s.db); got != opened {
				t.Errorf("Open from %d differs from the golden:\n%s", from, firstDifference(opened, got))
			}
		})
	}
}

func readGolden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("%v (generate it with CG_GOLDEN_UPDATE=1)", err)
	}
	return string(b)
}

func compareGolden(t *testing.T, name, got string) {
	t.Helper()
	if os.Getenv("CG_GOLDEN_UPDATE") != "" {
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if want := readGolden(t, name); got != want {
		t.Errorf("%s differs:\n%s", name, firstDifference(want, got))
	}
}

// firstDifference shows the first line where two dumps part.
func firstDifference(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(w) || i < len(g); i++ {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl != gl {
			return fmt.Sprintf("line %d\n  want: %s\n  got:  %s", i+1, wl, gl)
		}
	}
	return "(no line differs)"
}

// dumpSchema renders everything SQLite knows about the schema: each object
// of sqlite_master with its normalised SQL, then each table's columns,
// foreign keys and indexes with their columns.
func dumpSchema(t *testing.T, db *sql.DB) string {
	t.Helper()
	var b strings.Builder
	rows := query(t, db, `SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_master ORDER BY type, name`)
	var tables []string
	for _, r := range rows {
		fmt.Fprintf(&b, "%s %s on %s\n  %s\n", r[0], r[1], r[2], normaliseSQL(r[3]))
		if r[0] == "table" {
			tables = append(tables, r[1])
		}
	}
	for _, table := range tables {
		fmt.Fprintf(&b, "\ncolumns of %s\n", table)
		for _, c := range query(t, db, `SELECT cid, name, type, "notnull", COALESCE(dflt_value, '-'), pk FROM pragma_table_info(?) ORDER BY cid`, table) {
			fmt.Fprintf(&b, "  %s\n", strings.Join(c, " | "))
		}
		for _, fk := range query(t, db, `SELECT id, seq, "table", "from", COALESCE("to", '-'), on_update, on_delete, match FROM pragma_foreign_key_list(?) ORDER BY id, seq`, table) {
			fmt.Fprintf(&b, "  fk %s\n", strings.Join(fk, " | "))
		}
		for _, ix := range query(t, db, `SELECT name, "unique", origin, partial FROM pragma_index_list(?) ORDER BY name`, table) {
			var cols []string
			for _, c := range query(t, db, `SELECT seqno, cid, COALESCE(name, '-') FROM pragma_index_info(?) ORDER BY seqno`, ix[0]) {
				cols = append(cols, strings.Join(c, ":"))
			}
			fmt.Fprintf(&b, "  index %s unique=%s origin=%s partial=%s on %s\n", ix[0], ix[1], ix[2], ix[3], strings.Join(cols, ", "))
		}
	}
	return b.String()
}

// query reads every row of q as strings.
func query(t *testing.T, db *sql.DB, q string, args ...any) [][]string {
	t.Helper()
	rows, err := db.Query(q, args...)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out [][]string
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		row := make([]string, len(cols))
		for i, v := range vals {
			row[i] = v.String
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// normaliseSQL makes two spellings of one statement compare equal: comments
// go, keywords and identifiers are lower-cased, runs of white space become
// one space and none is kept beside punctuation. Quoted literals are kept
// exactly, case included.
func normaliseSQL(q string) string {
	var out []rune
	space := false
	in := []rune(q)
	punct := func(r rune) bool { return strings.ContainsRune("(),=<>|;", r) }
	for i := 0; i < len(in); i++ {
		r := in[i]
		switch {
		case r == '\'':
			if space && len(out) > 0 && !punct(out[len(out)-1]) {
				out = append(out, ' ')
			}
			space = false
			out = append(out, r)
			for i++; i < len(in); i++ {
				out = append(out, in[i])
				if in[i] == '\'' {
					if i+1 < len(in) && in[i+1] == '\'' {
						i++
						out = append(out, in[i])
						continue
					}
					break
				}
			}
		case r == '-' && i+1 < len(in) && in[i+1] == '-':
			for i < len(in) && in[i] != '\n' {
				i++
			}
			space = true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			space = true
		default:
			if space && len(out) > 0 && !punct(out[len(out)-1]) && !punct(r) {
				out = append(out, ' ')
			}
			space = false
			out = append(out, []rune(strings.ToLower(string(r)))...)
		}
	}
	return string(out)
}
