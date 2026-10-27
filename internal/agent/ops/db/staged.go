package db

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
)

// Staged is a dump imported into a scratch database, put in place of a live
// database's tables by renames, so the live database never goes without the
// tables it is serving from (SPEC §5, staged replacement). Moves are
// recorded so each can be undone:
//
//  1. Stage: the dump goes into cgm_<16 hex> (scratch), the live database
//     untouched.
//  2. Swap: one RENAME TABLE moves every dump table into the live database;
//     a live table of the same name goes to cgh_<16 hex> (hold) in the same
//     statement. Live tables the dump does not have stay, so a site whose
//     prefix the dump does not share keeps running until it is switched.
//  3. Retire: one RENAME TABLE moves those remaining live tables to hold:
//     the live database is now exactly the dump.
//  4. Commit drops hold and scratch; Unretire and Unswap undo 3 and 2.
type Staged struct {
	o *Ops
	// m is the adapter of the server the live database is on, where the
	// scratch and holding databases are made too (SPEC §9).
	m       host.MariaDB
	server  int64
	live    string
	scratch string
	hold    string
	tables  []string // the dump's, in scratch until swapped
	held    []string // live tables the swap displaced into hold
	retired []string // live tables Retire moved into hold
	swapped bool
}

// stagePrefixes name the databases of a staged replacement.
const (
	stageScratch = "cgm_"
	stageHold    = "cgh_"
)

// Stage imports the dump at path into a new scratch database with the live
// database's character set and collation, and lists its tables. A dump that
// makes no table, or a view (a view cannot be renamed into another
// database), is invalid. The live database is not touched.
func (o *Ops) Stage(ctx context.Context, live, path string) (*Staged, error) {
	if !o.known(ctx, live) || scratchNameRe.MatchString(live) {
		return nil, agentrpc.Invalid("invalid database name")
	}
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return nil, err
	}
	h := hex.EncodeToString(suffix)
	server, err := o.serverOf(ctx, live)
	if err != nil {
		return nil, err
	}
	m, err := o.mariaOfServer(server)
	if err != nil {
		return nil, err
	}
	s := &Staged{o: o, m: m, server: server, live: live, scratch: stageScratch + h, hold: stageHold + h}
	cs, coll, err := o.charset(ctx, live)
	if err != nil {
		return nil, err
	}
	o.scratch.set(s.scratch, server)
	o.scratch.set(s.hold, server)
	if err := m.Exec(ctx, "CREATE DATABASE `"+s.scratch+"` CHARACTER SET "+cs+" COLLATE "+coll); err != nil {
		s.forget()
		return nil, fmt.Errorf("create the staging database: %w", err)
	}
	fail := func(err error) (*Staged, error) {
		s.drop(ctx, s.scratch)
		return nil, err
	}
	if err := o.ImportFile(ctx, s.scratch, path); err != nil {
		return fail(err)
	}
	views, err := m.QueryStrings(ctx, "SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_TYPE <> 'BASE TABLE'", s.scratch)
	if err != nil {
		return fail(fmt.Errorf("list the staged tables: %w", err))
	}
	if len(views) > 0 {
		return fail(agentrpc.Invalid("the dump creates views (%s), which a migration cannot move", strings.Join(views, ", ")))
	}
	if s.tables, err = o.baseTables(ctx, s.scratch); err != nil {
		return fail(err)
	}
	if len(s.tables) == 0 {
		return fail(agentrpc.Invalid("the dump imported no tables"))
	}
	return s, nil
}

// Tables are the dump's tables.
func (s *Staged) Tables() []string { return slices.Clone(s.tables) }

// Value reads one column of the staged table (scratch, before the swap)
// where key = value: a WordPress option, say. "" when there is no such row.
func (s *Staged) Value(ctx context.Context, table, column, key, value string) (string, error) {
	got, err := s.m.QueryStrings(ctx, "SELECT "+ident(column)+" FROM "+ident(s.scratch)+"."+ident(table)+" WHERE "+ident(key)+" = ? LIMIT 1", value)
	if err != nil || len(got) == 0 {
		return "", err
	}
	return got[0], nil
}

