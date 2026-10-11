package db

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
)

// scopedUserRe and scratchDBRe match exactly what this package and the host
// adapter create, so the sweep never drops anything else.
var (
	scopedUserRe = regexp.MustCompile(`^cgq_[0-9a-f]{16}$`)
	scratchDBRe  = regexp.MustCompile(`^cgv_[0-9a-f]{16}$`)
	// Staged replacements (staged.go).
	stagingScratchRe = regexp.MustCompile(`^cgm_[0-9a-f]{16}$`)
	stagingHoldRe    = regexp.MustCompile(`^cgh_[0-9a-f]{16}$`)
)

// Sweep removes what an agent that died mid-operation left behind: scoped
// accounts, restore-test databases and credential files (SPEC §8). Run it
// before serving, when nothing can be in flight.
func (o *Ops) Sweep(ctx context.Context) error {
	var errs []error
	users, err := o.MariaDB.QueryStrings(ctx, "SELECT User FROM mysql.user WHERE User LIKE 'cgq\\_%' AND Host = 'localhost'")
	if err != nil {
		errs = append(errs, fmt.Errorf("list leftover scoped accounts: %w", err))
	}
	for _, u := range users {
		if scopedUserRe.MatchString(u) {
			if err := o.MariaDB.Exec(ctx, "DROP USER IF EXISTS ?@'localhost'", u); err != nil {
				errs = append(errs, fmt.Errorf("drop leftover account %s: %w", u, err))
			}
		}
	}
	dbs, err := o.MariaDB.QueryStrings(ctx, "SELECT SCHEMA_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME LIKE 'cgv\\_%'")
	if err != nil {
		errs = append(errs, fmt.Errorf("list leftover restore-test databases: %w", err))
	}
	for _, d := range dbs {
		if scratchDBRe.MatchString(d) {
			if err := o.MariaDB.Exec(ctx, "DROP DATABASE IF EXISTS `"+d+"`"); err != nil {
				errs = append(errs, fmt.Errorf("drop leftover database %s: %w", d, err))
			}
		}
	}
	// A staged replacement cut short: its scratch database is only a copy
	// of a dump, and goes. Its holding database may be the only copy of a
	// site's previous tables: it stays, and is named for the operator.
	staged, err := o.MariaDB.QueryStrings(ctx, "SELECT SCHEMA_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME LIKE 'cgm\\_%' OR SCHEMA_NAME LIKE 'cgh\\_%'")
	if err != nil {
		errs = append(errs, fmt.Errorf("list leftover staging databases: %w", err))
	}
	for _, d := range staged {
		switch {
		case stagingScratchRe.MatchString(d):
			if err := o.MariaDB.Exec(ctx, "DROP DATABASE IF EXISTS `"+d+"`"); err != nil {
				errs = append(errs, fmt.Errorf("drop leftover database %s: %w", d, err))
			}
		case stagingHoldRe.MatchString(d):
			o.log().Warn("a migration was cut short: the tables it replaced are kept in this database; check the site, then drop it", "database", d)
		}
	}
	// An import cut short left a site account with a second password.
	if err := o.MariaDB.Unborrow(ctx, o.tenantAccount(ctx)); err != nil {
		errs = append(errs, err)
	}
	files, _ := filepath.Glob(filepath.Join(o.WorkDir, ".dbq-*.cnf"))
	for _, f := range files {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// VerifyDump is the restore test of a dump: it imports the file at path
// into a throwaway cgv_ database through the same sandboxed import a restore
// uses, and returns how many tables it made. want is how many tables (and
// views) the database had when it was dumped: the import must make at least
// that many, so an empty database is a valid dump of an empty database and
// nothing else. want < 0 (a backup that did not record it) requires at
// least one. A WordPress dump must make an options table. The throwaway
// database is always dropped.
func (o *Ops) VerifyDump(ctx context.Context, path string, wordpress bool, want int) (int, error) {
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return 0, err
	}
	scratch := "cgv_" + hex.EncodeToString(suffix)
	if err := o.MariaDB.Exec(ctx, "CREATE DATABASE `"+scratch+"`"); err != nil {
		return 0, fmt.Errorf("create the restore-test database: %w", err)
	}
	defer func() {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
		defer cancel()
		if err := o.MariaDB.Exec(dctx, "DROP DATABASE IF EXISTS `"+scratch+"`"); err != nil {
			o.log().Warn("drop the restore-test database", "database", scratch, "err", err)
		}
	}()
	if err := o.ImportFile(ctx, scratch, path); err != nil {
		return 0, err
	}
	count := func(query string) (int, error) {
		got, err := o.MariaDB.QueryStrings(ctx, query, scratch)
		if err != nil {
			return 0, fmt.Errorf("count the restored tables: %w", err)
		}
		n := 0
		if len(got) == 1 {
			_, err = fmt.Sscan(got[0], &n)
		}
		if len(got) != 1 || err != nil {
			return 0, fmt.Errorf("count the restored tables: unexpected answer %q", got)
		}
		return n, nil
	}
	tables, err := count(countTablesQuery)
	if err != nil {
		return 0, err
	}
	switch {
	case want < 0 && tables == 0:
		return 0, agentrpc.Invalid("the dump imported no tables")
	case tables < want:
		return 0, agentrpc.Invalid("the dump imported %d tables; the database had %d", tables, want)
	}
	if wordpress {
		options, err := count(countOptionsQuery)
		if err != nil {
			return 0, err
		}
		if options == 0 {
			return 0, agentrpc.Invalid("the dump has no WordPress options table")
		}
	}
	return tables, nil
}

// The restore test's counts, the database bound.
const (
	countTablesQuery  = "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = ?"
	countOptionsQuery = "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME LIKE '%options'"
)
