package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// recover-admin as another user says to run it as root, before it touches
// the database: it used to fail opening the file, with the database's
// error and no word of root.
func TestRecoverAdminNeedsRoot(t *testing.T) {
	defer func(f func() int) { geteuid = f }(geteuid)
	geteuid = func() int { return 1000 }
	db := filepath.Join(t.TempDir(), "state.db")
	err := recoverAdmin([]string{"--state", db, "--email", "a@example.com"})
	if !errors.Is(err, errNotRoot) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Fatalf("the database was touched: %v", err)
	}
	// Help needs no root.
	if err := recoverAdmin([]string{"-h"}); err != nil {
		t.Fatalf("help: %v", err)
	}
}
