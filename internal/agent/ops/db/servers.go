package db

import (
	"context"
	"crypto/tls"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// External servers (SPEC §9, #30): a database or an account may be on a
// registered MariaDB or MySQL server instead of the box's own. The record
// says which; every handler finds the adapter of the server from it.

// Target is where a session connects to an external server.
type Target struct {
	Net  string // "tcp"
	Addr string // host:port
	TLS  *tls.Config
}

// Servers gives the agent's connection to each external server. The agent
// holds them in memory (SetDBServers); nil means there is none.
type Servers interface {
	// Maria is the administrator's adapter of MariaDB server id.
	Maria(id int64) (host.MariaDB, error)
	// Target is how a session reaches MariaDB server id.
	Target(id int64) (Target, error)
	// Kind is "mariadb" or "postgres".
	Kind(id int64) (string, error)
	// Postgres is the administrator's adapter of PostgreSQL server id.
	Postgres(id int64) (*host.PostgresAdmin, error)
	// AdminUser is the name of server id's administrator.
	AdminUser(id int64) string
	// BackedUp says whether a site's backup dumps server id's databases.
	BackedUp(id int64) bool
}

// UnknownServer is the answer for a server the agent has no connection for:
// the panel pushes its list and repeats the call (internal/api SPEC §6.5.1).
func UnknownServer(id int64) error {
	return &agentrpc.Error{Code: agentrpc.CodeUnavailable, Message: unknownServerMessage(id)}
}

func unknownServerMessage(id int64) string {
	return "unknown database server " + strconv.FormatInt(id, 10)
}

// mariaOfServer is the adapter of a server: the box's own for 0.
func (o *Ops) mariaOfServer(id int64) (host.MariaDB, error) {
	if id == 0 {
		return o.MariaDB, nil
	}
	if o.Servers == nil {
		return nil, UnknownServer(id)
	}
	return o.Servers.Maria(id)
}

// DatabaseKind is the engine of the server a site's database is on
// (mariadb or postgres) and whether that server is an external one, for the
// site's backup manifest (ops/backup SPEC §4).
func (o *Ops) DatabaseKind(ctx context.Context, database string) (engine string, external bool, err error) {
	id, err := o.serverOf(ctx, database)
	if err != nil || id == 0 {
		return rpc.KindMariaDB, false, err
	}
	_, pg, err := o.pgOfServer(id)
	if err != nil {
		return "", true, err
	}
	if pg {
		return rpc.KindPostgres, true, nil
	}
	return rpc.KindMariaDB, true, nil
}

// serverOf is the server a database is on: a scratch database's is the
// server it was made on (the staged replacement's), a recorded database's the
// record's, any other the box's.
func (o *Ops) serverOf(ctx context.Context, database string) (int64, error) {
	if o.Servers == nil {
		return 0, nil // no external server can exist: the record has nothing to say
	}
	if scratchNameRe.MatchString(database) {
		return o.scratch.get(database), nil
	}
	return o.registry().DatabaseServer(ctx, database)
}

// maria is the adapter of the server database is on.
func (o *Ops) maria(ctx context.Context, database string) (host.MariaDB, error) {
	id, err := o.serverOf(ctx, database)
	if err != nil {
		return nil, err
	}
	return o.mariaOfServer(id)
}

// mariaOfUser is the adapter of the server an account is on.
func (o *Ops) mariaOfUser(ctx context.Context, user string) (host.MariaDB, error) {
	if o.Servers == nil {
		return o.MariaDB, nil
	}
	id, err := o.registry().UserServer(ctx, user)
	if err != nil {
		return nil, err
	}
	return o.mariaOfServer(id)
}

// target is how a session reaches the server database is on: nil Server is
// the box's own socket.
func (o *Ops) target(ctx context.Context, database string) (*Target, error) {
	id, err := o.serverOf(ctx, database)
	if err != nil || id == 0 {
		return nil, err
	}
	if o.Servers == nil {
		return nil, UnknownServer(id)
	}
	t, err := o.Servers.Target(id)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// scratchServers remembers the server each scratch database was made on, so
// the staged replacement's later statements go to the same one.
type scratchServers struct {
	mu sync.Mutex
	m  map[string]int64
}

func (s *scratchServers) set(name string, server int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]int64{}
	}
	if server == 0 {
		delete(s.m, name)
		return
	}
	s.m[name] = server
}

func (s *scratchServers) get(name string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[name]
}

func (s *scratchServers) forget(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, name)
}

