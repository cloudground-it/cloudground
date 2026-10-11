// Package api is the panel: the HTTP API the web interface and cgctl use,
// the desired state it keeps, and the jobs that bring the server to it. It
// runs unprivileged; everything that needs root is a call to the agent
// (internal/agentrpc). SPEC.md is the contract.
package api

import (
	"context"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudground-it/cloudground/internal/accounts"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/api/events"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/state"
	"github.com/cloudground-it/cloudground/internal/throttle"
)

// AgentCaller is how the panel reaches the agent: *agentrpc.Client in the
// binary, a fake in the tests. Every call goes through agentrpc.Call or
// agentrpc.Into, typed by the method table.
type AgentCaller = agentrpc.Caller

// Server is the panel (SPEC.md §13). The exported fields are set by
// cmd/panel-api before Routes; the rest is built by Init.
type Server struct {
	Store *state.Store
	Agent AgentCaller
	Log   *slog.Logger
	// UI is the built web interface; nil serves 404 for it (tests).
	UI fs.FS
	// InsecureCookies leaves the Secure flag off the session cookie, for a
	// panel served over plain HTTP on a developer's machine.
	InsecureCookies bool
	// DefaultPHP is the PHP version a new site gets when the request names
	// none ("" means 8.4).
	DefaultPHP string
	// BackupPrivateEndpoint lets the backup repository's s3:/rest: endpoint
	// be a private or local address (CLOUDGROUND_BACKUP_PRIVATE_ENDPOINT).
	BackupPrivateEndpoint bool
	// Shutdown closes when the process starts to stop, so a handler that
	// streams can end instead of holding the drain.
	Shutdown <-chan struct{}

	// Jobs is the persistent queue all background work runs on, sched the
	// recurring part of it (jobqueue.go).
	Jobs  *jobs.Queue
	sched *jobs.Scheduler
	// Events is the bus behind GET /api/events (events/SPEC.md).
	Events *events.Bus

	// accounts is the accounts domain: sign-in and its limits, second
	// factors, users (SPEC.md §2).
	accounts *accounts.Service
	// connectorFailLimiter counts wrong connector tokens by address.
	connectorFailLimiter *throttle.Window
	// studio holds Quarry's own sessions and the handoffs into it,
	// studioGate the limits of its sign-in (studio_session.go,
	// studio_auth.go).
	studio     studioSessions
	studioGate *studioGate

	// siteLocks: one *sync.Mutex per site id (lockSite).
	siteLocks sync.Map
	// statements bounds the database statements running at once
	// (statement_limit.go).
	statements statementLimiter
	// updates is the release check's cache (release_check.go).
	updates releaseCheck
	// caps is the agent's last Capabilities answer (capabilities.go).
	caps capsCache

	// The CSP is computed once from the UI's app shell.
	cspOnce sync.Once
	csp     string
	// routePatterns is what Routes registered, in order (tests).
	routePatterns []string

	// The live monitor and the stream: heartbeat and monitorEvery override
	// their periods in tests; globalStreams counts open streams with a
	// server-wide view; monitorKick asks for a reading now.
	heartbeat, monitorEvery time.Duration
	globalStreams           atomic.Int64
	monitorKick             chan struct{}
	// maintenanceWait overrides how long a run-now waits (tests).
	maintenanceWait time.Duration

	// Alerts: one evaluation at a time (alertsMu, which also guards
	// alertsSettled: the first evaluation after the upgrade has ended);
	// alertKick asks the watcher for one; guesses counts wrong connector
	// tokens from every address; webhookClient replaces the guarded client
	// of the webhook (tests).
	alertsMu      sync.Mutex
	alertsSettled bool
	alertKick     chan struct{}
	guesses       tokenGuesses
	webhookClient *http.Client
	// webhookLookup resolves a webhook's host when it is saved (nil: the
	// system resolver; tests replace it).
	webhookLookup func(ctx context.Context, host string) ([]net.IP, error)

	// uptime holds the last check of every monitored site (uptime.go);
	// uptimeKick asks for a round now; uptimeClient replaces the checks'
	// HTTP client and uptimeEvery their period (tests).
	uptime       uptimeState
	usageWalks   usageWalks
	uptimeKick   chan struct{}
	uptimeClient *http.Client
	uptimeEvery  time.Duration
}

// connectorFailures is how many wrong connector tokens an address may send
// a minute (connector_purge.go).
const connectorFailures = 20

// Init builds what the Server needs at run time and starts the job queue,
// which first settles what a previous process left. Calling it again does
// nothing.
func (s *Server) Init() {
	if s.Events == nil {
		s.Events = events.New(events.DefaultRing)
		if s.Store != nil {
			s.Store.SetEvents(s.Events)
		}
	}
	if s.monitorKick == nil {
		s.monitorKick = make(chan struct{}, 1)
	}
	if s.alertKick == nil {
		s.alertKick = make(chan struct{}, 1)
	}
	if s.uptimeKick == nil {
		s.uptimeKick = make(chan struct{}, 1)
	}
	if s.accounts != nil {
		return
	}
	s.accounts = accounts.New(s.Store, s.audit)
	s.connectorFailLimiter = throttle.New(connectorFailures, time.Minute)
	s.studioGate = newStudioGate()
	if s.Store != nil {
		s.startJobQueue()
	}
}
