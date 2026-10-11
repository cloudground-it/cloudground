package db

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

func TestScanFileStatements(t *testing.T) {
	for _, tc := range []struct {
		sql  string
		want int
	}{
		{"INSERT INTO t VALUES (1);", 0},
		{"LOAD DATA LOCAL INFILE '/root/secret.txt' INTO TABLE loot;", 1},
		{"load  data\n concurrent local infile '/etc/hostname' into table t;", 1},
		{"LOAD XML LOCAL INFILE '/etc/passwd' INTO TABLE t;", 1},
		{"SELECT * FROM t INTO OUTFILE '/tmp/x';", 1},
		{"SELECT 1 INTO DUMPFILE \"/tmp/x\";", 1},
		{"LOAD DATA INFILE '/a' INTO TABLE t; SELECT 1 INTO OUTFILE '/b';", 2},
		// Words that only look alike.
		{"INSERT INTO posts VALUES ('download data infile-free');", 0},
	} {
		got, err := scanFileStatements(strings.NewReader(tc.sql))
		if err != nil || len(got) != tc.want {
			t.Errorf("%q: %v %v", tc.sql, got, err)
		}
	}
}

// A statement across the boundary of two chunks is still seen.
func TestScanFileStatementsAcrossChunks(t *testing.T) {
	for _, at := range []int{scanChunk - 10, scanChunk - 1, scanChunk, scanChunk + 5, 3*scanChunk - 7} {
		sql := strings.Repeat("x ", at/2) + "LOAD DATA LOCAL INFILE '/etc/hostname' INTO TABLE t;"
		got, err := scanFileStatements(strings.NewReader(sql))
		if err != nil || len(got) != 1 {
			t.Errorf("at %d: %v %v", at, got, err)
		}
	}
}

// SR-T1: an import of a file with LOAD DATA … INFILE warns before it runs,
// and when the server refuses the statement the person reads why.
func TestImportWarnsOfFileStatements(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	write := func(name, sql string) string {
		if err := os.WriteFile(filepath.Join(f.site.RootPath, "shared", name), []byte(sql), 0o644); err != nil {
			t.Fatal(err)
		}
		return "shared/" + name
	}
	res, err := f.o.Import(ctx, rpc.DBImportParams{Site: f.site, Database: "site_3",
		RelPath: write("ok.sql", "CREATE TABLE t (v TEXT);\n-- LOAD DATA LOCAL INFILE '/etc/hostname' INTO TABLE t;\n")})
	if err != nil || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "LOAD DATA") {
		t.Fatalf("warnings %v, err %v", res.Warnings, err)
	}
	f.m.OnImport = func(string, string) error { return errors.New("ERROR 4166 (HY000) at line 2") }
	_, err = f.o.Import(ctx, rpc.DBImportParams{Site: f.site, Database: "site_3",
		RelPath: write("bad.sql", "LOAD DATA LOCAL INFILE '/root/secret.txt' INTO TABLE t;\n")})
	var e *agentrpc.Error
	if !errors.As(err, &e) || e.Code != agentrpc.CodeInvalid || !strings.Contains(e.Message, "LOAD DATA") {
		t.Fatalf("the failure does not say why: %v", err)
	}
}
