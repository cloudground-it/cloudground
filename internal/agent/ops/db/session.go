package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
	"github.com/go-sql-driver/mysql"
)

// Limits bound one statement's result.
type Limits struct {
	MaxRows  int
	MaxBytes int // cell data
}

// ConsoleLimits are the console's (SPEC §3).
var ConsoleLimits = Limits{MaxRows: 5000, MaxBytes: 8 << 20}

// DefaultConsoleTimeout is how long a console statement may run.
const DefaultConsoleTimeout = 60 * time.Second

// DialSpec is a connection to open as a scoped account, or (Account nil)
// as a database user with its own password (Login).
type DialSpec struct {
	Account  *host.ScopedAccount
	Login    Login
	Database string
	// StatementTimeout becomes the session's max_statement_time.
	StatementTimeout time.Duration
}

// Conn is one connection as a scoped account.
type Conn interface {
	// ID is the server's id for the connection (CONNECTION_ID()), what
	// KILL takes.
	ID() int64
	// Query runs one statement and renders its result as text.
	Query(ctx context.Context, lim Limits, stmt string, args ...any) (rpc.DBQueryResult, error)
	// Each runs one statement and hands fn every row as it arrives, the
	// cells aliasing the driver's buffer (valid until fn returns; nil is
	// NULL). fn returning ErrStop ends the result early without an error.
	Each(ctx context.Context, stmt string, args []any, fn func(cols []string, row [][]byte) error) error
	// Exec runs one statement that returns no rows and reports the rows it
	// matched.
	Exec(ctx context.Context, stmt string, args ...any) (int64, error)
	Close() error
}

// ErrStop, returned by an Each callback, ends the result early.
var ErrStop = errors.New("stop reading rows")

// Session is a scoped account and one connection as it. Close drops both.
type Session struct {
	o        *Ops
	acct     *host.ScopedAccount
	conn     Conn
	timeout  time.Duration
	database string
}

// OpenSession opens a session on one of the site's databases ("" is its
// primary; any name the record does not give the site is refused, SPEC §2)
// in which every call may last timeout (0 means the console's). Read-only
// grants SELECT and SHOW VIEW only. The caller closes it.
func (o *Ops) OpenSession(ctx context.Context, s state.Site, database string, readOnly bool, timeout time.Duration) (*Session, error) {
	database, err := o.SiteDatabase(ctx, s, database)
	if err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = o.consoleTimeout()
	}
	return o.session(ctx, database, readOnly, timeout)
}

// session opens a session on database: read-only grants SELECT and SHOW VIEW
// only.
func (o *Ops) session(ctx context.Context, database string, readOnly bool, timeout time.Duration) (*Session, error) {
	acct, err := o.MariaDB.ScopedAccount(ctx, database, readOnly)
	if err != nil {
		return nil, err
	}
	dial := o.Dial
	if dial == nil {
		dial = driverDial(o.Socket)
	}
	conn, err := dial(ctx, DialSpec{Account: acct, Database: database, StatementTimeout: statementTime(timeout)})
	if err != nil {
		return nil, errors.Join(fmt.Errorf("connect to %s: %w", database, err), acct.Close())
	}
	return &Session{o: o, acct: acct, conn: conn, timeout: timeout, database: database}, nil
}

// Login is a database user's own name and password: a person signed in to
// Quarry with them (Fase 9b).
type Login struct {
	User, Password string
}

// LoginSession opens a session signed in as login itself, on database ("":
// none, for what lists the user's databases): no account is made or
// granted, and what the user may see and change is what the server grants
// it. The name must be one of a site's (the caller checks whose); the
// connection must sign in as exactly user@localhost, never as an anonymous
// account the server matched instead (ErrLoginRefused otherwise).
func (o *Ops) LoginSession(ctx context.Context, login Login, database string, timeout time.Duration) (*Session, error) {
	if !site.ValidDBName(login.User) || login.Password == "" {
		return nil, ErrLoginRefused
	}
	if database != "" && !host.ValidDatabaseName(database) {
		return nil, agentrpc.Invalid("invalid database name")
	}
	if timeout <= 0 {
		timeout = o.consoleTimeout()
	}
	dial := o.Dial
	if dial == nil {
		dial = driverDial(o.Socket)
	}
	conn, err := dial(ctx, DialSpec{Login: login, Database: database, StatementTimeout: statementTime(timeout)})
	if err != nil {
		if AccessDenied(err) {
			return nil, ErrLoginRefused
		}
		return nil, fmt.Errorf("connect as %s: %w", login.User, err)
	}
	s := &Session{o: o, conn: conn, timeout: timeout, database: database}
	var who string
	err = s.Each(ctx, "SELECT CURRENT_USER()", nil, func(_ []string, row [][]byte) error {
		if len(row) == 1 {
			who = string(row[0])
		}
		return nil
	})
	if err != nil || who != login.User+"@localhost" {
		s.Close()
		if err != nil && !AccessDenied(err) {
			return nil, fmt.Errorf("check the signed-in user: %w", err)
		}
		return nil, ErrLoginRefused
	}
	return s, nil
}

