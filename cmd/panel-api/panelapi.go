// Command panel-api serves CloudGround's HTTP API and web interface, without
// privileges. Privileged work goes to panel-agent over its
// authenticated Unix socket.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/api"
	"github.com/cloudground-it/cloudground/internal/config"
	"github.com/cloudground-it/cloudground/internal/mailgate"
	"github.com/cloudground-it/cloudground/internal/state"
	"github.com/cloudground-it/cloudground/internal/version"
	"github.com/cloudground-it/cloudground/ui"
)

const help = `CloudGround's control plane: the HTTP API and embedded web UI. Started by
systemd as cloudground-api.service; there is rarely a reason to run it by hand.

It runs unprivileged and hands privileged work to panel-agent over an
authenticated Unix socket.

Locked out? As root on this machine:
  panel-api recover-admin                       list the accounts
  panel-api recover-admin --email you@example.com
  panel-api recover-admin --email you@example.com --disable-2fa

Configuration: /etc/cloudground/panel.env, then the environment
(internal/config/SPEC.md):
  CLOUDGROUND_STATE              panel database (default /var/lib/cloudground/state.db)
  CLOUDGROUND_SECRET_KEY         key sealing the database's secrets, created on first
                                 start (default /var/lib/cloudground/secret.key; back
                                 it up with the database, which is useless without it)
  CLOUDGROUND_AGENT_SOCKET       agent socket (default /run/cloudground/agent.sock)
  CLOUDGROUND_AGENT_TOKEN_FILE   shared secret with the agent
  CLOUDGROUND_LISTEN             TCP address without socket activation (development;
                                 production listens on /run/cloudground/api.sock)
  CLOUDGROUND_LOCAL_LISTEN       loopback listener for the WordPress connector
  CLOUDGROUND_DRAIN_TIMEOUT      how long a stop waits for running tasks (default 30s)`

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	// Recovery comes first, ahead of --help handling and of everything the
	// daemon needs: it must work on a box whose panel is down or broken.
	if len(args) > 0 && args[0] == "recover-admin" {
		if err := recoverAdmin(args[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "panel-api recover-admin: %v\n", err)
			return 1
		}
		return 0
	}
	if version.HandleFlag(args, "panel-api", help) {
		return 0
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := config.Load(config.API)
	if err != nil {
		log.Error("configuration", "err", err)
		return 1
	}
	for _, w := range cfg.Warnings {
		log.Warn("configuration", "warning", w)
	}
	if err := serve(cfg, log); err != nil {
		log.Error("panel-api stopped", "err", err)
		return 1
	}
	return 0
}

// drainer is the one shutdown hook main needs from the server: wait, until
// the deadline, for the work in flight (tasks today, the job queue later).
type drainer interface {
	Drain(ctx context.Context) error
}

func serve(cfg *config.Config, log *slog.Logger) error {
	store, err := state.OpenWith(cfg.StatePath, state.Options{SecretKeyPath: cfg.SecretKeyPath})
	if err != nil {
		return fmt.Errorf("open state: %w", err)
	}
	defer store.Close()

	agentToken, err := config.ReadSecret(cfg.AgentTokenFile, 1)
	if err != nil {
		return fmt.Errorf("agent token: %w", err)
	}

	appCtx, stopApp := context.WithCancel(context.Background())
	defer stopApp()
	srv := &api.Server{
		Store:                 store,
		Agent:                 agentrpc.NewClient(cfg.AgentSocket, agentToken, log),
		Log:                   log,
		UI:                    ui.FS(),
		InsecureCookies:       cfg.Dev,
		DefaultPHP:            cfg.DefaultPHP,
		BackupPrivateEndpoint: cfg.BackupPrivateEndpoint,
		Shutdown:              appCtx.Done(),
	}
	srv.Init()
	srv.StartScheduler(appCtx)
	if err := announceSetup(store, log); err != nil {
		return err
	}

	// No ReadTimeout: it covers the whole connection, and past it the
	// request's context is cancelled whatever the handler is doing. A body
	// has a deadline of its own, sized to its limit and lifted once read.
	panel := api.NewHTTPServer(srv.Routes())
	connector := &http.Server{
		Addr: cfg.LocalListen, Handler: srv.ConnectorRoutes(),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20,
	}

	failed := make(chan error, 2)
	go func() {
		log.Info("connector listener", "addr", cfg.LocalListen)
		if err := connector.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// The panel itself still works; purges from sites do not.
			log.Error("connector listener failed", "err", err)
		}
	}()
	mailGate := &http.Server{
		Handler: srv.MailRoutes(), ConnContext: api.MailConnContext,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 60 * time.Second,
		WriteTimeout: 120 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 64 << 10,
	}
	go serveMailGate(mailGate, log)
	start, err := panelServe(cfg, panel)
	if err != nil {
		return err
	}
	go func() {
		if err := start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			failed <- err
		}
	}()
	log.Info("panel-api listening", "version", version.Version, "dev", cfg.Dev)

	stop := make(chan os.Signal, 2)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	select {
	case err := <-failed:
		return fmt.Errorf("serve: %w", err)
	case sig := <-stop:
		log.Info("stopping", "signal", sig.String(), "deadline", cfg.DrainTimeout.String())
	}
	// A second signal while draining means "now".
	go func() {
		sig := <-stop
		log.Warn("second signal: exiting without waiting", "signal", sig.String())
		os.Exit(1)
	}()
	return shutdown(httpShutdownTimeout, cfg.DrainTimeout, stopApp, log, srv, panel, connector, mailGate)
}

