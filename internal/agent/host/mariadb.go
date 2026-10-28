package host

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/go-sql-driver/mysql"
)

// ErrInvalidDatabase is a database name the agent will not put in SQL.
var ErrInvalidDatabase = errors.New("invalid database name")

// dbNameRe is the identifier rule for databases the agent manages.
var dbNameRe = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

// reservedDatabases are the server's own schemas: never dumped, imported,
// or granted to a scoped account, whatever the caller checked (SR-R1).
var reservedDatabases = map[string]bool{"mysql": true, "sys": true, "performance_schema": true, "information_schema": true}

// ValidDatabaseName reports whether name is acceptable as a database.
func ValidDatabaseName(name string) bool {
	return dbNameRe.MatchString(name) && !reservedDatabases[strings.ToLower(name)]
}

// GrantPattern is database as the name of a GRANT or REVOKE: there `_` and
// `%` are LIKE wildcards, so `site_1`.* also granted site01, siteA1 and any
// database of that shape the account created (SR-T2). Escaped, the grant is
// on that one database.
func GrantPattern(database string) string {
	return strings.NewReplacer(`\`, `\\`, `_`, `\_`, `%`, `\%`).Replace(database)
}

// ValidTableName reports whether name is acceptable as a table to dump. The
// rule is the database rule: it never starts with '-', so a table name can
// never be read as an option by mariadb-dump.
func ValidTableName(name string) bool { return dbNameRe.MatchString(name) }

// DataTimeout bounds a dump or an import. They move a whole database and
// last as long as it takes; the caller's context is the real deadline.
const DataTimeout = 24 * time.Hour

// ScopedAccountRe matches exactly the accounts ScopedAccount creates, as
// MariaDB prints them, so a sweep can never drop an account it did not make.
var ScopedAccountRe = regexp.MustCompile(`^'cgq_[0-9a-f]{16}'@'localhost'$`)

// ScopedAccount is a short-lived MariaDB account confined to one database,
// with a 0600 client defaults file holding its credentials.
type ScopedAccount struct {
	User         string
	Password     string
	DefaultsFile string

	once    sync.Once
	cleanup func() error
	err     error
}

// Close removes the defaults file and drops the account. Safe to call twice.
func (s *ScopedAccount) Close() error {
	s.once.Do(func() {
		if s.cleanup != nil {
			s.err = s.cleanup()
		}
	})
	return s.err
}

// MariaDB administers the local server.
type MariaDB interface {
	// Exec runs one statement as root. args are bound as values only.
	Exec(ctx context.Context, query string, args ...any) error
	// QueryStrings returns the first column of every row, as text.
	QueryStrings(ctx context.Context, query string, args ...any) ([]string, error)
	ScopedAccount(ctx context.Context, database string, readOnly bool) (*ScopedAccount, error)
	// Dump streams mariadb-dump of database into w: the whole database, or
	// only tables when any are given.
	Dump(ctx context.Context, database string, w io.Writer, tables ...string) error
	// DumpAs is Dump signed in as a database user with its own password
	// (Quarry's signed-in users): the server shows the dump what that
	// user may read, nothing more.
	DumpAs(ctx context.Context, as Credentials, database string, w io.Writer, tables ...string) error
	// Import feeds r to the sandboxed mariadb client, as the account as
	// says, with every DEFINER clause rewritten to that account.
	Import(ctx context.Context, database string, r io.Reader, as ImportAs) error
	// Unborrow puts back their own authentication alone on the site
	// accounts (those tenant accepts) an import cut short left with a
	// second password. Run it at start, before serving.
	Unborrow(ctx context.Context, tenant func(user string) bool) error
}

// MariaDBAdmin is the real MariaDB.
type MariaDBAdmin struct {
	Socket  string // /run/mysqld/mysqld.sock
	WorkDir string // where scoped defaults files go (root-only)
	Tools   Exec   // runs mariadb-dump and the mariadb client
	// Remote, when set, points the adapter at an external server instead of
	// the socket (mariadb_remote.go).
	Remote *RemoteServer

	mu sync.Mutex
	db *sql.DB
	// borrows serialises borrow per site account.
	borrowMu sync.Mutex
	borrows  map[string]*sync.Mutex
	// execFn replaces the root connection's Exec in tests.
	execFn func(ctx context.Context, query string, args ...any) error
	// queryFn replaces the root connection's QueryStrings in tests.
	queryFn func(ctx context.Context, query string, args ...any) ([]string, error)
}

// NewMariaDB returns the real adapter. Nothing connects until first use.
func NewMariaDB(socket, workDir string, e Exec) *MariaDBAdmin {
	return &MariaDBAdmin{Socket: socket, WorkDir: workDir, Tools: e}
}

// rootDB opens (once) a pool connected as root over the socket. MariaDB's
// unix_socket plugin authenticates root by the peer's uid: there is no
// password.
func (m *MariaDBAdmin) rootDB() (*sql.DB, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.db != nil {
		return m.db, nil
	}
	cfg, err := m.rootConfig()
	if err != nil {
		return nil, err
	}
	conn, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(conn)
	db.SetMaxOpenConns(4)
	db.SetConnMaxIdleTime(time.Minute)
	m.db = db
	return db, nil
}

// RootConfig is the driver configuration of the root pool. AllowAllFiles
// stays false and no file or reader is ever registered with the driver, so
// a LOAD DATA LOCAL INFILE sent on it reads nothing of this machine.
func RootConfig(socket string) *mysql.Config {
	cfg := mysql.NewConfig()
	cfg.User = "root"
	cfg.Net, cfg.Addr = "unix", socket
	cfg.Timeout = 10 * time.Second
	cfg.ReadTimeout = 5 * time.Minute
	cfg.WriteTimeout = 5 * time.Minute
	// Statements such as CREATE USER … IDENTIFIED BY ? cannot be prepared
	// server-side; the driver binds the value as an escaped literal.
	cfg.InterpolateParams = true
	cfg.AllowAllFiles = false
	return cfg
}

// Close releases the pool.
func (m *MariaDBAdmin) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.db == nil {
		return nil
	}
	err := m.db.Close()
	m.db = nil
	return err
}

func (m *MariaDBAdmin) Exec(ctx context.Context, query string, args ...any) error {
	query = m.q(query)
	if m.execFn != nil {
		return m.execFn(ctx, query, args...)
	}
	db, err := m.rootDB()
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, query, args...)
	return err
}

func (m *MariaDBAdmin) QueryStrings(ctx context.Context, query string, args ...any) ([]string, error) {
	query = m.q(query)
	if m.queryFn != nil {
		return m.queryFn(ctx, query, args...)
	}
	db, err := m.rootDB()
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []string
	dest := make([]any, len(cols))
	for i := range dest {
		dest[i] = new(sql.RawBytes)
	}
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		if len(dest) > 0 {
			out = append(out, string(*dest[0].(*sql.RawBytes)))
		}
	}
	return out, rows.Err()
}

// Privileges granted to a scoped account. Write mode withholds TRIGGER,
// EVENT and the routine privileges: objects created with those carry the
// account as DEFINER, and it is dropped seconds later, leaving them broken.
const (
	scopedReadPrivileges  = "SELECT, SHOW VIEW"
	scopedWritePrivileges = "SELECT, INSERT, UPDATE, DELETE, CREATE, DROP, INDEX, ALTER, " +
		"CREATE TEMPORARY TABLES, LOCK TABLES, CREATE VIEW, SHOW VIEW"
)

// newScopedCredentials draws a cgq_<16 hex> name and a base64url password.
func newScopedCredentials() (user, password string, err error) {
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return "", "", err
	}
	pw := make([]byte, 24)
	if _, err := rand.Read(pw); err != nil {
		return "", "", err
	}
	return "cgq_" + hex.EncodeToString(suffix), base64.RawURLEncoding.EncodeToString(pw), nil
}

// writeDefaultsFile writes a [client] file readable by root only. The
// client reads it alone (--defaults-file, DefaultsArg): no other option file,
// so a [client] or [mysql] user= in root's ~/.my.cnf or in /etc/mysql can
// never sign a tenant's import or dump in as someone else (root, by
// unix_socket). What the server's own client files would give is written
// here: the socket and utf8mb4.
func writeDefaultsFile(dir, socket, user, password string) (string, error) {
	return writeClientFile(dir, func(string) string {
		if socket == "" {
			return ""
		}
		return "socket=" + OptionValue(socket) + "\n"
	}, nil, user, password)
}

// writeClientFile is writeDefaultsFile for any server: conn gives the lines
// that say where it is, and for a TLS server with a CA of its own the CA is
// written beside the file (name + ".ca", root-only, removed with it).
func writeClientFile(dir string, conn func(caFile string) string, remote *RemoteServer, user, password string) (string, error) {
	f, err := os.CreateTemp(dir, ".dbq-*.cnf")
	if err != nil {
		return "", err
	}
	name := f.Name()
	fail := func(err error) (string, error) {
		f.Close()
		removeClientFile(name)
		return "", err
	}
	if err := f.Chmod(0o600); err != nil {
		return fail(err)
	}
	caFile := ""
	if remote != nil && remote.TLS && remote.CACert != "" {
		caFile = name + ".ca"
		if err := os.WriteFile(caFile, []byte(remote.CACert), 0o600); err != nil {
			return fail(err)
		}
	}
	body := "[client]\nuser=" + user + "\npassword=" + password + "\n" + conn(caFile)
	body += "default-character-set=utf8mb4\n"
	if _, err := f.WriteString(body); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		removeClientFile(name)
		return "", err
	}
	return name, nil
}

func (m *MariaDBAdmin) ScopedAccount(ctx context.Context, database string, readOnly bool) (*ScopedAccount, error) {
	privileges := scopedReadPrivileges
	if !readOnly {
		privileges = scopedWritePrivileges
	}
	return m.scopedAccount(ctx, database, privileges)
}

// scopedAccount makes a cgq_ account granted privileges on database.
func (m *MariaDBAdmin) scopedAccount(ctx context.Context, database, privileges string) (*ScopedAccount, error) {
	if !ValidDatabaseName(database) {
		return nil, ErrInvalidDatabase
	}
	user, password, err := newScopedCredentials()
	if err != nil {
		return nil, err
	}
	if err := m.Exec(ctx, "CREATE USER ?@'localhost' IDENTIFIED BY ? WITH MAX_USER_CONNECTIONS 4", user, password); err != nil {
		return nil, fmt.Errorf("create scoped database user: %w", err)
	}
	drop := func() error {
		// Its own deadline: a cancelled request must still clean up.
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		return m.Exec(dctx, "DROP USER IF EXISTS ?@'localhost'", user)
	}
	// The database name is validated above and is ours, never bound.
	if err := m.Exec(ctx, fmt.Sprintf("GRANT %s ON `%s`.* TO ?@'localhost'", privileges, GrantPattern(database)), user); err != nil {
		return nil, errors.Join(fmt.Errorf("grant scoped database user: %w", err), drop())
	}
	file, err := m.writeFile(user, password)
	if err != nil {
		return nil, errors.Join(err, drop())
	}
	return &ScopedAccount{User: user, Password: password, DefaultsFile: file, cleanup: func() error {
		return errors.Join(removeClientFile(file), drop())
	}}, nil
}

// DumpArgs is the mariadb-dump argv (after the binary) for database and,
// optionally, some of its tables.
func DumpArgs(database string, tables ...string) []string {
	return append([]string{"--protocol=socket", "--single-transaction", database}, tables...)
}

// dumpArgs is DumpArgs for this server (the protocol).
func (m *MariaDBAdmin) dumpArgs(database string, tables ...string) []string {
	return append([]string{m.protocolArg(), "--single-transaction", database}, tables...)
}

// ImportArgs is the mariadb client argv for a sandboxed import (R-0.1):
// --sandbox refuses client commands such as `system`, which would otherwise
// run as root, and the scoped account keeps the SQL inside one database.
// --local-infile=0 refuses LOAD DATA LOCAL INFILE: the client runs as root
// and would otherwise send the server any file root can read (SR-T1).
// The defaults file must come first.
func ImportArgs(defaultsFile, database string) []string {
	return []string{DefaultsArg(defaultsFile), "--sandbox", "--local-infile=0", "--protocol=socket", "--one-database", database}
}

// importArgs is ImportArgs for this server (the protocol).
func (m *MariaDBAdmin) importArgs(defaultsFile, database string) []string {
	return []string{DefaultsArg(defaultsFile), "--sandbox", "--local-infile=0", m.protocolArg(), "--one-database", database}
}

// DefaultsArg makes the client read file and no other option file
// (writeDefaultsFile): it must be the first argument.
func DefaultsArg(file string) string { return "--defaults-file=" + file }

func (m *MariaDBAdmin) Dump(ctx context.Context, database string, w io.Writer, tables ...string) error {
	if !ValidDatabaseName(database) {
		return ErrInvalidDatabase
	}
	for _, t := range tables {
		if !ValidTableName(t) {
			return fmt.Errorf("invalid table name %q", t)
		}
	}
	if m.Remote != nil {
		acct, err := m.adminClientFile()
		if err != nil {
			return err
		}
		args := append([]string{DefaultsArg(acct.DefaultsFile)}, m.dumpArgs(database, tables...)...)
		_, err = m.Tools.Run(ctx, Cmd{Name: "mariadb-dump", Args: args, Stdout: w, Timeout: DataTimeout})
		return errors.Join(err, acct.Close())
	}
	_, err := m.Tools.Run(ctx, Cmd{Name: "mariadb-dump", Args: DumpArgs(database, tables...), Stdout: w, Timeout: DataTimeout})
	return err
}

// Credentials are a database user's own name and password, as a person
// gave them to Quarry. They reach the client in a root-only defaults
// file, never in an argument.
type Credentials struct {
	User, Password string
}

// valid refuses a name that is not a site's and a password a defaults file
// cannot hold.
func (c Credentials) valid() error {
	if !site.ValidDBName(c.User) {
		return fmt.Errorf("%q is not a site's database user", c.User)
	}
	if c.Password == "" || strings.ContainsAny(c.Password, "\x00\r\n") {
		return errors.New("the password cannot go in a defaults file")
	}
	return nil
}

// DumpAs writes a defaults file with as's credentials for this dump alone.
func (m *MariaDBAdmin) DumpAs(ctx context.Context, as Credentials, database string, w io.Writer, tables ...string) error {
	if !ValidDatabaseName(database) {
		return ErrInvalidDatabase
	}
	for _, t := range tables {
		if !ValidTableName(t) {
			return fmt.Errorf("invalid table name %q", t)
		}
	}
	acct, err := m.loginAccount(as)
	if err != nil {
		return err
	}
	args := append([]string{DefaultsArg(acct.DefaultsFile)}, m.dumpArgs(database, tables...)...)
	_, err = m.Tools.Run(ctx, Cmd{Name: "mariadb-dump", Args: args, Stdout: w, Timeout: DataTimeout})
	return errors.Join(err, acct.Close())
}

// plainOption is a value an option file reads as written.
var plainOption = regexp.MustCompile(`^[A-Za-z0-9._~@%+=-]+$`)

// OptionValue is s as the value of an option file's line: as it is when it
// holds nothing the file treats specially, otherwise in double quotes with
// `\` and `"` escaped. Unquoted, a `#` starts a comment and spaces at the
// ends are dropped; inside quotes the client keeps both and reads `\\`
// and `\"` back as `\` and `"`. A value with a line break cannot be
// written (Credentials.valid refuses it).
func OptionValue(s string) string {
	if plainOption.MatchString(s) {
		return s
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// loginAccount is a signed-in user's own credentials in a defaults file:
// nothing is created or granted, and Close removes only the file.
func (m *MariaDBAdmin) loginAccount(as Credentials) (*ScopedAccount, error) {
	if err := as.valid(); err != nil {
		return nil, err
	}
	file, err := m.writeFile(as.User, OptionValue(as.Password))
	if err != nil {
		return nil, err
	}
	return &ScopedAccount{User: as.User, DefaultsFile: file, cleanup: func() error { return removeClientFile(file) }}, nil
}

// ImportAs is the account an import runs as (SR-T7).
type ImportAs struct {
	// Login is a database user signed in to Quarry with its own
	// password: the import runs as it and every DEFINER clause names it.
	// What the dump may change is what the server lets that user change.
	Login *Credentials
	// Site is an account of the site with all privileges on the database
	// (ops/db chooses it from its record): the import runs as it, and
	// every DEFINER clause names it, so the triggers, views and routines
	// of the dump are the site's and keep working. Empty: a scoped account, for the agent's scratch databases.
	Site string
	// Triggers grants the scoped account TRIGGER too, with DEFINER clauses
	// rewritten to CURRENT_USER: the restore test, whose database (and
	// every object in it) is dropped with the account. Without it a
	// trigger in the dump fails the import, as before (a staged migration,
	// whose tables move into the site's database).
	Triggers bool
}

// definer is the DEFINER every clause of the dump is rewritten to.
func (as ImportAs) definer() string {
	if as.Login != nil {
		return "`" + as.Login.User + "`@`localhost`"
	}
	if as.Site != "" {
		return "`" + as.Site + "`@`localhost`"
	}
	return "CURRENT_USER"
}

func (m *MariaDBAdmin) Import(ctx context.Context, database string, r io.Reader, as ImportAs) error {
	if !ValidDatabaseName(database) {
		return ErrInvalidDatabase
	}
	acct, definer, err := m.importActor(ctx, database, as)
	if err != nil {
		return err
	}
	pr, pw := io.Pipe()
	rewrote := make(chan error, 1)
	go func() {
		_, err := RewriteDefiners(pw, r, definer)
		pw.CloseWithError(err)
		rewrote <- err
	}()
	_, err = m.Tools.Run(ctx, Cmd{Name: "mariadb", Args: m.importArgs(acct.DefaultsFile, database), Stdin: pr, Stdout: io.Discard, Timeout: DataTimeout})
	// The client may stop reading first (it failed): unblock the filter.
	pr.CloseWithError(errImportEnded)
	ferr := <-rewrote
	if errors.Is(ferr, errImportEnded) || errors.Is(ferr, io.ErrClosedPipe) {
		ferr = nil
	}
	return errors.Join(err, ferr, acct.Close())
}

// importActor is the account an import runs as and the DEFINER its clauses
// are rewritten to. The account is never the server's administrator: a dump
// is tenant content, so it runs with no more than one database's privileges,
// enforced by the server's own grants (SR-T7).
//
//   - A signed-in database user (Login) runs as itself, scoped by its own
//     grants; its DEFINER names it.
//   - On the box's own server a site database import runs as the site's own
//     account (borrow), so the site's triggers and views keep their owner.
//   - On an external server borrowing needs mysql.global_priv, which a
//     managed server rarely grants, so the import runs as a throwaway account
//     scoped to exactly that one database (GRANT … ON `db`.* only, never the
//     administrator), its DEFINER clauses rewritten to CURRENT_USER. The
//     administrator is used only to create and drop that account, never to
//     run the dump.
func (m *MariaDBAdmin) importActor(ctx context.Context, database string, as ImportAs) (*ScopedAccount, string, error) {
	switch {
	case as.Login != nil:
		acct, err := m.loginAccount(*as.Login)
		return acct, m.definerOf(as), err
	case as.Site != "" && m.Remote != nil:
		// No admin: a scoped account on the external server, with TRIGGER so
		// the dump's triggers and views import (owned by this account; the
		// box's own server keeps the site's own owner through borrow).
		acct, err := m.scopedAccount(ctx, database, scopedWritePrivileges+", TRIGGER")
		return acct, "CURRENT_USER", err
	case as.Site != "":
		acct, err := m.borrow(ctx, as.Site)
		return acct, m.definerOf(as), err
	case as.Triggers:
		acct, err := m.scopedAccount(ctx, database, scopedWritePrivileges+", TRIGGER")
		return acct, m.definerOf(as), err
	default:
		acct, err := m.ScopedAccount(ctx, database, false)
		return acct, m.definerOf(as), err
	}
}

// errImportEnded closes the filter's pipe once the client has exited.
var errImportEnded = errors.New("the import ended")

// borrowablePlugins are the authentications borrow can put back exactly:
// their stored string is the whole credential.
var borrowablePlugins = map[string]bool{"mysql_native_password": true, "ed25519": true}

// borrow lets the agent sign in as a site's own account for one import,
// without its password and without SET USER: the account gets a second,
// random password beside its own (IDENTIFIED VIA <its plugin> USING <its
// hash> OR mysql_native_password USING PASSWORD(<random>)), in a root-only
// defaults file; Close puts back its own authentication alone. The site
// keeps signing in with its password throughout. One borrow per account at
// a time. A borrow cut short (the agent died) is undone by the startup
// sweep (ops/db SPEC §8).
func (m *MariaDBAdmin) borrow(ctx context.Context, user string) (*ScopedAccount, error) {
	if !site.ValidDBName(user) {
		return nil, fmt.Errorf("%q is not a site's account", user)
	}
	lock := m.borrowLock(user)
	lock.Lock()
	release := func(err error) (*ScopedAccount, error) { lock.Unlock(); return nil, err }
	plugin, auth, err := m.authOf(ctx, user)
	if err != nil {
		return release(err)
	}
	_, password, err := newScopedCredentials()
	if err != nil {
		return release(err)
	}
	if err := m.Exec(ctx, "ALTER USER ?@'localhost' IDENTIFIED VIA "+plugin+" USING ? OR mysql_native_password USING PASSWORD(?)", user, auth, password); err != nil {
		return release(fmt.Errorf("let the agent sign in as %s: %w", user, err))
	}
	restore := func() error {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		return m.Exec(dctx, "ALTER USER ?@'localhost' IDENTIFIED VIA "+plugin+" USING ?", user, auth)
	}
	file, err := m.writeFile(user, password)
	if err != nil {
		err = errors.Join(err, restore())
		return release(err)
	}
	return &ScopedAccount{User: user, Password: password, DefaultsFile: file, cleanup: func() error {
		defer lock.Unlock()
		return errors.Join(removeClientFile(file), restore())
	}}, nil
}

// authOf reads an account's own (first) authentication: its plugin and
// stored string, which borrow puts back.
func (m *MariaDBAdmin) authOf(ctx context.Context, user string) (plugin, auth string, err error) {
	const q = "SELECT JSON_VALUE(Priv, ?) FROM mysql.global_priv WHERE User = ? AND Host = 'localhost'"
	p, err := m.QueryStrings(ctx, q, "$.plugin", user)
	if err != nil {
		return "", "", fmt.Errorf("read %s's authentication: %w", user, err)
	}
	if len(p) != 1 {
		return "", "", fmt.Errorf("the site's database account %s does not exist", user)
	}
	a, err := m.QueryStrings(ctx, q, "$.authentication_string", user)
	if err != nil || len(a) != 1 {
		return "", "", fmt.Errorf("read %s's authentication: %v", user, err)
	}
	if !borrowablePlugins[p[0]] {
		return "", "", fmt.Errorf("the account %s signs in with %q, which an import cannot borrow", user, p[0])
	}
	return p[0], a[0], nil
}

func (m *MariaDBAdmin) Unborrow(ctx context.Context, tenant func(user string) bool) error {
	if m.Remote != nil {
		return nil // nothing is borrowed on an external server
	}
	users, err := m.QueryStrings(ctx, "SELECT User FROM mysql.global_priv WHERE Host = 'localhost' AND JSON_LENGTH(Priv, '$.auth_or') > 1")
	if err != nil {
		return fmt.Errorf("list the site accounts an import left signed in: %w", err)
	}
	var errs []error
	for _, u := range users {
		if !site.ValidDBName(u) || tenant == nil || !tenant(u) {
			continue
		}
		// The first authentication is the account's own: borrow added the
		// second.
		plugin, auth, err := m.authOf(ctx, u)
		if err == nil {
			err = m.Exec(ctx, "ALTER USER ?@'localhost' IDENTIFIED VIA "+plugin+" USING ?", u, auth)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("put back %s's own authentication: %w", u, err))
		}
	}
	return errors.Join(errs...)
}

func (m *MariaDBAdmin) borrowLock(user string) *sync.Mutex {
	m.borrowMu.Lock()
	defer m.borrowMu.Unlock()
	if m.borrows == nil {
		m.borrows = map[string]*sync.Mutex{}
	}
	l, ok := m.borrows[user]
	if !ok {
		l = &sync.Mutex{}
		m.borrows[user] = l
	}
	return l
}

var _ MariaDB = (*MariaDBAdmin)(nil)