// Remotes is the agent's table of external servers: in memory only, replaced
// whole by the panel (SetDBServers). It is the Servers an Ops uses.
type Remotes struct {
	// WorkDir is root's scratch directory (the tools' defaults files).
	WorkDir string
	// Tools runs mariadb-dump and the mariadb client.
	Tools host.Exec
	// Resolve is how a server's name becomes addresses (Test); nil is the
	// system resolver.
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)

	mu       sync.RWMutex
	servers  map[int64]host.RemoteServer
	adapters map[int64]*host.MariaDBAdmin
	pgs      map[int64]*host.PostgresAdmin
}

// Set replaces the table. A server that is gone or changed loses its pool.
func (r *Remotes) Set(_ context.Context, p rpc.SetDBServersParams) (agentrpc.None, error) {
	next := make(map[int64]host.RemoteServer, len(p.Servers))
	for _, s := range p.Servers {
		next[s.ID] = remoteOf(s)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, a := range r.adapters {
		if n, ok := next[id]; !ok || n != r.servers[id] {
			_ = a.Close()
			delete(r.adapters, id)
		}
	}
	for id, a := range r.pgs {
		if n, ok := next[id]; !ok || n != r.servers[id] {
			_ = a.Close()
			delete(r.pgs, id)
		}
	}
	r.servers = next
	return agentrpc.None{}, nil
}

func remoteOf(s rpc.DBServerParams) host.RemoteServer {
	kind := s.Kind
	if kind == "" {
		kind = rpc.KindMariaDB
	}
	return host.RemoteServer{ID: s.ID, Host: s.Host, Port: s.Port, TLS: s.TLS, CACert: s.CACert, AdminUser: s.AdminUser, AdminPassword: s.AdminPassword,
		Kind: kind, BackupDumps: s.BackupDumps}
}

// Kind is the kind of server id.
func (r *Remotes) Kind(id int64) (string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.servers[id]
	if !ok {
		return "", UnknownServer(id)
	}
	return s.Kind, nil
}

// AdminUser is the name of server id's administrator ("" for an unknown one).
func (r *Remotes) AdminUser(id int64) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.servers[id].AdminUser
}

// BackedUp says whether a site's backup dumps server id's databases: always
// for MariaDB, for PostgreSQL unless the provider backs it up.
func (r *Remotes) BackedUp(id int64) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.servers[id]
	return !ok || s.Kind != rpc.KindPostgres || s.BackupDumps
}

// Postgres is the administrator's adapter of PostgreSQL server id.
func (r *Remotes) Postgres(id int64) (*host.PostgresAdmin, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a, ok := r.pgs[id]; ok {
		return a, nil
	}
	s, ok := r.servers[id]
	if !ok {
		return nil, UnknownServer(id)
	}
	if s.Kind != rpc.KindPostgres {
		return nil, agentrpc.Invalid("not a PostgreSQL server")
	}
	if r.pgs == nil {
		r.pgs = map[int64]*host.PostgresAdmin{}
	}
	a := host.NewPostgres(s, r.WorkDir, r.Tools)
	r.pgs[id] = a
	return a, nil
}

// Maria is the administrator's adapter of server id.
func (r *Remotes) Maria(id int64) (host.MariaDB, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a, ok := r.adapters[id]; ok {
		return a, nil
	}
	s, ok := r.servers[id]
	if !ok {
		return nil, UnknownServer(id)
	}
	if s.Kind == rpc.KindPostgres {
		return nil, agentrpc.Invalid(pgUnsupported)
	}
	if r.adapters == nil {
		r.adapters = map[int64]*host.MariaDBAdmin{}
	}
	a := host.NewRemoteMariaDB(s, r.WorkDir, r.Tools)
	r.adapters[id] = a
	return a, nil
}

// Target is how a session reaches server id.
func (r *Remotes) Target(id int64) (Target, error) {
	r.mu.RLock()
	s, ok := r.servers[id]
	r.mu.RUnlock()
	if !ok {
		return Target{}, UnknownServer(id)
	}
	if s.Kind == rpc.KindPostgres {
		return Target{}, agentrpc.Invalid(pgUnsupported)
	}
	t := Target{Net: "tcp", Addr: s.Addr()}
	if s.TLS {
		cfg, err := s.TLSConfig()
		if err != nil {
			return Target{}, err
		}
		t.TLS = cfg
	}
	return t, nil
}

