package db

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// A database user signed in to Quarry with its own password (Fase 9b,
// SPEC §3.1): what the agent does as that user, never as root or a scoped
// account.

// SiteUser refuses an account the record does not give the site: only a
// site's own database users may sign in to Quarry (never root, whose
// unix_socket sign-in the agent, running as root, would pass without a
// password; never the agent's scoped accounts or the server's own).
func (o *Ops) SiteUser(ctx context.Context, s state.Site, user string) error {
	return o.ownedUser(ctx, s, user)
}

// ImportAsLogin imports relPath (a .sql file of the site's tree, never
// followed through a symlink) into database as login, through the
// sandboxed client: what the dump may change is what the server lets the
// user change. There is no rollback point: making one, and putting it back,
// would take privileges the user may not have. The file is read for
// statements that read or write a file first, as DBImport does (§5).
func (o *Ops) ImportAsLogin(ctx context.Context, s state.Site, login Login, database, relPath string) (rpc.DBImportResult, error) {
	if !host.ValidDatabaseName(database) {
		return rpc.DBImportResult{}, agentrpc.Invalid("invalid database name")
	}
	if !strings.HasSuffix(strings.ToLower(relPath), ".sql") {
		return rpc.DBImportResult{}, agentrpc.Invalid("the import file must use the .sql extension")
	}
	work, err := os.MkdirTemp(o.WorkDir, "db-import-")
	if err != nil {
		return rpc.DBImportResult{}, err
	}
	defer os.RemoveAll(work)
	r, err := o.Open(s)
	if err != nil {
		return rpc.DBImportResult{}, err
	}
	staged, size, err := stage(r, relPath, work)
	r.Close()
	if err != nil {
		return rpc.DBImportResult{}, err
	}
	warnings, err := fileStatementWarnings(staged)
	if err != nil {
		return rpc.DBImportResult{}, err
	}
	f, err := os.Open(staged)
	if err != nil {
		return rpc.DBImportResult{}, err
	}
	defer f.Close()
	err = o.MariaDB.Import(ctx, database, f, host.ImportAs{Login: &host.Credentials{User: login.User, Password: login.Password}})
	if err != nil {
		msg := "the import stopped at a statement the server refused; what ran before it stays"
		if len(warnings) > 0 {
			msg += ": " + strings.Join(warnings, "; ")
		}
		return rpc.DBImportResult{}, agentrpc.Wrap(agentrpc.CodeInvalid, msg, fmt.Errorf("import as %s: %w", login.User, err))
	}
	return rpc.DBImportResult{Imported: relPath, SizeBytes: size, Warnings: warnings}, nil
}

// DumpAsLogin streams mariadb-dump of database (or some of its tables) as
// login: the dump holds what the server lets the user read.
func (o *Ops) DumpAsLogin(ctx context.Context, login Login, database string, w io.Writer, tables ...string) error {
	return o.MariaDB.DumpAs(ctx, host.Credentials{User: login.User, Password: login.Password}, database, w, tables...)
}