// Swap puts the dump's tables in the live database in one statement,
// moving the live tables they replace into hold.
func (s *Staged) Swap(ctx context.Context) error {
	if err := s.m.Exec(ctx, "CREATE DATABASE `"+s.hold+"`"); err != nil {
		return fmt.Errorf("create the holding database: %w", err)
	}
	live, err := s.o.baseTables(ctx, s.live)
	if err != nil {
		return err
	}
	var moves, held []string
	for _, t := range s.tables {
		if slices.Contains(live, t) {
			moves = append(moves, s.move(s.live, s.hold, t))
			held = append(held, t)
		}
		moves = append(moves, s.move(s.scratch, s.live, t))
	}
	if err := s.rename(ctx, moves); err != nil {
		return fmt.Errorf("swap the dump's tables in: %w", err)
	}
	s.held, s.swapped = held, true
	return nil
}

// Retire moves the live tables the dump does not have into hold.
func (s *Staged) Retire(ctx context.Context) error {
	live, err := s.o.baseTables(ctx, s.live)
	if err != nil {
		return err
	}
	var rest, moves []string
	for _, t := range live {
		if !slices.Contains(s.tables, t) {
			rest = append(rest, t)
			moves = append(moves, s.move(s.live, s.hold, t))
		}
	}
	if err := s.rename(ctx, moves); err != nil {
		return fmt.Errorf("retire the replaced tables: %w", err)
	}
	s.retired = rest
	return nil
}

// Unretire puts the tables Retire moved back in the live database.
func (s *Staged) Unretire(ctx context.Context) error {
	var moves []string
	for _, t := range s.retired {
		moves = append(moves, s.move(s.hold, s.live, t))
	}
	if err := s.rename(ctx, moves); err != nil {
		return fmt.Errorf("put the replaced tables back: %w", err)
	}
	s.retired = nil
	return nil
}

// Unswap undoes Swap in one statement (each dump table back to scratch,
// then the live table it displaced back in its place) and drops the staging
// databases. Unretire first: Unswap leaves hold alone unless it is empty.
func (s *Staged) Unswap(ctx context.Context) error {
	if s.swapped {
		var moves []string
		for _, t := range s.tables {
			moves = append(moves, s.move(s.live, s.scratch, t))
			if slices.Contains(s.held, t) {
				moves = append(moves, s.move(s.hold, s.live, t))
			}
		}
		if err := s.rename(ctx, moves); err != nil {
			return fmt.Errorf("put the original tables back: %w", err)
		}
		s.swapped, s.held = false, nil
	}
	s.drop(ctx, s.scratch)
	if len(s.retired) == 0 {
		s.drop(ctx, s.hold)
	}
	s.forget()
	return nil
}

// Commit drops what the live database no longer has, and the scratch
// database the dump came through.
func (s *Staged) Commit(ctx context.Context) {
	s.drop(ctx, s.hold)
	s.drop(ctx, s.scratch)
	s.forget()
}

// Hold names the database the replaced tables are in until Commit (for an
// error that has to say where they are).
func (s *Staged) Hold() string { return s.hold }

func (s *Staged) move(from, to, table string) string {
	return ident(from) + "." + ident(table) + " TO " + ident(to) + "." + ident(table)
}

func (s *Staged) rename(ctx context.Context, moves []string) error {
	if len(moves) == 0 {
		return nil
	}
	return s.m.Exec(ctx, "RENAME TABLE "+strings.Join(moves, ", "))
}

// drop drops a staging database on its own deadline: it runs on failures
// too, a cancelled request's included.
func (s *Staged) drop(ctx context.Context, database string) {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
	defer cancel()
	if err := s.m.Exec(dctx, "DROP DATABASE IF EXISTS `"+database+"`"); err != nil {
		s.o.log().Warn("drop a staging database", "database", database, "err", err)
	}
}

// forget lets the router forget the scratch databases once they are gone.
func (s *Staged) forget() {
	s.o.scratch.forget(s.scratch)
	s.o.scratch.forget(s.hold)
}

// baseTables lists a database's tables.
func (o *Ops) baseTables(ctx context.Context, database string) ([]string, error) {
	m, err := o.maria(ctx, database)
	if err != nil {
		return nil, err
	}
	got, err := m.QueryStrings(ctx, "SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_TYPE = 'BASE TABLE' ORDER BY TABLE_NAME", database)
	if err != nil {
		return nil, fmt.Errorf("list the tables of %s: %w", database, err)
	}
	return got, nil
}

// ident quotes a name for SQL: a dump's table names are the tenant's, so a
// backquote in one is doubled, never trusted.
func ident(name string) string { return "`" + strings.ReplaceAll(name, "`", "``") + "`" }