// Test connects with the credentials given and reports the server's version
// when its administrator can create accounts and grant privileges.
func (r *Remotes) Test(ctx context.Context, p rpc.DBServerParams) (rpc.DBServerTestResult, error) {
	if err := r.checkAddrs(ctx, p.Host); err != nil {
		return rpc.DBServerTestResult{}, err
	}
	if p.Kind == rpc.KindPostgres {
		return r.testPG(ctx, p)
	}
	m := host.NewRemoteMariaDB(remoteOf(p), r.WorkDir, r.Tools)
	defer m.Close()
	version, err := m.QueryStrings(ctx, "SELECT VERSION()")
	if err != nil || len(version) != 1 {
		return rpc.DBServerTestResult{}, agentrpc.Invalid("could not connect to %s as %s: %s", p.Host, p.AdminUser, connectError(err))
	}
	grants, err := m.QueryStrings(ctx, "SHOW GRANTS FOR CURRENT_USER()")
	if err != nil {
		return rpc.DBServerTestResult{}, agentrpc.Invalid("could not read the privileges of %s: %s", p.AdminUser, connectError(err))
	}
	all := strings.ToUpper(strings.Join(grants, "\n"))
	if !strings.Contains(all, "GRANT OPTION") || !(strings.Contains(all, "ALL PRIVILEGES") || strings.Contains(all, "CREATE USER")) {
		return rpc.DBServerTestResult{}, agentrpc.Invalid("%s cannot create accounts and grant privileges: it needs ALL PRIVILEGES WITH GRANT OPTION", p.AdminUser)
	}
	return rpc.DBServerTestResult{Version: version[0]}, nil
}

// checkAddrs refuses a server whose name resolves to an address the agent
// never connects (rpc.DBHostRefused: link-local, unspecified, multicast),
// before it sends the administrator's credentials anywhere (H-5-1). The
// API refuses such a literal already; a name is resolved here, where the
// connection is made. A name that does not resolve is left to the
// connection, which says so.
func (r *Remotes) checkAddrs(ctx context.Context, h string) error {
	h = strings.Trim(h, "[]")
	var addrs []netip.Addr
	if a, err := netip.ParseAddr(h); err == nil {
		addrs = []netip.Addr{a}
	} else {
		resolve := r.Resolve
		if resolve == nil {
			resolve = func(ctx context.Context, h string) ([]netip.Addr, error) {
				ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				return net.DefaultResolver.LookupNetIP(ctx, "ip", h)
			}
		}
		addrs, _ = resolve(ctx, h)
	}
	for _, a := range addrs {
		if why := rpc.DBHostRefused(a); why != "" {
			return agentrpc.Invalid("the database server %s resolves to %s, %s: the agent never connects there", h, a.Unmap(), why)
		}
	}
	return nil
}

// connectError is what a person is told of a failed connection: the
// driver's own sentence (it names no password).
func connectError(err error) string {
	if err == nil {
		return "no answer"
	}
	return err.Error()
}

// testPG connects to a PostgreSQL server with the credentials given and
// reports its version when its administrator may create roles and databases.
func (r *Remotes) testPG(ctx context.Context, p rpc.DBServerParams) (rpc.DBServerTestResult, error) {
	pg := host.NewPostgres(remoteOf(p), r.WorkDir, r.Tools)
	defer pg.Close()
	version, err := pg.Version(ctx)
	if err != nil || version == "" {
		return rpc.DBServerTestResult{}, agentrpc.Invalid("could not connect to %s as %s: %s", p.Host, p.AdminUser, connectError(err))
	}
	ok, err := pg.QueryStrings(ctx, "", "SELECT (rolsuper OR (rolcreaterole AND rolcreatedb))::text FROM pg_roles WHERE rolname = current_user")
	if err != nil || len(ok) != 1 || ok[0] != "true" {
		return rpc.DBServerTestResult{}, agentrpc.Invalid("%s cannot create roles and databases: it needs CREATEROLE and CREATEDB (or to be a superuser)", p.AdminUser)
	}
	return rpc.DBServerTestResult{Version: version}, nil
}

// BackedUpDatabases lists the site's databases a backup dumps: those on a
// server the provider does not back up instead.
func (o *Ops) BackedUpDatabases(ctx context.Context, s state.Site) ([]string, error) {
	names, err := o.SiteDatabases(ctx, s)
	if err != nil || o.Servers == nil {
		return names, err
	}
	var out []string
	for _, n := range names {
		id, err := o.serverOf(ctx, n)
		if err != nil {
			return nil, err
		}
		if o.Servers.BackedUp(id) {
			out = append(out, n)
		}
	}
	return out, nil
}