// ErrLoginRefused is a sign-in the server refused (a wrong name or
// password), or one that did not sign in as the user it named.
var ErrLoginRefused = errors.New("the database refused the user name or password")

// AccessDenied reports whether err is the server refusing a sign-in or a
// database: ER_ACCESS_DENIED_ERROR (1045), ER_DBACCESS_DENIED_ERROR (1044),
// ER_ACCESS_DENIED_NO_PASSWORD_ERROR (1698).
func AccessDenied(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && (me.Number == 1045 || me.Number == 1044 || me.Number == 1698)
}

// Database is the database the session is granted on.
func (s *Session) Database() string { return s.database }

// statementTime is the server-side limit for a call that may last timeout:
// a little less, so the server stops the statement itself and says so.
func statementTime(timeout time.Duration) time.Duration {
	if timeout > 10*time.Second {
		return timeout - 5*time.Second
	}
	return timeout
}

// Query runs one statement within the session's time. When the time runs
// out, or the caller goes away, the connection is killed as root: dropping
// a scoped account does not end the statement it is running.
func (s *Session) Query(ctx context.Context, lim Limits, stmt string, args ...any) (rpc.DBQueryResult, error) {
	var res rpc.DBQueryResult
	var err error
	s.guard(ctx, func(ctx context.Context) { res, err = s.conn.Query(ctx, lim, stmt, args...) })
	return res, err
}

// Each streams one statement's rows to fn within the session's time, with
// the same kill as Query.
func (s *Session) Each(ctx context.Context, stmt string, args []any, fn func(cols []string, row [][]byte) error) error {
	var err error
	s.guard(ctx, func(ctx context.Context) { err = s.conn.Each(ctx, stmt, args, fn) })
	return err
}

// Exec runs one statement that returns no rows within the session's time,
// with the same kill as Query, and returns the rows it matched.
func (s *Session) Exec(ctx context.Context, stmt string, args ...any) (int64, error) {
	var n int64
	var err error
	s.guard(ctx, func(ctx context.Context) { n, err = s.conn.Exec(ctx, stmt, args...) })
	return n, err
}

// guard runs call under the session's deadline. When the deadline or the
// caller ends it first, the connection is killed as root, and guard waits
// for the kill before returning, so the account is never dropped under a
// statement that still runs.
func (s *Session) guard(ctx context.Context, call func(context.Context)) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	killed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		s.kill()
		close(killed)
	})
	call(ctx)
	if stop() {
		// The statement saw the deadline and returned before the
		// AfterFunc started; stop() then cancelled it, so kill here or the
		// statement may still be running on the server.
		if ctx.Err() != nil {
			s.kill()
		}
		return
	}
	<-killed
}

// ServerMessage is what a person is shown for a failed statement: the
// server's own error, or that the time ran out. Anything else is logged and
// shown as a plain failure.
func (s *Session) ServerMessage(err error) string { return s.o.serverMessage(err, s.timeout) }

func (s *Session) kill() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.o.MariaDB.Exec(ctx, "KILL ?", s.conn.ID()); err != nil {
		// Already gone (the statement ended meanwhile) is the usual reason.
		s.o.log().Debug("kill a console connection", "id", s.conn.ID(), "err", err)
	}
}

// Close closes the connection and drops the account (a signed-in user's
// session has none: only the connection goes).
func (s *Session) Close() error {
	if s.acct == nil {
		return s.conn.Close()
	}
	return errors.Join(s.conn.Close(), s.acct.Close())
}

// scopedConfig is the driver configuration of a session. AllowAllFiles
// stays false and nothing is registered with the driver: a LOAD DATA LOCAL
// INFILE in the console reads no file of this machine (SR-T1).
func scopedConfig(socket string, d DialSpec) *mysql.Config {
	cfg := mysql.NewConfig()
	if d.Account != nil {
		cfg.User, cfg.Passwd = d.Account.User, d.Account.Password
	} else {
		cfg.User, cfg.Passwd = d.Login.User, d.Login.Password
	}
	cfg.Net, cfg.Addr = "unix", socket
	cfg.DBName = d.Database
	cfg.Timeout = 10 * time.Second
	// An UPDATE reports the rows it matched, not only those it changed:
	// Quarry checks that an edit by primary key found its row.
	cfg.ClientFoundRows = true
	cfg.AllowAllFiles = false
	return cfg
}