// serveMailGate serves the sites' mail gate (internal/api SPEC §14.2) on
// its socket in the unit's runtime directory. Every site user may connect
// (0666): the gate knows each one by its uid. Without the directory (a run
// outside the unit) there is no gate, and sites' mail() fails as it did
// before; the panel itself works.
func serveMailGate(s *http.Server, log *slog.Logger) {
	if _, err := os.Stat(filepath.Dir(mailgate.Socket)); err != nil {
		log.Info("no mail gate: its directory is missing", "dir", filepath.Dir(mailgate.Socket))
		return
	}
	_ = os.Remove(mailgate.Socket)
	ln, err := net.Listen("unix", mailgate.Socket)
	if err != nil {
		log.Error("mail gate listener failed", "err", err)
		return
	}
	if err := os.Chmod(mailgate.Socket, 0o666); err != nil {
		log.Error("mail gate: socket mode", "err", err)
		_ = ln.Close()
		return
	}
	log.Info("mail gate", "socket", mailgate.Socket)
	if err := s.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("mail gate failed", "err", err)
	}
}

// httpShutdownTimeout is how long requests in flight get to finish at a
// stop before they are cancelled. Short on purpose: a request can be waiting
// on this very stop (POST /api/services/cloudground-api/restart waits for
// systemd's restart job, which waits for this process), and must not hold
// every restart for the whole task drain.
const httpShutdownTimeout = 5 * time.Second

// shutdown stops in order: background loops and event streams; the HTTP
// servers, whose requests in flight get httpDeadline and are then cancelled
// (their connections closed, so their contexts end); then the server's
// drain hook, which alone gets drainDeadline for the background tasks.
func shutdown(httpDeadline, drainDeadline time.Duration, stopApp func(), log *slog.Logger, d drainer, servers ...*http.Server) error {
	stopApp()
	hctx, cancelHTTP := context.WithTimeout(context.Background(), httpDeadline)
	for _, s := range servers {
		if err := s.Shutdown(hctx); err != nil {
			log.Warn("requests still open at the HTTP deadline: cancelling them", "err", err)
			_ = s.Close()
		}
	}
	cancelHTTP()
	ctx, cancel := context.WithTimeout(context.Background(), drainDeadline)
	defer cancel()
	if err := d.Drain(ctx); err != nil {
		log.Warn("drain incomplete", "err", err)
		return nil // the next start marks the cut-off tasks failed
	}
	log.Info("stopped cleanly")
	return nil
}

// announceSetup mints the one-time setup link on a panel without accounts.
func announceSetup(store *state.Store, log *slog.Logger) error {
	n, err := store.CountUsers()
	if err != nil || n > 0 {
		return nil
	}
	token, err := api.NewSetupToken(store, 24*time.Hour)
	if err != nil {
		return fmt.Errorf("store the setup token: %w", err)
	}
	log.Info("setup pending", "url", "https://<server-ip>:8443/setup/"+token,
		"note", "link valid for 24h; restart panel-api to mint a new one")
	return nil
}

// panelServe picks how the panel listens: plain HTTP on CLOUDGROUND_LISTEN
// in development; else on the socket systemd passes (plain HTTP behind
// nginx when it is a Unix socket, TLS when it is TCP); else TLS on
// CLOUDGROUND_LISTEN.
func panelServe(cfg *config.Config, s *http.Server) (func() error, error) {
	if cfg.Dev {
		s.Addr = cfg.Listen
		return s.ListenAndServe, nil
	}
	ln, err := socketFromSystemd()
	if err != nil {
		return nil, err
	}
	switch {
	case ln != nil && ln.Addr().Network() == "unix":
		return func() error { return s.Serve(ln) }, nil
	case ln != nil:
		s.TLSConfig = tlsConfig(cfg.TLSCert, cfg.TLSKey)
		return func() error { return s.ServeTLS(ln, "", "") }, nil
	default:
		s.Addr = cfg.Listen
		s.TLSConfig = tlsConfig(cfg.TLSCert, cfg.TLSKey)
		return func() error { return s.ListenAndServeTLS("", "") }, nil
	}
}

// socketFromSystemd returns the listener cloudground-api.socket passed as
// fd 3, or nil without socket activation. Keeping the socket in PID 1 lets
// connections queue across a restart instead of failing with a 502.
func socketFromSystemd() (net.Listener, error) {
	if os.Getenv("LISTEN_PID") != strconv.Itoa(os.Getpid()) || os.Getenv("LISTEN_FDS") != "1" {
		return nil, nil
	}
	f := os.NewFile(3, "cloudground-api.socket")
	if f == nil {
		return nil, errors.New("socket activation: fd 3 is not open")
	}
	defer f.Close()
	ln, err := net.FileListener(f)
	if err != nil {
		return nil, fmt.Errorf("socket activation: %w", err)
	}
	return ln, nil
}
