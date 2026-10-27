package host

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresAdmin administers an external PostgreSQL server (host SPEC,
// postgres): the driver (pgx) as the administrator for statements, pg_dump
// for dumps and psql for restores, both run by systemd-run as a user of their
// own with nothing of this machine to read (a dump is the tenant's data, and
// psql runs the meta-commands a dump holds).
type PostgresAdmin struct {
	Server  RemoteServer
	WorkDir string // root's scratch space: credential files, removed after each run
	Tools   Exec   // runs systemd-run

	mu    sync.Mutex
	pools map[string]*pgxpool.Pool
}

// NewPostgres is the adapter of an external PostgreSQL server. Nothing
// connects until first use.
func NewPostgres(r RemoteServer, workDir string, e Exec) *PostgresAdmin {
	return &PostgresAdmin{Server: r, WorkDir: workDir, Tools: e}
}

// QuotePGIdent is s as an identifier.
func QuotePGIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// QuotePGLiteral is s as a string literal. A value with a NUL or a backslash
// is refused by its caller (a password may have neither); the doubling of the
// quote is what keeps the rest inside the literal.
func QuotePGLiteral(s string) string { return `'` + strings.ReplaceAll(s, `'`, `''`) + `'` }

// pool opens (once) a pool on database ("" is the maintenance database,
// postgres).
func (p *PostgresAdmin) pool(ctx context.Context, database string) (*pgxpool.Pool, error) {
	if database == "" {
		database = "postgres"
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if pl, ok := p.pools[database]; ok {
		return pl, nil
	}
	// sslmode=require: TLS or nothing, no fallback to plain text; the
	// certificate is then verified by the configuration set below.
	cfg, err := pgxpool.ParseConfig(fmt.Sprintf("host=%s port=%d user=%s dbname=%s sslmode=require connect_timeout=10",
		p.Server.Host, p.Server.Port, p.Server.AdminUser, database))
	if err != nil {
		return nil, errors.New("the server's address cannot be used")
	}
	cfg.ConnConfig.Password = p.Server.AdminPassword
	t, err := p.Server.TLSConfig()
	if err != nil {
		return nil, err
	}
	cfg.ConnConfig.TLSConfig = t
	// DDL takes no bound values: statements are sent as text, the values
	// quoted by QuotePGLiteral.
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	cfg.MaxConns = 4
	cfg.MaxConnIdleTime = time.Minute
	pl, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if p.pools == nil {
		p.pools = map[string]*pgxpool.Pool{}
	}
	p.pools[database] = pl
	return pl, nil
}

// Exec runs statements on database ("" is postgres).
func (p *PostgresAdmin) Exec(ctx context.Context, database, sql string) error {
	pl, err := p.pool(ctx, database)
	if err != nil {
		return err
	}
	_, err = pl.Exec(ctx, sql)
	return err
}

// QueryStrings returns the first column of every row as text.
func (p *PostgresAdmin) QueryStrings(ctx context.Context, database, sql string, args ...any) ([]string, error) {
	pl, err := p.pool(ctx, database)
	if err != nil {
		return nil, err
	}
	rows, err := pl.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		if len(vals) > 0 {
			out = append(out, fmt.Sprint(vals[0]))
		}
	}
	return out, rows.Err()
}

// Version is the server's version line.
func (p *PostgresAdmin) Version(ctx context.Context) (string, error) {
	v, err := p.QueryStrings(ctx, "", "SELECT version()")
	if err != nil || len(v) != 1 {
		return "", err
	}
	return v[0], nil
}

// Close releases the pools.
func (p *PostgresAdmin) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, pl := range p.pools {
		pl.Close()
		delete(p.pools, k)
	}
	return nil
}

