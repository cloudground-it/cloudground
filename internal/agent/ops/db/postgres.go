package db

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/site"
)

// PostgreSQL servers (SPEC §9.1, #81). The same record and routing as the
// MariaDB servers of §9; the SQL differs.

// ownerRole is the NOLOGIN role that owns database db: an account with all
// privileges on it is a member, so what any of them creates is the owner's.
func ownerRole(db string) string { return "cgo_" + db }

const pgUnsupported = "not available for PostgreSQL databases yet"

// pgOfServer is the adapter of server id when it is a PostgreSQL one.
func (o *Ops) pgOfServer(id int64) (*host.PostgresAdmin, bool, error) {
	if id == 0 || o.Servers == nil {
		return nil, false, nil
	}
	kind, err := o.Servers.Kind(id)
	if err != nil || kind != rpc.KindPostgres {
		return nil, false, err
	}
	pg, err := o.Servers.Postgres(id)
	return pg, err == nil, err
}

// pgOfDatabase is the PostgreSQL adapter of the server database is on.
func (o *Ops) pgOfDatabase(ctx context.Context, database string) (*host.PostgresAdmin, bool, error) {
	id, err := o.serverOf(ctx, database)
	if err != nil {
		return nil, false, err
	}
	return o.pgOfServer(id)
}

// pgOfUser is the PostgreSQL adapter of the server an account is on.
func (o *Ops) pgOfUser(ctx context.Context, user string) (*host.PostgresAdmin, bool, error) {
	if o.Servers == nil {
		return nil, false, nil
	}
	id, err := o.registry().UserServer(ctx, user)
	if err != nil {
		return nil, false, err
	}
	return o.pgOfServer(id)
}

// pgIgnore reports whether err is one of the SQLSTATEs that mean "there was
// nothing to undo": a missing database (3D000), role (42704) or object (42P01).
func pgIgnore(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && (pe.Code == "3D000" || pe.Code == "42704" || pe.Code == "42P01" || pe.Code == "42P06")
}

// pgDropDatabase drops database, ending the sessions on it. An autovacuum
// worker on it cannot be ended by an administrator that is not a superuser
// (42501), but it gives way to the lock the drop asks for within a moment, so
// that one error is tried again a few times.
func pgDropDatabase(ctx context.Context, pg *host.PostgresAdmin, database string) error {
	var err error
	for try := 0; try < 10; try++ {
		err = pg.Exec(ctx, "", "DROP DATABASE IF EXISTS "+host.QuotePGIdent(database)+" WITH (FORCE)")
		var pe *pgconn.PgError
		if err == nil || !errors.As(err, &pe) || pe.Code != "42501" {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(time.Second):
		}
	}
	return err
}

func pgExecAll(ctx context.Context, pg *host.PostgresAdmin, database string, stmts ...string) error {
	for _, s := range stmts {
		if err := pg.Exec(ctx, database, s); err != nil {
			return err
		}
	}
	return nil
}

// pgCreateDatabase makes database with its owner role, closed to everyone
// else.
func pgCreateDatabase(ctx context.Context, pg *host.PostgresAdmin, db, adminUser string) error {
	role, q := ownerRole(db), host.QuotePGIdent
	have, err := pg.QueryStrings(ctx, "", "SELECT rolname FROM pg_roles WHERE rolname = "+host.QuotePGLiteral(role))
	if err != nil {
		return err
	}
	if len(have) == 0 {
		if err := pg.Exec(ctx, "", "CREATE ROLE "+q(role)+" NOLOGIN"); err != nil {
			return err
		}
	}
	exists, err := pg.QueryStrings(ctx, "", "SELECT datname FROM pg_database WHERE datname = "+host.QuotePGLiteral(db))
	if err != nil {
		return err
	}
	if err := pg.Exec(ctx, "", "GRANT "+q(role)+" TO "+q(adminUser)); err != nil {
		return err
	}
	if len(exists) == 0 {
		if err := pg.Exec(ctx, "", "CREATE DATABASE "+q(db)+" OWNER "+q(role)); err != nil {
			return err
		}
	}
	if err := pg.Exec(ctx, "", "REVOKE ALL ON DATABASE "+q(db)+" FROM PUBLIC"); err != nil {
		return err
	}
	return pgExecAll(ctx, pg, db, "REVOKE ALL ON SCHEMA public FROM PUBLIC", "ALTER SCHEMA public OWNER TO "+q(role))
}

