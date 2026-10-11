// Package panel serves the panel's own methods: the certificate for the
// panel's domain, and the self-update with its guard. SPEC.md in this
// directory is the contract.
package panel

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
)

// The panel's systemd units.
const (
	AgentUnit     = "cloudground-agent.service"
	APIUnit       = "cloudground-api.service"
	APISocketUnit = "cloudground-api.socket"
	GuardUnit     = "cloudground-update-guard.service"
)

// Paths are the places this package reads and writes.
type Paths struct {
	NginxSites  string // the panel vhost goes here
	ACMEWebroot string
	CertLiveDir string // certbot's live/ directory
	PanelCert   string // the certificate nginx serves for the panel domain
	PanelKey    string
	PanelAccess string // the operator's access rules, included by the vhost
	PanelGroup  string // the API's group, which reads the panel key
	APISocket   string // the API's Unix socket
	BinDir      string // panel-api, panel-agent, cgctl
	SystemdDir  string // the panel's unit files
	WorkDir     string // root-only scratch: staged state, the update plan
	// StateDB is the API's state database and Records the agent's own
	// records (its site index and database registry): what the pre-update
	// snapshot copies (SPEC §2.5). Empty: no snapshot of that part.
	StateDB string
	Records []string
}

// SnapshotRoot is where the pre-update snapshots are kept.
func (p Paths) SnapshotRoot() string { return filepath.Join(p.StateDir(), SnapshotDirName) }

// SnapshotDirName is the snapshots' directory in the state directory.
const SnapshotDirName = "update-snapshots"

// StateDir holds what outlives an update: the newest release installed
// (read by the installer too) and the last update's outcome.
func (p Paths) StateDir() string { return parentDir(p.WorkDir) }

// Ops holds what the handlers use.
type Ops struct {
	Paths   Paths
	ACME    host.ACME
	Nginx   host.Nginx
	Systemd host.Systemd
	// CertMu and NginxMu are the agent's shared locks; the order is CertMu,
	// then NginxMu.
	CertMu  *sync.Mutex
	NginxMu *sync.Mutex
	// Preflight proves domains reach this server's ACME webroot (ops/certs).
	Preflight func(ctx context.Context, domains []string) error
	// UpdateClient fetches releases. Nil: a client that refuses redirects
	// off https.
	UpdateClient *http.Client
	// LookupGroup resolves a group name to its gid. Nil: the system's.
	LookupGroup func(name string) (int, error)
	Log         *slog.Logger

	updateMu sync.Mutex // one self-update at a time
}

// Register wires this package's methods into the agent's RPC server.
func (o *Ops) Register(s *agentrpc.Server) {
	agentrpc.Handle(s, agentrpc.PanelCert, o.PanelCertificate)
	agentrpc.Handle(s, agentrpc.SelfUpdate, o.SelfUpdate)
}

func (o *Ops) log() *slog.Logger {
	if o.Log != nil {
		return o.Log
	}
	return slog.Default()
}

func (o *Ops) groupID(name string) (int, error) {
	if o.LookupGroup != nil {
		return o.LookupGroup(name)
	}
	g, err := user.LookupGroup(name)
	if err != nil {
		return 0, fmt.Errorf("group %s: %w", name, err)
	}
	return strconv.Atoi(g.Gid)
}