// ValidPGName reports whether name is a database or role name the agent puts
// in SQL: a site's (lowercase letters, digits, underscore) or the agent's own
// (cgo_, cgq_, cgv_ prefixes).
func ValidPGName(name string) bool {
	if name == "" || len(name) > 63 {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// PGClient is how a client tool signs in: the user and password reach the
// tool in a credential file of the service, never in an argument.
type PGClient struct {
	User, Password string
	// Role, when set, is the role the session acts as (PGOPTIONS role=).
	Role string
}

const pgScript = `export PGPASSWORD="$(cat "$CREDENTIALS_DIRECTORY/pw")"; export PGSSLROOTCERT="$CREDENTIALS_DIRECTORY/ca"; exec "$@"`

// sandboxArgs is the systemd-run argv (after the binary) that runs a client
// tool as a throwaway user with no access to this machine beyond the
// network: read-only file system, no home, a private /tmp, no capabilities,
// the two credentials as service credentials.
func (p *PostgresAdmin) sandboxArgs(c PGClient, database, pwFile, caFile string, tool []string) []string {
	env := []string{
		"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8",
		"PGHOST=" + p.Server.Host, "PGPORT=" + strconv.Itoa(p.Server.Port), "PGUSER=" + c.User, "PGDATABASE=" + database,
		"PGSSLMODE=verify-full", "PGCONNECT_TIMEOUT=10", "PGAPPNAME=cloudground", "PSQLRC=/dev/null",
	}
	if c.Role != "" {
		env = append(env, "PGOPTIONS=-c role="+c.Role)
	}
	args := []string{"--quiet", "--collect", "--wait", "--pipe", "--service-type=exec",
		"-p", "DynamicUser=yes", "-p", "ProtectSystem=strict", "-p", "ProtectHome=yes", "-p", "PrivateTmp=yes",
		"-p", "PrivateDevices=yes", "-p", "NoNewPrivileges=yes", "-p", "ProtectProc=invisible", "-p", "ProcSubset=pid",
		"-p", "RestrictAddressFamilies=AF_INET AF_INET6", "-p", "CapabilityBoundingSet=", "-p", "MemoryMax=1G",
		"-p", "LoadCredential=pw:" + pwFile, "-p", "LoadCredential=ca:" + caFile,
		"-p", "RuntimeMaxSec=" + strconv.Itoa(int(DataTimeout/time.Second))}
	for _, e := range env {
		args = append(args, "--setenv="+e)
	}
	return append(append(args, "--", "/bin/sh", "-c", pgScript, "cg-pg"), tool...)
}

// creds writes the password and the CA the tool reads, in a directory of
// its own; the returned function removes them.
func (p *PostgresAdmin) creds(password string) (pwFile, caFile string, cleanup func(), err error) {
	dir, err := os.MkdirTemp(p.WorkDir, ".pg-*")
	if err != nil {
		return "", "", nil, err
	}
	cleanup = func() { os.RemoveAll(dir) }
	pwFile, caFile = filepath.Join(dir, "pw"), filepath.Join(dir, "ca")
	if err := os.WriteFile(pwFile, []byte(password), 0o600); err != nil {
		cleanup()
		return "", "", nil, err
	}
	ca := p.Server.CACert
	if ca == "" {
		b, err := os.ReadFile(SystemCABundle)
		if err != nil {
			cleanup()
			return "", "", nil, fmt.Errorf("read the system's certificate authorities: %w", err)
		}
		ca = string(b)
	}
	if err := os.WriteFile(caFile, []byte(ca), 0o600); err != nil {
		cleanup()
		return "", "", nil, err
	}
	return pwFile, caFile, cleanup, nil
}

// run runs a client tool as c in the sandbox.
func (p *PostgresAdmin) run(ctx context.Context, c PGClient, database string, stdin io.Reader, stdout io.Writer, tool ...string) error {
	pw, ca, cleanup, err := p.creds(c.Password)
	if err != nil {
		return err
	}
	defer cleanup()
	cmd := Cmd{Name: "systemd-run", Args: p.sandboxArgs(c, database, pw, ca, tool), Stdin: stdin, Stdout: stdout, Timeout: DataTimeout,
		Env: []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"}}
	_, err = p.Tools.Run(ctx, cmd)
	return err
}

// Dump streams pg_dump of database (plain format, no owners or privileges:
// a restore makes everything the database's own owner's) into w, as the
// administrator.
func (p *PostgresAdmin) Dump(ctx context.Context, database string, w io.Writer) error {
	if !ValidPGName(database) {
		return ErrInvalidDatabase
	}
	return p.run(ctx, PGClient{User: p.Server.AdminUser, Password: p.Server.AdminPassword}, database, nil, w,
		"pg_dump", "--format=plain", "--no-owner", "--no-acl", "--encoding=UTF8")
}

// scopedLogin makes a short-lived login role that may act as role, for a
// restore: the tool never holds the administrator's password.
func (p *PostgresAdmin) scopedLogin(ctx context.Context, role string) (user, password string, drop func(), err error) {
	suffix := make([]byte, 8)
	pw := make([]byte, 24)
	if _, err := rand.Read(suffix); err != nil {
		return "", "", nil, err
	}
	if _, err := rand.Read(pw); err != nil {
		return "", "", nil, err
	}
	user, password = "cgq_"+hex.EncodeToString(suffix), base64.RawURLEncoding.EncodeToString(pw)
	if err := p.Exec(ctx, "", "CREATE ROLE "+QuotePGIdent(user)+" LOGIN PASSWORD "+QuotePGLiteral(password)+" CONNECTION LIMIT 2"); err != nil {
		return "", "", nil, fmt.Errorf("create a scoped database user: %w", err)
	}
	drop = func() {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = p.Exec(dctx, "", "DROP ROLE IF EXISTS "+QuotePGIdent(user))
	}
	if err := p.Exec(ctx, "", "GRANT "+QuotePGIdent(role)+" TO "+QuotePGIdent(user)); err != nil {
		drop()
		return "", "", nil, fmt.Errorf("let the scoped user act as %s: %w", role, err)
	}
	return user, password, drop, nil
}

// Restore feeds a dump to psql in database, as a scoped login that acts as
// role (the database's owner), stopping at the first error and in one
// transaction.
func (p *PostgresAdmin) Restore(ctx context.Context, database, role string, r io.Reader) error {
	if !ValidPGName(database) || !ValidPGName(role) {
		return ErrInvalidDatabase
	}
	user, pw, drop, err := p.scopedLogin(ctx, role)
	if err != nil {
		return err
	}
	defer drop()
	return p.run(ctx, PGClient{User: user, Password: pw, Role: role}, database, r, io.Discard,
		"psql", "-X", "-q", "--no-psqlrc", "-v", "ON_ERROR_STOP=1", "--single-transaction", "-f", "-")
}