// createDBPG is CreateDB on a PostgreSQL server.
func (o *Ops) createDBPG(ctx context.Context, p rpc.SiteDatabaseParams, pg *host.PostgresAdmin) (map[string]string, error) {
	reg := o.registry()
	owner, err := reg.DatabaseOwner(ctx, p.Name)
	if err != nil {
		return nil, err
	}
	switch {
	case owner != 0 && owner != p.Site.ID:
		return nil, agentrpc.Conflict("a database called %s already exists on this server", p.Name)
	case owner != 0:
		if have, err := reg.DatabaseServer(ctx, p.Name); err != nil || have != p.ServerID {
			if err != nil {
				return nil, err
			}
			return nil, agentrpc.Conflict("%s is on another database server", p.Name)
		}
	default:
		got, err := pg.QueryStrings(ctx, "", "SELECT datname FROM pg_database WHERE datname = "+host.QuotePGLiteral(p.Name))
		if err != nil {
			return nil, fmt.Errorf("look for the database %s: %w", p.Name, err)
		}
		if len(got) > 0 {
			return nil, agentrpc.Conflict("a database called %s already exists on this server", p.Name)
		}
	}
	if err := reg.update(ctx, func(f *registryFile) error {
		f.Databases[p.Name] = p.Site.ID
		f.Servers[p.Name] = p.ServerID
		return nil
	}); err != nil {
		return nil, err
	}
	if err := pgCreateDatabase(ctx, pg, p.Name, o.pgAdmin(p.ServerID)); err != nil {
		if owner == 0 {
			o.forgetDatabase(ctx, p.Name)
		}
		return nil, fmt.Errorf("create the database %s: %w", p.Name, err)
	}
	return map[string]string{"database": p.Name}, nil
}

// pgAdmin is the administrator's name on a PostgreSQL server (the role made a
// member of each owner role).
func (o *Ops) pgAdmin(id int64) string { return o.Servers.AdminUser(id) }

// dropDBPG is the part of DropDB on a PostgreSQL server: the accounts lose
// their grants, then the database and its owner role go.
func (o *Ops) dropDBPG(ctx context.Context, pg *host.PostgresAdmin, name string) error {
	rec, err := o.registry().SiteOfDatabase(ctx, name)
	if err != nil {
		return err
	}
	var errs []error
	for user, grants := range rec {
		if level, ok := grants[name]; ok {
			errs = append(errs, o.revokePG(ctx, pg, user, name, level))
		}
	}
	errs = append(errs, pgDropDatabase(ctx, pg, name))
	errs = append(errs, pg.Exec(ctx, "", "DROP ROLE IF EXISTS "+host.QuotePGIdent(ownerRole(name))))
	return errors.Join(errs...)
}

// grantPG gives user level on database.
func (o *Ops) grantPG(ctx context.Context, pg *host.PostgresAdmin, user, database, level string) error {
	u, d, role := host.QuotePGIdent(user), host.QuotePGIdent(database), host.QuotePGIdent(ownerRole(database))
	if level == site.GrantAll {
		if err := pgExecAll(ctx, pg, "", "GRANT "+role+" TO "+u,
			"ALTER ROLE "+u+" IN DATABASE "+d+" SET role = "+host.QuotePGLiteral(ownerRole(database))); err != nil {
			return err
		}
		return nil
	}
	if err := pg.Exec(ctx, "", "GRANT CONNECT ON DATABASE "+d+" TO "+u); err != nil {
		return err
	}
	return pgExecAll(ctx, pg, database,
		"GRANT USAGE ON SCHEMA public TO "+u,
		"GRANT SELECT ON ALL TABLES IN SCHEMA public TO "+u,
		"GRANT SELECT ON ALL SEQUENCES IN SCHEMA public TO "+u,
		"ALTER DEFAULT PRIVILEGES FOR ROLE "+role+" IN SCHEMA public GRANT SELECT ON TABLES TO "+u,
		"ALTER DEFAULT PRIVILEGES FOR ROLE "+role+" IN SCHEMA public GRANT SELECT ON SEQUENCES TO "+u)
}

// revokePG takes user's privileges on database away, whichever level it had;
// a database already gone is nothing to undo.
func (o *Ops) revokePG(ctx context.Context, pg *host.PostgresAdmin, user, database, level string) error {
	u, d, role := host.QuotePGIdent(user), host.QuotePGIdent(database), host.QuotePGIdent(ownerRole(database))
	var errs []error
	if level == site.GrantAll {
		errs = append(errs, pg.Exec(ctx, "", "REVOKE "+role+" FROM "+u))
		errs = append(errs, pg.Exec(ctx, "", "ALTER ROLE "+u+" IN DATABASE "+d+" RESET role"))
	} else {
		errs = append(errs, pgExecAll(ctx, pg, database,
			"ALTER DEFAULT PRIVILEGES FOR ROLE "+role+" IN SCHEMA public REVOKE SELECT ON TABLES FROM "+u,
			"ALTER DEFAULT PRIVILEGES FOR ROLE "+role+" IN SCHEMA public REVOKE SELECT ON SEQUENCES FROM "+u,
			"REVOKE ALL ON ALL TABLES IN SCHEMA public FROM "+u,
			"REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM "+u,
			"REVOKE USAGE ON SCHEMA public FROM "+u))
		errs = append(errs, pg.Exec(ctx, "", "REVOKE CONNECT ON DATABASE "+d+" FROM "+u))
	}
	var real []error
	for _, e := range errs {
		if e != nil && !pgIgnore(e) {
			real = append(real, e)
		}
	}
	if err := errors.Join(real...); err != nil {
		return fmt.Errorf("revoke %s's privileges on %s: %w", user, database, err)
	}
	return nil
}

