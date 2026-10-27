package db

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
	"github.com/go-sql-driver/mysql"
)

// A site's databases and database accounts (SPEC §2, §7): created, dropped,
// granted and listed only as the agent's record says they are the site's.

// Password bounds for a database account. The panel generates 32
// characters; a person may choose one of at least 12.
const (
	minPassword = 12
	maxPassword = 256
)

// defaultMaxConns caps each account's connections.
const defaultMaxConns = 50

// privileges are what each grant level gives on one database.
var privileges = map[string]string{
	site.GrantAll:  "ALL PRIVILEGES",
	site.GrantRead: "SELECT, SHOW VIEW",
}

// registry returns the record, built on first use.
func (o *Ops) registry() *Registry {
	o.regOnce.Do(func() {
		path := o.RegistryFile
		if path == "" {
			path = filepath.Join(o.WorkDir, "databases.json")
		}
		o.reg = &Registry{path: path, scan: o.scanServer}
	})
	return o.reg
}

// LoadRegistry reads the record, rebuilding it from the server on an
// install from before it (SPEC §2). Run it at start; a failure is retried
// on first use, and every request naming a database waits for it.
func (o *Ops) LoadRegistry(ctx context.Context) error {
	return o.registry().view(ctx, func(*registryFile) {})
}

// The rebuild's two queries (exported for the fakes of other packages'
// tests).
const (
	ScanDatabasesQuery = "SELECT SCHEMA_NAME FROM information_schema.SCHEMATA"
	ScanUsersQuery     = "SELECT User FROM mysql.global_priv WHERE Host = 'localhost'"
)

// scanServer lists the server's databases and local accounts.
func (o *Ops) scanServer(ctx context.Context) (dbs, users []string, err error) {
	dbs, err = o.MariaDB.QueryStrings(ctx, ScanDatabasesQuery)
	if err != nil {
		return nil, nil, err
	}
	users, err = o.MariaDB.QueryStrings(ctx, ScanUsersQuery)
	return dbs, users, err
}

// localDB reports whether the site has a primary database the agent
// manages.
func localDB(s state.Site) bool { return s.Config.Database.Enabled && !s.Config.Database.External }

// SiteDatabase returns the site's database called requested ("" is the
// primary), refusing any name the record does not give the site.
func (o *Ops) SiteDatabase(ctx context.Context, s state.Site, requested string) (string, error) {
	if requested == "" {
		if !localDB(s) {
			return "", agentrpc.Invalid("%s has no primary database", s.Domain)
		}
		requested = s.Config.Database.Name
	}
	if !site.ValidDBName(requested) {
		return "", agentrpc.Invalid("invalid database name")
	}
	owner, err := o.registry().DatabaseOwner(ctx, requested)
	if err != nil {
		return "", err
	}
	if owner != s.ID || s.ID <= 0 {
		return "", agentrpc.Invalid("%q is not a database of %s", requested, s.Domain)
	}
	return requested, nil
}

// SiteDatabases lists the databases recorded for the site.
func (o *Ops) SiteDatabases(ctx context.Context, s state.Site) ([]string, error) {
	rec, err := o.registry().Site(ctx, s.ID)
	return rec.Databases, err
}

// known reports whether name is a database the agent acts on: one the
// record gives a site, or one of its own scratch databases.
func (o *Ops) known(ctx context.Context, name string) bool {
	if scratchNameRe.MatchString(name) {
		return true
	}
	if !site.ValidDBName(name) {
		return false
	}
	owner, err := o.registry().DatabaseOwner(ctx, name)
	return err == nil && owner > 0
}

// ownedUser refuses an account the record does not give the site.
func (o *Ops) ownedUser(ctx context.Context, s state.Site, user string) error {
	owner, err := o.registry().UserOwner(ctx, user)
	if err != nil {
		return err
	}
	if owner != s.ID || s.ID <= 0 {
		return agentrpc.Invalid("%q is not a database user of %s", user, s.Domain)
	}
	return nil
}

// checkPassword refuses a password the agent may not set: out of bounds,
// with control characters, or with ' " \ (SPEC §3.1: the primary
// account's goes into wp-config.php through wp-cli, which quotes none of
// them right).
func checkPassword(p string) error {
	if len(p) < minPassword || len(p) > maxPassword {
		return agentrpc.Invalid("the database password must be %d to %d characters", minPassword, maxPassword)
	}
	if strings.ContainsFunc(p, unicode.IsControl) {
		return agentrpc.Invalid("the database password contains control characters")
	}
	if strings.ContainsAny(p, `'"\`) {
		return agentrpc.Invalid(`the database password may not contain ', " or \`)
	}
	return nil
}

