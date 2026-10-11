// panel-agent is CloudGround's privileged daemon: it serves typed commands
// from panel-api on a root-owned Unix socket (provisioning, releases,
// backups, certificates, cache purges, drift checks).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent"
	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/cron"
	"github.com/cloudground-it/cloudground/internal/agent/ops/malware"
	"github.com/cloudground-it/cloudground/internal/agent/ops/panel"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/config"
	"github.com/cloudground-it/cloudground/internal/siteshell"
	"github.com/cloudground-it/cloudground/internal/version"
)

const help = `CloudGround's privileged daemon. Started by systemd as cloudground-agent.service;
it is not normally run by hand.

It serves typed commands from panel-api on a root-owned Unix socket:
provisioning, releases, backups, certificates, cache purges and drift
checks. Only root and the API's user may connect (checked by peer
credentials), and every request carries the shared token.

Configuration: /etc/cloudground/panel.env, then the environment
(internal/config/SPEC.md):
  CLOUDGROUND_AGENT_SOCKET       socket path (default /run/cloudground/agent.sock)
  CLOUDGROUND_AGENT_TOKEN_FILE   shared-secret file (default /etc/cloudground/agent.token)
  CLOUDGROUND_SOCKET_GROUP       group allowed to reach the socket (default cloudground)
  CLOUDGROUND_API_USER           the API's user, the one non-root peer served (default cloudground)

  panel-agent update-guard <plan>
      Run by the agent itself, as the transient unit
      cloudground-update-guard.service, after a self-update: restarts both
      services on the new release and rolls back if they do not come up.

  panel-agent malware-signatures
      Run by the agent itself, as a transient unit, before a malware scan:
      updates ClamAV's databases (freshclam) and rebuilds the reduced set
      the scan reads in /var/lib/cloudground-signatures.

  panel-agent adopt-databases [--state <state.db>] [--dry-run]
      Run by root on the box, with the agent stopped, when the agent's
      record of the sites' databases (/var/lib/cloudground-agent/databases.json)
      was lost: rebuilds it from the panel's state (read only) and the
      server's databases and accounts, and prints what it adopted, kept and
      refused (a system or reserved name, one two sites claim, one the
      server lacks). It creates, drops and grants nothing.`

// answerTimeout bounds how long a stopping agent waits to answer the
// requests in flight (a self-update's reply among them).
const answerTimeout = 5 * time.Second

// minTokenLen is the shortest shared secret the agent accepts.
const minTokenLen = 32

func main() {
	// The same binary runs under two other names, by argv[0], before any
	// flag or configuration is read (R-4.5):
	//   cg-site-shell  a site shell login's login shell, run by sshd as the
	//                  site user: the client of the agent's shell gate;
	//   cg-site-cron   run by cron as root from a site's cron.d file: starts
	//                  that site's scheduled job in the site's sandbox.
	switch {
	case siteshell.Is(os.Args[0]):
		os.Exit(siteshell.Main(os.Args))
	case cron.IsClient(os.Args[0]):
		os.Exit(cron.ClientMain(os.Args))
	}
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if version.HandleFlag(args, "panel-agent", help) {
		return 0
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if len(args) > 0 && args[0] == host.ResticExecName {
		// A sandboxed restic run (host SPEC: restic): secrets on stdin.
		return host.ResticExecMain(args[1:], os.Stdin)
	}
	if len(args) > 0 && args[0] == "update-guard" {
		return panel.GuardMain(args[1:], log)
	}
	if len(args) > 0 && args[0] == malware.RefreshName {
		// The malware scan's signature update, in the transient unit the
		// agent starts for it (ops/malware SPEC §2).
		return malware.RefreshMain(args[1:], os.Stdout, os.Stderr)
	}
	if len(args) > 0 && args[0] == adoptName {
		return adoptMain(args[1:], os.Stdout)
	}
	cfg, err := config.Load(config.Agent)
	if err != nil {
		log.Error("configuration", "err", err)
		return 1
	}
	for _, w := range cfg.Warnings {
		log.Warn("configuration", "warning", w)
	}
	if err := serve(cfg, log); err != nil {
		log.Error("panel-agent stopped", "err", err)
		return 1
	}
	return 0
}

func serve(cfg *config.Config, log *slog.Logger) error {
	token, err := config.ReadSecret(cfg.AgentTokenFile, minTokenLen)
	if err != nil {
		return fmt.Errorf("agent token (regenerate it with the installer): %w", err)
	}
	a := agent.New(log)
	a.AllowPrivateRepo = cfg.BackupPrivateEndpoint
	if err := a.EnsureWorkDir(); err != nil {
		return fmt.Errorf("unsafe agent work directory: %w", err)
	}
	// Every site of a request must match the agent's own record of it.
	if err := a.LoadSiteIndex(); err != nil {
		return err
	}
	// Nothing is in flight before the socket opens, so every scoped database
	// account still present was left by an agent that died mid-query.
	sweep, cancel := context.WithTimeout(context.Background(), time.Minute)
	// Which database and account is which site's: rebuilt from the server
	// on an install from before the record. The sweep reads it.
	if err := a.LoadDatabaseRegistry(sweep); err != nil {
		log.Warn("the record of the sites' databases is not loaded yet; it is retried on first use", "err", err)
	}
	if err := a.SweepDatabase(sweep); err != nil {
		log.Warn("leftover scoped database accounts not swept", "err", err)
	}
	cancel()
	harden, cancelHarden := context.WithTimeout(context.Background(), time.Minute)
	if err := a.HardenDatabase(harden); err != nil {
		log.Warn("MariaDB's security settings not all applied", "err", err)
	}
	cancelHarden()
	// A migration cut short left its site in WordPress maintenance (503).
	if err := a.SweepMigrations(); err != nil {
		log.Warn("a cut-short migration's maintenance not lifted", "err", err)
	}
	restic, cancelRestic := context.WithTimeout(context.Background(), 30*time.Second)
	a.CheckRestic(restic)
	cancelRestic()
	// The API's user is the one non-root peer served; without it there is
	// no API to serve, and starting anyway would only hide the mistake
	// behind "agent unavailable" errors.
	apiUID, err := agentrpc.UIDOf(cfg.APIUser)
	if err != nil {
		return fmt.Errorf("the API's user %q: %w", cfg.APIUser, err)
	}
	srv := a.NewServer(agentrpc.Config{Token: token, AllowedUIDs: []uint32{0, apiUID}, Log: log})

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	served := make(chan error, 1)
	go func() { served <- srv.ListenAndServe(cfg.AgentSocket, cfg.SocketGroup) }()
	log.Info("panel-agent listening", "socket", cfg.AgentSocket, "api_uid", apiUID, "version", version.Version)
	// The site shells' gate (R-4.5). Without it a shell login gets no
	// shell; nothing else depends on it.
	gateCtx, stopGate := context.WithCancel(context.Background())
	defer stopGate()
	go func() {
		if err := a.ServeShellGate(gateCtx); err != nil {
			log.Warn("the site shell gate is not running", "err", err)
		}
	}()

	select {
	case err := <-served:
		return fmt.Errorf("serve: %w", err)
	case sig := <-stop:
		// systemd's stop, a restart included: answer what is in flight, so
		// a self-update's reply reaches the API although the guard restarts
		// the agent right after.
		log.Info("stopping: answering the requests in flight", "signal", sig.String())
	}
	ctx, cancelStop := context.WithTimeout(context.Background(), answerTimeout)
	defer cancelStop()
	if err := srv.Shutdown(ctx); err != nil {
		log.Warn("requests still running when the agent stopped", "err", err)
	}
	if err := <-served; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}