// applyGrantsPG takes user from the grants it has to want and records what it
// reached, also when a step fails.
func (o *Ops) applyGrantsPG(ctx context.Context, pg *host.PostgresAdmin, user string, have, want map[string]string) error {
	got := map[string]string{}
	for d, l := range have {
		got[d] = l
	}
	var errs []error
	for _, d := range sortedKeys(have) {
		if want[d] == have[d] {
			continue
		}
		if err := o.revokePG(ctx, pg, user, d, have[d]); err != nil {
			errs = append(errs, err)
			continue
		}
		delete(got, d)
	}
	for _, d := range sortedKeys(want) {
		if got[d] == want[d] {
			continue
		}
		if err := o.grantPG(ctx, pg, user, d, want[d]); err != nil {
			errs = append(errs, fmt.Errorf("grant %s %s on %s: %w", user, want[d], d, err))
			continue
		}
		got[d] = want[d]
	}
	if err := o.registry().update(ctx, func(f *registryFile) error {
		u, ok := f.Users[user]
		if !ok {
			return nil
		}
		u.Grants = got
		f.Users[user] = u
		return nil
	}); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// createUserPG is CreateUser on a PostgreSQL server.
func (o *Ops) createUserPG(ctx context.Context, p rpc.DBUserParams, pg *host.PostgresAdmin) (map[string]string, error) {
	reg := o.registry()
	owner, err := reg.UserOwner(ctx, p.User)
	if err != nil {
		return nil, err
	}
	switch {
	case owner != 0 && owner != p.Site.ID:
		return nil, agentrpc.Conflict("a database user called %s already exists on this server", p.User)
	case owner != 0:
		if have, err := reg.UserServer(ctx, p.User); err != nil || have != p.ServerID {
			if err != nil {
				return nil, err
			}
			return nil, agentrpc.Conflict("%s is on another database server", p.User)
		}
	default:
		got, err := pg.QueryStrings(ctx, "", "SELECT rolname FROM pg_roles WHERE rolname = "+host.QuotePGLiteral(p.User))
		if err != nil {
			return nil, fmt.Errorf("look for the account %s: %w", p.User, err)
		}
		if len(got) > 0 {
			return nil, agentrpc.Conflict("a database user called %s already exists on this server", p.User)
		}
	}
	want, err := o.wantGrants(ctx, p.Site, p.Grants)
	if err != nil {
		return nil, err
	}
	if err := o.sameServer(ctx, p.ServerID, want); err != nil {
		return nil, err
	}
	var have map[string]string
	if err := reg.update(ctx, func(f *registryFile) error {
		u, ok := f.Users[p.User]
		if !ok {
			u = registryUser{Site: p.Site.ID, Grants: map[string]string{}, Server: p.ServerID}
		}
		have = u.Grants
		f.Users[p.User] = u
		return nil
	}); err != nil {
		return nil, err
	}
	undo := func(cause error) (map[string]string, error) {
		if owner == 0 {
			dctx, cancel := detached(ctx, time.Minute)
			defer cancel()
			cause = errors.Join(cause, pg.Exec(dctx, "", "DROP ROLE IF EXISTS "+host.QuotePGIdent(p.User)))
			o.forgetUser(ctx, p.User)
		}
		return nil, cause
	}
	u := host.QuotePGIdent(p.User)
	verb := "CREATE"
	if owner != 0 {
		verb = "ALTER"
	}
	if err := pg.Exec(ctx, "", verb+" ROLE "+u+" LOGIN PASSWORD "+host.QuotePGLiteral(p.Password)+fmt.Sprintf(" CONNECTION LIMIT %d", defaultMaxConns)); err != nil {
		return undo(fmt.Errorf("create the database user %s: %w", p.User, err))
	}
	// The administrator is made a member of the account so that it may end
	// the account's sessions when its database is dropped (an administrator
	// that is not a superuser may only signal sessions of roles it is a
	// member of).
	if err := pg.Exec(ctx, "", "GRANT "+u+" TO "+host.QuotePGIdent(o.pgAdmin(p.ServerID))); err != nil {
		return undo(fmt.Errorf("let the administrator end the sessions of %s: %w", p.User, err))
	}
	if err := o.applyGrantsPG(ctx, pg, p.User, have, want); err != nil {
		return undo(err)
	}
	return map[string]string{"user": p.User}, nil
}

// dropUserPG drops an account on a PostgreSQL server after taking back its
// grants.
func (o *Ops) dropUserPG(ctx context.Context, pg *host.PostgresAdmin, user string, grants map[string]string) error {
	var errs []error
	for _, d := range sortedKeys(grants) {
		errs = append(errs, o.revokePG(ctx, pg, user, d, grants[d]))
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	// A session of the account would outlive it (and could not be ended by
	// an administrator that is not a superuser once the role is gone).
	if err := pg.Exec(ctx, "", "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename = "+host.QuotePGLiteral(user)); err != nil {
		return fmt.Errorf("end the sessions of %s: %w", user, err)
	}
	return pg.Exec(ctx, "", "DROP ROLE IF EXISTS "+host.QuotePGIdent(user))
}

// emptyPG empties a PostgreSQL database: its schema is dropped and made
// again, owned by the database's owner role, and the read grants of the
// record put back (dropping the database would lose its CONNECT grants).
func (o *Ops) emptyPG(ctx context.Context, pg *host.PostgresAdmin, database string) error {
	role := host.QuotePGIdent(ownerRole(database))
	if err := pgExecAll(ctx, pg, database, "DROP SCHEMA IF EXISTS public CASCADE", "CREATE SCHEMA public AUTHORIZATION "+role,
		"REVOKE ALL ON SCHEMA public FROM PUBLIC"); err != nil {
		return fmt.Errorf("empty %s: %w", database, err)
	}
	users, err := o.registry().SiteOfDatabase(ctx, database)
	if err != nil {
		return err
	}
	var errs []error
	for user, grants := range users {
		if grants[database] == site.GrantRead {
			errs = append(errs, o.grantPG(ctx, pg, user, database, site.GrantRead))
		}
	}
	return errors.Join(errs...)
}

// importPG feeds a dump to database as its owner role.
func (o *Ops) importPG(ctx context.Context, pg *host.PostgresAdmin, database, role, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := pg.Restore(ctx, database, role, f); err != nil {
		return fmt.Errorf("import into %s: %w", database, err)
	}
	return nil
}

// tablesPG lists the tables of database's schema public.
func tablesPG(ctx context.Context, pg *host.PostgresAdmin, database string) ([]string, error) {
	return pg.QueryStrings(ctx, database, "SELECT table_name::text FROM information_schema.tables WHERE table_schema = 'public' AND table_type = 'BASE TABLE' ORDER BY table_name")
}

// verifyPG restores the dump into a scratch database on the same server and
// counts its tables (the restore test of a PostgreSQL database).
func (o *Ops) verifyPG(ctx context.Context, pg *host.PostgresAdmin, adminUser, path string, want int) (int, error) {
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return 0, err
	}
	scratch := "cgv_" + hex.EncodeToString(suffix)
	if err := pgCreateDatabase(ctx, pg, scratch, adminUser); err != nil {
		return 0, fmt.Errorf("create the restore-test database: %w", err)
	}
	defer func() {
		dctx, cancel := detached(ctx, 5*time.Minute)
		defer cancel()
		if err := pgDropDatabase(dctx, pg, scratch); err != nil {
			o.log().Warn("drop the restore-test database", "database", scratch, "err", err)
		}
		if err := pg.Exec(dctx, "", "DROP ROLE IF EXISTS "+host.QuotePGIdent(ownerRole(scratch))); err != nil {
			o.log().Warn("drop the restore-test role", "role", ownerRole(scratch), "err", err)
		}
	}()
	if err := o.importPG(ctx, pg, scratch, ownerRole(scratch), path); err != nil {
		return 0, err
	}
	tables, err := tablesPG(ctx, pg, scratch)
	if err != nil {
		return 0, fmt.Errorf("count the restored tables: %w", err)
	}
	n := len(tables)
	switch {
	case want < 0 && n == 0:
		return 0, agentrpc.Invalid("the dump imported no tables")
	case n < want:
		return 0, agentrpc.Invalid("the dump imported %d tables; the database had %d", n, want)
	}
	return n, nil
}

// listPG reads the size and table count of databases on a PostgreSQL server.
func (o *Ops) listPG(ctx context.Context, pg *host.PostgresAdmin, names []string) (map[string][2]int64, error) {
	out := map[string][2]int64{}
	for _, d := range names {
		size, err := pg.QueryStrings(ctx, "", "SELECT pg_database_size("+host.QuotePGLiteral(d)+")::text")
		if err != nil {
			return nil, err
		}
		tables, err := tablesPG(ctx, pg, d)
		if err != nil {
			return nil, err
		}
		var n int64
		if len(size) == 1 {
			_, _ = fmt.Sscan(size[0], &n)
		}
		out[d] = [2]int64{int64(len(tables)), n}
	}
	return out, nil
}