func commerce(s state.Site) bool {
	return s.Type == state.SiteWooCommerce || s.Profile == state.ProfileCommerce
}

// userExists reports whether the server has an account of this name, from
// any host.
func (o *Ops) userExists(ctx context.Context, server int64, user string) (bool, error) {
	m, err := o.mariaOfServer(server)
	if err != nil {
		return false, err
	}
	got, err := m.QueryStrings(ctx, "SELECT User FROM mysql.global_priv WHERE User = ?", user)
	if err != nil {
		return false, fmt.Errorf("look for the account %s: %w", user, err)
	}
	return len(got) > 0, nil
}

// CreateDB makes one of a site's databases and records it (SPEC §7). A
// name the record gives another site, or one the server has that is no
// site's, is a conflict; one already the site's is made if missing.
func (o *Ops) CreateDB(ctx context.Context, p rpc.SiteDatabaseParams) (map[string]string, error) {
	if err := p.Validate(); err != nil {
		return nil, agentrpc.Invalid("%s", err)
	}
	reg := o.registry()
	owner, err := reg.DatabaseOwner(ctx, p.Name)
	if err != nil {
		return nil, err
	}
	m, err := o.mariaOfServer(p.ServerID)
	if err != nil {
		return nil, err
	}
	switch {
	case owner != 0 && owner != p.Site.ID:
		return nil, agentrpc.Conflict("a database called %s already exists on this server", p.Name)
	case owner != 0:
		have, err := reg.DatabaseServer(ctx, p.Name)
		if err != nil {
			return nil, err
		}
		if have != p.ServerID {
			return nil, agentrpc.Conflict("%s is on another database server", p.Name)
		}
	case owner == 0:
		exists, err := o.exists(ctx, p.ServerID, p.Name)
		if err != nil {
			return nil, err
		}
		if exists {
			return nil, agentrpc.Conflict("a database called %s already exists on this server", p.Name)
		}
	}
	// The box's own server is tuned for the site; an external one is not
	// ours to tune.
	if p.ServerID == 0 {
		if err := o.Tune(ctx, commerce(p.Site)); err != nil {
			return nil, err
		}
	}
	if err := reg.update(ctx, func(f *registryFile) error {
		f.Databases[p.Name] = p.Site.ID
		if p.ServerID != 0 {
			f.Servers[p.Name] = p.ServerID
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := m.Exec(ctx, "CREATE DATABASE IF NOT EXISTS `"+p.Name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		if owner == 0 {
			o.forgetDatabase(ctx, p.Name)
		}
		return nil, fmt.Errorf("create the database %s: %w", p.Name, err)
	}
	return map[string]string{"database": p.Name}, nil
}

// forgetDatabase takes a database and every grant on it out of the record.
func (o *Ops) forgetDatabase(ctx context.Context, name string) {
	err := o.registry().update(ctx, func(f *registryFile) error {
		delete(f.Databases, name)
		delete(f.Servers, name)
		for _, u := range f.Users {
			delete(u.Grants, name)
		}
		return nil
	})
	if err != nil {
		o.log().Error("take a database out of the record", "database", name, "err", err)
	}
}

// DropDB drops one of a site's databases: its accounts lose their grants
// on it first (a database created later with the name must not inherit
// them). The primary database, which the site's application uses, is
// refused.
func (o *Ops) DropDB(ctx context.Context, p rpc.SiteDatabaseParams) (map[string]string, error) {
	if err := p.Validate(); err != nil {
		return nil, agentrpc.Invalid("%s", err)
	}
	name, err := o.SiteDatabase(ctx, p.Site, p.Name)
	if err != nil {
		return nil, err
	}
	if localDB(p.Site) && p.Site.Config.Database.Name == name {
		return nil, agentrpc.Conflict("%s is the site's primary database, which its application uses", name)
	}
	rec, err := o.registry().Site(ctx, p.Site.ID)
	if err != nil {
		return nil, err
	}
	m, err := o.maria(ctx, name)
	if err != nil {
		return nil, err
	}
	var errs []error
	for user, grants := range rec.Users {
		if _, ok := grants[name]; ok {
			errs = append(errs, o.revoke(ctx, user, name))
		}
	}
	errs = append(errs, m.Exec(ctx, "DROP DATABASE IF EXISTS `"+name+"`"))
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("drop the database %s: %w", name, err)
	}
	o.forgetDatabase(ctx, name)
	return map[string]string{"dropped": name}, nil
}

// revoke takes every privilege user has on database; having none is not an
// error.
func (o *Ops) revoke(ctx context.Context, user, database string) error {
	m, err := o.maria(ctx, database)
	if err != nil {
		return err
	}
	err = m.Exec(ctx, "REVOKE ALL PRIVILEGES ON `"+host.GrantPattern(database)+"`.* FROM ?@'localhost'", user)
	var me *mysql.MySQLError
	if errors.As(err, &me) && (me.Number == 1141 || me.Number == 1147) {
		return nil // there is no such grant
	}
	if err != nil {
		return fmt.Errorf("revoke %s's privileges on %s: %w", user, database, err)
	}
	return nil
}

// wantGrants checks grants name only the site's databases, once each.
func (o *Ops) wantGrants(ctx context.Context, s state.Site, grants []state.DBGrant) (map[string]string, error) {
	want := map[string]string{}
	for _, g := range grants {
		if _, err := o.SiteDatabase(ctx, s, g.Database); err != nil || g.Database == "" {
			return nil, agentrpc.Invalid("%q is not a database of %s", g.Database, s.Domain)
		}
		if _, ok := privileges[g.Level]; !ok {
			return nil, agentrpc.Invalid("invalid grant level %q", g.Level)
		}
		if _, dup := want[g.Database]; dup {
			return nil, agentrpc.Invalid("%s is granted twice", g.Database)
		}
		want[g.Database] = g.Level
	}
	return want, nil
}

// applyGrants takes user from the grants it has to want, one database at a
// time, and records what it reached, also when a step fails.
func (o *Ops) applyGrants(ctx context.Context, m host.MariaDB, user string, have, want map[string]string) error {
	got := map[string]string{}
	for d, l := range have {
		got[d] = l
	}
	var errs []error
	for _, d := range sortedKeys(have) {
		if want[d] == have[d] {
			continue
		}
		if err := o.revoke(ctx, user, d); err != nil {
			errs = append(errs, err)
			continue
		}
		delete(got, d)
	}
	for _, d := range sortedKeys(want) {
		if got[d] == want[d] {
			continue
		}
		q := "GRANT " + privileges[want[d]] + " ON `" + host.GrantPattern(d) + "`.* TO ?@'localhost'"
		if err := m.Exec(ctx, q, user); err != nil {
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

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// CreateUser makes one of a site's database accounts with its password and
// its grants, and records it (SPEC §7). An account the record gives another
// site, or one the server has that is no site's, is a conflict; one already
// the site's gets this password.
func (o *Ops) CreateUser(ctx context.Context, p rpc.DBUserParams) (map[string]string, error) {
	if err := p.Validate(); err != nil {
		return nil, agentrpc.Invalid("%s", err)
	}
	if err := checkPassword(p.Password); err != nil {
		return nil, err
	}
	reg := o.registry()
	owner, err := reg.UserOwner(ctx, p.User)
	if err != nil {
		return nil, err
	}
	m, err := o.mariaOfServer(p.ServerID)
	if err != nil {
		return nil, err
	}
	switch {
	case owner != 0 && owner != p.Site.ID:
		return nil, agentrpc.Conflict("a database user called %s already exists on this server", p.User)
	case owner != 0:
		have, err := reg.UserServer(ctx, p.User)
		if err != nil {
			return nil, err
		}
		if have != p.ServerID {
			return nil, agentrpc.Conflict("%s is on another database server", p.User)
		}
	case owner == 0:
		exists, err := o.userExists(ctx, p.ServerID, p.User)
		if err != nil {
			return nil, err
		}
		if exists {
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
			err := m.Exec(context.WithoutCancel(ctx), "DROP USER IF EXISTS ?@'localhost'", p.User)
			o.forgetUser(ctx, p.User)
			cause = errors.Join(cause, err)
		}
		return nil, cause
	}
	stmts := []struct {
		q    string
		args []any
	}{
		{"CREATE USER IF NOT EXISTS ?@'localhost' IDENTIFIED BY ?", []any{p.User, p.Password}},
		{fmt.Sprintf("ALTER USER ?@'localhost' IDENTIFIED BY ? WITH MAX_USER_CONNECTIONS %d", defaultMaxConns), []any{p.User, p.Password}},
	}
	for _, s := range stmts {
		if err := m.Exec(ctx, s.q, s.args...); err != nil {
			return undo(fmt.Errorf("create the database user %s: %w", p.User, err))
		}
	}
	if err := o.applyGrants(ctx, m, p.User, have, want); err != nil {
		return undo(err)
	}
	return map[string]string{"user": p.User}, nil
}

// forgetUser takes an account out of the record.
func (o *Ops) forgetUser(ctx context.Context, user string) {
	if err := o.registry().update(ctx, func(f *registryFile) error {
		delete(f.Users, user)
		return nil
	}); err != nil {
		o.log().Error("take a database user out of the record", "user", user, "err", err)
	}
}

// primaryUser reports whether user is the account the site's application
// signs in with.
func primaryUser(s state.Site, user string) bool {
	return localDB(s) && s.Config.Database.User == user
}

// DropUser drops one of a site's accounts. The primary account is refused.
func (o *Ops) DropUser(ctx context.Context, p rpc.DBUserParams) (map[string]string, error) {
	if err := o.ownedUser(ctx, p.Site, p.User); err != nil {
		return nil, err
	}
	if primaryUser(p.Site, p.User) {
		return nil, agentrpc.Conflict("%s is the account the site's application signs in with", p.User)
	}
	m, err := o.mariaOfUser(ctx, p.User)
	if err != nil {
		return nil, err
	}
	if err := m.Exec(ctx, "DROP USER IF EXISTS ?@'localhost'", p.User); err != nil {
		return nil, fmt.Errorf("drop the database user %s: %w", p.User, err)
	}
	o.forgetUser(ctx, p.User)
	return map[string]string{"dropped": p.User}, nil
}

// SetGrants replaces an account's grants on the site's databases. The
// primary account keeps all privileges on the primary database.
func (o *Ops) SetGrants(ctx context.Context, p rpc.DBUserParams) (map[string]string, error) {
	if err := p.Validate(); err != nil {
		return nil, agentrpc.Invalid("%s", err)
	}
	if err := o.ownedUser(ctx, p.Site, p.User); err != nil {
		return nil, err
	}
	want, err := o.wantGrants(ctx, p.Site, p.Grants)
	if err != nil {
		return nil, err
	}
	if primaryUser(p.Site, p.User) && want[p.Site.Config.Database.Name] != site.GrantAll {
		return nil, agentrpc.Invalid("%s must keep all privileges on %s, the site's primary database", p.User, p.Site.Config.Database.Name)
	}
	server, err := o.registry().UserServer(ctx, p.User)
	if err != nil {
		return nil, err
	}
	if err := o.sameServer(ctx, server, want); err != nil {
		return nil, err
	}
	m, err := o.mariaOfServer(server)
	if err != nil {
		return nil, err
	}
	rec, err := o.registry().Site(ctx, p.Site.ID)
	if err != nil {
		return nil, err
	}
	if err := o.applyGrants(ctx, m, p.User, rec.Users[p.User], want); err != nil {
		return nil, err
	}
	return map[string]string{"user": p.User}, nil
}

// SetPassword gives an account a new password. The primary account of a
// WordPress site gets it in wp-config.php too (PrimaryPassword); when that
// fails, the account gets its previous password back.
func (o *Ops) SetPassword(ctx context.Context, p rpc.DBUserParams) (map[string]string, error) {
	if err := o.ownedUser(ctx, p.Site, p.User); err != nil {
		return nil, err
	}
	if err := checkPassword(p.Password); err != nil {
		return nil, err
	}
	m, err := o.mariaOfUser(ctx, p.User)
	if err != nil {
		return nil, err
	}
	wire := primaryUser(p.Site, p.User) && o.PrimaryPassword != nil
	var plugin, auth string
	if wire {
		var err error
		if plugin, auth, err = o.authOf(ctx, p.User); err != nil {
			return nil, err
		}
	}
	if err := m.Exec(ctx, "ALTER USER ?@'localhost' IDENTIFIED BY ?", p.User, p.Password); err != nil {
		return nil, fmt.Errorf("change the password of %s: %w", p.User, err)
	}
	if wire {
		if err := o.PrimaryPassword(ctx, p.Site, p.Password); err != nil {
			back := o.MariaDB.Exec(context.WithoutCancel(ctx), "ALTER USER ?@'localhost' IDENTIFIED VIA "+plugin+" USING ?", p.User, auth)
			if back != nil {
				return nil, fmt.Errorf("the site's configuration could not take the new password (%v), and putting the old one back failed: %w", err, back)
			}
			return nil, fmt.Errorf("the site's configuration could not take the new password; the old one is kept: %w", err)
		}
	}
	return map[string]string{"user": p.User}, nil
}

// borrowable are the authentications SetPassword can put back exactly.
var borrowable = map[string]bool{"mysql_native_password": true, "ed25519": true}

// authOf reads an account's own authentication, to put it back.
func (o *Ops) authOf(ctx context.Context, user string) (plugin, auth string, err error) {
	const q = "SELECT JSON_VALUE(Priv, ?) FROM mysql.global_priv WHERE User = ? AND Host = 'localhost'"
	pl, err := o.MariaDB.QueryStrings(ctx, q, "$.plugin", user)
	if err != nil || len(pl) != 1 {
		return "", "", fmt.Errorf("read %s's authentication: %v", user, err)
	}
	a, err := o.MariaDB.QueryStrings(ctx, q, "$.authentication_string", user)
	if err != nil || len(a) != 1 {
		return "", "", fmt.Errorf("read %s's authentication: %v", user, err)
	}
	if !borrowable[pl[0]] {
		return "", "", agentrpc.Invalid("the account %s signs in with %q; its password cannot be changed safely here", user, pl[0])
	}
	return pl[0], a[0], nil
}

// List returns the site's databases, with their size and table count, and
// its accounts with their grants, as recorded.
func (o *Ops) List(ctx context.Context, p rpc.DBListParams) (rpc.DBListResult, error) {
	rec, err := o.registry().Site(ctx, p.Site.ID)
	if err != nil {
		return rpc.DBListResult{}, err
	}
	out := rpc.DBListResult{Databases: []rpc.DBInfo{}, Users: []rpc.DBUserInfo{}}
	servers := map[string]int64{}
	for _, d := range rec.Databases {
		id, err := o.registry().DatabaseServer(ctx, d)
		if err != nil {
			return rpc.DBListResult{}, err
		}
		servers[d] = id
		out.Databases = append(out.Databases, rpc.DBInfo{Name: d, ServerID: id, SizeBytes: -1, Tables: -1})
	}
	for _, u := range sortedUsers(rec.Users) {
		id, err := o.registry().UserServer(ctx, u)
		if err != nil {
			return rpc.DBListResult{}, err
		}
		info := rpc.DBUserInfo{Name: u, ServerID: id, Grants: []state.DBGrant{}}
		for _, d := range sortedKeys(rec.Users[u]) {
			info.Grants = append(info.Grants, state.DBGrant{Database: d, Level: rec.Users[u][d]})
		}
		out.Users = append(out.Users, info)
	}
	// The sizes are read from each server the site's databases are on.
	byServer := map[int64][]string{}
	for _, d := range rec.Databases {
		byServer[servers[d]] = append(byServer[servers[d]], d)
	}
	stats := map[string][2]int64{}
	failed := map[int64]bool{}
	for server, names := range byServer {
		m, err := o.mariaOfServer(server)
		var rows []string
		if err == nil {
			args := make([]any, len(names))
			for i, d := range names {
				args[i] = d
			}
			q := "SELECT CONCAT(TABLE_SCHEMA, ' ', COUNT(*), ' ', IFNULL(SUM(DATA_LENGTH + INDEX_LENGTH), 0)) FROM information_schema.TABLES WHERE TABLE_SCHEMA IN (?" +
				strings.Repeat(", ?", len(args)-1) + ") GROUP BY TABLE_SCHEMA"
			rows, err = m.QueryStrings(ctx, q, args...)
		}
		if err != nil {
			o.log().Warn("read the sizes of a site's databases", "site", p.Site.Domain, "server", server, "err", err)
			out.StatsError = "the sizes could not be read"
			failed[server] = true
			continue
		}
		for _, r := range rows {
			f := strings.Fields(r)
			if len(f) == 3 {
				stats[f[0]] = [2]int64{atoi(f[1]), atoi(f[2])}
			}
		}
	}
	for i := range out.Databases {
		if failed[out.Databases[i].ServerID] {
			continue
		}
		st := stats[out.Databases[i].Name] // an empty database has no row
		out.Databases[i].Tables, out.Databases[i].SizeBytes = st[0], st[1]
	}
	return out, nil
}

// sameServer refuses grants on databases of another server than the
// account's.
func (o *Ops) sameServer(ctx context.Context, server int64, want map[string]string) error {
	for d := range want {
		id, err := o.registry().DatabaseServer(ctx, d)
		if err != nil {
			return err
		}
		if id != server {
			return agentrpc.Invalid("%s is on another database server than the account", d)
		}
	}
	return nil
}

func sortedUsers(m map[string]map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ProvisionSite makes a new site's primary database and its account,
// granted all of it (sites.Create).
func (o *Ops) ProvisionSite(ctx context.Context, s state.Site, password string) error {
	if !localDB(s) {
		return nil
	}
	d := s.Config.Database
	if _, err := o.CreateDB(ctx, rpc.SiteDatabaseParams{Site: s, Name: d.Name}); err != nil {
		return err
	}
	_, err := o.CreateUser(ctx, rpc.DBUserParams{Site: s, User: d.User, Password: password,
		Grants: []state.DBGrant{{Database: d.Name, Level: site.GrantAll}}})
	return err
}

// DropSite drops every database and account recorded for the site, and
// takes them out of the record (sites.Delete and the undo of a creation).
// Each is attempted; the errors are joined.
func (o *Ops) DropSite(ctx context.Context, siteID int64) error {
	rec, err := o.registry().Site(ctx, siteID)
	if err != nil {
		return err
	}
	var errs []error
	for _, u := range sortedUsers(rec.Users) {
		m, err := o.mariaOfUser(ctx, u)
		if err == nil {
			err = m.Exec(ctx, "DROP USER IF EXISTS ?@'localhost'", u)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("drop the database user %s: %w", u, err))
			continue
		}
		o.forgetUser(ctx, u)
	}
	for _, d := range rec.Databases {
		m, err := o.maria(ctx, d)
		if err == nil {
			err = m.Exec(ctx, "DROP DATABASE IF EXISTS `"+d+"`")
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("drop the database %s: %w", d, err))
			continue
		}
		o.forgetDatabase(ctx, d)
	}
	return errors.Join(errs...)
}

// importAs is the account an import into database runs as (SPEC §5,
// SR-T7): a site's database as an account of that site with all privileges
// on it (the one of the database's own name first, then the first by name),
// so the dump's triggers, views and routines are the site's and keep
// working; the restore test's scratch database as a scoped account that may
// create triggers; a staged migration's as one that may not. A site's
// database no account of the site may write gets a scoped account without
// TRIGGER: a dump with a trigger then fails, rather than leave a trigger
// whose definer is gone.
func (o *Ops) importAs(ctx context.Context, database string) host.ImportAs {
	switch {
	case strings.HasPrefix(database, "cgv_"):
		return host.ImportAs{Triggers: true}
	case scratchNameRe.MatchString(database):
		return host.ImportAs{}
	}
	owner, err := o.registry().DatabaseOwner(ctx, database)
	if err != nil || owner == 0 {
		return host.ImportAs{}
	}
	rec, err := o.registry().Site(ctx, owner)
	if err != nil {
		return host.ImportAs{}
	}
	if rec.Users[database][database] == site.GrantAll {
		return host.ImportAs{Site: database}
	}
	for _, u := range sortedUsers(rec.Users) {
		if rec.Users[u][database] == site.GrantAll {
			return host.ImportAs{Site: u}
		}
	}
	o.log().Warn("no account of the site has all privileges on the database: importing as a scoped account, without triggers", "database", database)
	return host.ImportAs{}
}

// tenantAccount reports whether user is an account of a site, for the
// startup sweep (Unborrow).
func (o *Ops) tenantAccount(ctx context.Context) func(string) bool {
	return func(user string) bool {
		if site.PanelDefaultSite(user) > 0 {
			return true
		}
		owner, err := o.registry().UserOwner(ctx, user)
		return err == nil && owner > 0
	}
}
