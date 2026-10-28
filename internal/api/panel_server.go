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
	"net/netip"
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
	// The git webhook's limits by address: every delivery, and the ones
	// that failed authentication (git_hook.go); gitq is the deploy waiting
	// for each site (git_deploy.go).
	gitHookLimiter     *throttle.Window
	gitHookFailLimiter *throttle.Window
	gitq               gitQueue
	// studio holds Quarry's own sessions and the handoffs into it,
	// studioGate the limits of its sign-in (studio_session.go,
	// studio_auth.go).
	studio     studioSessions
	studioGate *studioGate
	// mcp is the remote MCP server's memory: authorisation requests, codes
	// and its limits (mcp_oauth.go); mcpHandler the route table its tools
	// call (mcp_server.go).
	mcp        *mcpState
	mcpMuxOnce sync.Once
	mcpHandler http.Handler

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
	// cdnPurges coalesces the connector's Cloudflare purges per site.
	cdnPurges     cdnPurges
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

	// vulnSource is the vulnerability database's client and cache
	// (vulndb.go), made once by vulnDB (tests set it first).
	vulnOnce   sync.Once
	vulnSource *vulnDB

	// blocks is the block list's failure counts and what netfilter has
	// (blocklist.go); blocklistEvery replaces the watcher's period and
	// localAddrs this machine's addresses (tests).
	blocks         blocklistState
	blocklistEvery time.Duration
	localAddrs     func() []netip.Addr

	// Cloudflare (cloudflare.go, #22): cfRanges caches the ranges in use
	// (nil: read the store again); cloudflareAPI and cloudflareClient
	// replace Cloudflare's API and the client that calls it (tests).
	cfMu             sync.Mutex
	cfRanges         *cfRangesInUse
	cloudflareAPI    string
	cloudflareClient *http.Client
	// fnRuns is the functions sweep's memory of what ran in which minute
	// (functions.go).
	fnRuns fnRuns
	// fnMu orders the changes to the functions (SPEC.md §17.3).
	fnMu sync.Mutex

	// dnsChecks limits the DNS readiness check; dnsLookup resolves its
	// names (nil: the system resolver; tests replace it) (dns_check.go).
	dnsChecks dnsGate
	dnsLookup func(ctx context.Context, network, host string) ([]netip.Addr, error)

	// updates is the last list of the server's pending package upgrades
	// (system_updates.go).
	sysUpdates updatesCache
	// Mail (mail_gate.go, mail_relay.go): mailRate counts each site's
	// messages through the gate; mailSend replaces the SMTP sender,
	// gateUser the uid → user name lookup and mailTXT the checklist's DNS
	// (tests).
	mailRate mailRateGate
	mailSend mailSender
	gateUser func(uid uint32) (string, error)
	mailTXT  func(ctx context.Context, name string) ([]string, error)
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
	s.initBlocklist()
	if s.accounts != nil {
		return
	}
	// A call the agent answers with "unknown database server" is repeated
	// once after the servers are handed over (db_servers.go).
	if _, ok := s.Agent.(syncingAgent); !ok && s.Agent != nil && s.Store != nil {
		s.Agent = syncingAgent{AgentCaller: s.Agent, s: s}
	}
	s.accounts = accounts.New(s.Store, s.audit)
	s.accounts.Failed = s.signInFailed
	s.connectorFailLimiter = throttle.New(connectorFailures, time.Minute)
	s.gitHookLimiter = throttle.New(gitHookDeliveries, time.Minute)
	s.gitHookFailLimiter = throttle.New(gitHookFailures, time.Minute)
	s.studioGate = newStudioGate()
	s.mcp = newMCPState()
	if s.Store != nil {
		s.startJobQueue()
	}
}
