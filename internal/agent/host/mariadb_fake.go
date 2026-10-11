package host

import (
	"context"
	"fmt"
	"io"
	"sync"
)

// FakeMariaDB records statements and keeps dumps and imports in memory.
type FakeMariaDB struct {
	mu         sync.Mutex
	statements []string
	open       map[string]string // scoped user -> database
	n          int
	// Dumps is what Dump writes for a database.
	Dumps map[string]string
	// Imported is what Import received, per database.
	Imported map[string]string
	// ImportedAs is "<database> <definer>" for every import, in order.
	ImportedAs []string
	// DumpedAs is "<user> <database>" for every DumpAs, in order.
	DumpedAs []string
	// Rows answers QueryStrings by query text.
	Rows map[string][]string
	// Err, when set, is returned by every call.
	Err error
	// OnExec, OnQuery, OnDump and OnImport, when set, script one kind of
	// call (OnExec and OnQuery run with the fake locked and must not call
	// it): OnExec sees
	// each statement before it is recorded and may refuse it; OnQuery
	// answers QueryStrings in place of Rows; OnDump
	// writes the dump in place of Dumps; OnImport sees what an import
	// received and may fail it (it is recorded in Imported only on success).
	OnExec   func(query string, args []any) error
	OnQuery  func(query string, args []any) ([]string, error)
	OnDump   func(database string, tables []string, w io.Writer) error
	OnImport func(database, sql string) error
}

// NewFakeMariaDB returns an empty fake.
func NewFakeMariaDB() *FakeMariaDB {
	return &FakeMariaDB{open: map[string]string{}, Dumps: map[string]string{}, Imported: map[string]string{}, Rows: map[string][]string{}}
}

// Statements returns every Exec'd statement, with its args appended.
func (f *FakeMariaDB) Statements() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.statements...)
}

// OpenAccounts is the number of scoped accounts not yet closed.
func (f *FakeMariaDB) OpenAccounts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.open)
}

func (f *FakeMariaDB) Exec(ctx context.Context, query string, args ...any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	if f.OnExec != nil {
		if err := f.OnExec(query, args); err != nil {
			return err
		}
	}
	if len(args) > 0 {
		query += fmt.Sprint(" ", args)
	}
	f.statements = append(f.statements, query)
	return nil
}

func (f *FakeMariaDB) QueryStrings(ctx context.Context, query string, args ...any) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	if f.OnQuery != nil {
		return f.OnQuery(query, args)
	}
	return append([]string(nil), f.Rows[query]...), nil
}

func (f *FakeMariaDB) ScopedAccount(ctx context.Context, database string, readOnly bool) (*ScopedAccount, error) {
	if !ValidDatabaseName(database) {
		return nil, ErrInvalidDatabase
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	f.n++
	user := fmt.Sprintf("cgq_%016x", f.n)
	f.open[user] = database
	return &ScopedAccount{User: user, Password: "fake-password", DefaultsFile: "/fake/" + user + ".cnf", cleanup: func() error {
		f.mu.Lock()
		defer f.mu.Unlock()
		delete(f.open, user)
		return nil
	}}, nil
}

func (f *FakeMariaDB) Dump(ctx context.Context, database string, w io.Writer, tables ...string) error {
	if !ValidDatabaseName(database) {
		return ErrInvalidDatabase
	}
	for _, t := range tables {
		if !ValidTableName(t) {
			return fmt.Errorf("invalid table name %q", t)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	dump, err, hook := f.Dumps[database], f.Err, f.OnDump
	f.mu.Unlock()
	if err != nil {
		return err
	}
	if hook != nil {
		return hook(database, tables, w)
	}
	_, err = io.WriteString(w, dump)
	return err
}

// DumpAs is Dump, recording who signed in.
func (f *FakeMariaDB) DumpAs(ctx context.Context, as Credentials, database string, w io.Writer, tables ...string) error {
	if err := as.valid(); err != nil {
		return err
	}
	f.mu.Lock()
	f.DumpedAs = append(f.DumpedAs, as.User+" "+database)
	f.mu.Unlock()
	return f.Dump(ctx, database, w, tables...)
}

// Unborrow is recorded as a statement.
func (f *FakeMariaDB) Unborrow(ctx context.Context, tenant func(string) bool) error {
	return f.Exec(ctx, "UNBORROW")
}

func (f *FakeMariaDB) Import(ctx context.Context, database string, r io.Reader, as ImportAs) error {
	if !ValidDatabaseName(database) {
		return ErrInvalidDatabase
	}
	f.mu.Lock()
	f.ImportedAs = append(f.ImportedAs, database+" "+as.definer())
	f.mu.Unlock()
	acct, err := f.ScopedAccount(ctx, database, false)
	if err != nil {
		return err
	}
	defer acct.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.mu.Lock()
	hook := f.OnImport
	f.mu.Unlock()
	if hook != nil {
		if err := hook(database, string(b)); err != nil {
			return err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Imported[database] += string(b)
	return nil
}

var _ MariaDB = (*FakeMariaDB)(nil)
