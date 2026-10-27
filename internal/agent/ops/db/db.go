// Package db is everything the agent does to MariaDB for a site: its
// database and account, the SQL console, the table list, export and import,
// and the dump, import and replace helpers that backup, restore, staging and
// migration share. It reaches the server only through host.MariaDB. SPEC.md
// in this directory is the contract.
package db

import (
	"context"
	"log/slog"
	"regexp"
	"sync"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Ops holds what the handlers use.
type Ops struct {
	MariaDB host.MariaDB
	// Servers is the agent's connection to each external server (#30);
	// nil: none.
	Servers Servers
	// Systemd restarts MariaDB after the first tuning.
	Systemd host.Systemd
	// Open opens a site's tree, creating entries for the site user.
	Open func(s state.Site) (*sitefs.Root, error)
	// WorkDir is root's own scratch directory: staged inputs, rollback
	// points.
	WorkDir string
	// Socket is the server's Unix socket, for scoped connections.
	Socket string
	// Dial opens a connection as a scoped account. Nil means the driver over
	// Socket; tests substitute it.
	Dial func(ctx context.Context, d DialSpec) (Conn, error)
	// Probe reports the machine's RAM (MB) and CPUs for tuning. Nil means
	// sysprobe.
	Probe func() (memMB int64, cpus int, err error)
	// TuneFile is MariaDB's tuning file ("" means tune.ConfigPath).
	TuneFile string
	// ConsoleTimeout is how long a console statement may run (0 means
	// DefaultConsoleTimeout).
	ConsoleTimeout time.Duration
	// RegistryFile is the agent's record of which database and account is
	// which site's (SPEC §2); "" is databases.json in WorkDir.
	RegistryFile string
	// PrimaryPassword writes a new password of a site's primary account
	// where its application reads it (wp-config.php). Nil writes nothing.
	PrimaryPassword func(ctx context.Context, s state.Site, password string) error
	Log             *slog.Logger

	tuneMu  sync.Mutex
	regOnce sync.Once
	reg     *Registry
	scratch scratchServers
}

// Register wires this package's methods into the agent's RPC server.
func (o *Ops) Register(s *agentrpc.Server) {
	agentrpc.Handle(s, agentrpc.CreateDatabase, o.CreateDB)
	agentrpc.Handle(s, agentrpc.DropDatabase, o.DropDB)
	agentrpc.Handle(s, agentrpc.CreateDBUser, o.CreateUser)
	agentrpc.Handle(s, agentrpc.DropDBUser, o.DropUser)
	agentrpc.Handle(s, agentrpc.SetDBPassword, o.SetPassword)
	agentrpc.Handle(s, agentrpc.SetDBGrants, o.SetGrants)
	agentrpc.Handle(s, agentrpc.ListDatabases, o.List)
	if r, ok := o.Servers.(*Remotes); ok {
		agentrpc.Handle(s, agentrpc.SetDBServers, r.Set)
		agentrpc.Handle(s, agentrpc.TestDBServer, r.Test)
	}
	agentrpc.Handle(s, agentrpc.DBQuery, o.Query)
	agentrpc.Handle(s, agentrpc.DBTables, o.Tables)
	agentrpc.Handle(s, agentrpc.DBExport, o.Export)
	agentrpc.Handle(s, agentrpc.DBImport, o.Import)
}

func (o *Ops) log() *slog.Logger {
	if o.Log != nil {
		return o.Log
	}
	return slog.Default()
}

// scratchNameRe is the agent's own throwaway databases: the restore test's
// (cgv_), a staged replacement's scratch and holding ones (cgm_, cgh_).
var scratchNameRe = regexp.MustCompile(`^cg[vmh]_[0-9a-f]{16}$`)

// ValidName reports whether name has the shape of a database the agent
// acts on: a site's (internal/site CheckDBName), or one of its scratch
// databases. Whose a site's database is, the record says (SPEC §2).
func ValidName(name string) bool { return site.ValidDBName(name) || scratchNameRe.MatchString(name) }

// detached is ctx without its cancellation, bounded by d: cleanup that must
// happen although the request has ended.
func detached(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), d)
}