// driverDial connects with go-sql-driver over the socket. Multi-statements
// stay off (the driver's default): one call is one statement.
func driverDial(socket string) func(ctx context.Context, d DialSpec) (Conn, error) {
	return func(ctx context.Context, d DialSpec) (Conn, error) {
		connector, err := mysql.NewConnector(scopedConfig(socket, d))
		if err != nil {
			return nil, err
		}
		db := sql.OpenDB(connector)
		db.SetMaxOpenConns(1)
		c, err := db.Conn(ctx)
		if err != nil {
			db.Close()
			return nil, err
		}
		dc := &driverConn{db: db, c: c}
		fail := func(err error) (Conn, error) {
			dc.Close()
			return nil, err
		}
		if err := c.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&dc.id); err != nil {
			return fail(err)
		}
		if d.StatementTimeout > 0 {
			secs := fmt.Sprintf("%.3f", d.StatementTimeout.Seconds())
			if _, err := c.ExecContext(ctx, "SET SESSION max_statement_time = "+secs); err != nil {
				return fail(err)
			}
		}
		return dc, nil
	}
}

type driverConn struct {
	db *sql.DB
	c  *sql.Conn
	id int64
}

func (d *driverConn) ID() int64 { return d.id }

func (d *driverConn) Close() error { return errors.Join(d.c.Close(), d.db.Close()) }

func (d *driverConn) Query(ctx context.Context, lim Limits, stmt string, args ...any) (rpc.DBQueryResult, error) {
	qctx, cancel := context.WithCancel(ctx)
	defer cancel()
	rows, err := d.c.QueryContext(qctx, stmt, args...)
	if err != nil {
		return rpc.DBQueryResult{}, err
	}
	res, err := render(rows, lim)
	if res.Truncated {
		// Closing the rows would read the rest of the result off the wire,
		// all of it; dropping the connection ends it at once.
		cancel()
	}
	rows.Close()
	return res, err
}

func (d *driverConn) Each(ctx context.Context, stmt string, args []any, fn func(cols []string, row [][]byte) error) error {
	qctx, cancel := context.WithCancel(ctx)
	defer cancel()
	rows, err := d.c.QueryContext(qctx, stmt, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	raw := make([]sql.RawBytes, len(cols))
	dest := make([]any, len(cols))
	for i := range raw {
		dest[i] = &raw[i]
	}
	row := make([][]byte, len(cols))
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return err
		}
		for i, v := range raw {
			row[i] = v
		}
		if err := fn(cols, row); err != nil {
			// Ending early: dropping the connection's read ends the result
			// at once instead of reading the rest off the wire.
			cancel()
			if errors.Is(err, ErrStop) {
				return nil
			}
			return err
		}
	}
	return rows.Err()
}

func (d *driverConn) Exec(ctx context.Context, stmt string, args ...any) (int64, error) {
	res, err := d.c.ExecContext(ctx, stmt, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// rowSource is what render reads: *sql.Rows, or a stand-in in tests.
type rowSource interface {
	Columns() ([]string, error)
	Next() bool
	Scan(dest ...any) error
	Err() error
}

// render turns a result into the console's text table: NULL as "NULL",
// every value as text, cut at the limits.
func render(rows rowSource, lim Limits) (rpc.DBQueryResult, error) {
	cols, err := rows.Columns()
	if err != nil {
		return rpc.DBQueryResult{}, err
	}
	res := rpc.DBQueryResult{}
	if len(cols) == 0 {
		res.Message = "OK"
		return res, rows.Err()
	}
	res.Columns = cols
	raw := make([]sql.RawBytes, len(cols))
	dest := make([]any, len(cols))
	for i := range raw {
		dest[i] = &raw[i]
	}
	size := 0
	for rows.Next() {
		if lim.MaxRows > 0 && len(res.Rows) >= lim.MaxRows {
			res.Truncated = true
			res.Message = fmt.Sprintf("showing the first %d rows", lim.MaxRows)
			break
		}
		if err := rows.Scan(dest...); err != nil {
			return rpc.DBQueryResult{}, err
		}
		row := make([]string, len(cols))
		n := 0
		for i, v := range raw {
			if v == nil {
				row[i] = "NULL"
			} else {
				row[i] = string(v)
			}
			n += len(row[i])
		}
		if lim.MaxBytes > 0 && size+n > lim.MaxBytes {
			res.Truncated = true
			res.Message = fmt.Sprintf("showing the first %d rows (%d MiB of data)", len(res.Rows), lim.MaxBytes>>20)
			break
		}
		size += n
		res.Rows = append(res.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return rpc.DBQueryResult{}, err
	}
	if len(res.Rows) == 0 && res.Message == "" {
		res.Message = "OK (no rows)"
	}
	return res, nil
}
