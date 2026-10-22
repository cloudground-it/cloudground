package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/api/events"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// The server's own routes (SPEC.md §7): health, capacity, the audit log,
// services, logs, the firewall, the panel's certificate and its update.

// healthz is the liveness probe's answer.
type healthz struct {
	Status string `json:"status"`
	Agent  bool   `json:"agent"`
}

// handleHealthz tells a monitor whether the panel is up and reaches the
// agent, and nothing more: anyone may ask.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.Ping, agentrpc.None{}, nil); err != nil {
		respond(w, http.StatusServiceUnavailable, healthz{Status: "degraded"})
		return
	}
	respond(w, http.StatusOK, healthz{Status: "ok", Agent: true})
}

func (s *Server) handleSystemStatus(w http.ResponseWriter, r *http.Request) {
	res, err := agentrpc.Call(r.Context(), s.Agent, agentrpc.SystemStatus, agentrpc.None{})
	if err != nil {
		respondAgentErr(w, 0, "agent unavailable", err)
		return
	}
	respond(w, http.StatusOK, res)
}

func (s *Server) handleSystemMetrics(w http.ResponseWriter, r *http.Request) {
	samples, err := s.Store.ListMetrics(0)
	if err != nil {
		s.logIfErr(err, "ListMetrics")
		respondErr(w, http.StatusInternalServerError, "could not read the metrics")
		return
	}
	respond(w, http.StatusOK, samples)
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Store.ListAuditEvents(200)
	if err != nil {
		s.logIfErr(err, "ListAuditEvents")
		respondErr(w, http.StatusInternalServerError, "could not read the audit log")
		return
	}
	if rows == nil {
		rows = []state.AuditEvent{}
	}
	respond(w, http.StatusOK, rows)
}

func (s *Server) handleListServices(w http.ResponseWriter, r *http.Request) {
	res, err := agentrpc.Call(r.Context(), s.Agent, agentrpc.ServiceStatus, agentrpc.None{})
	answer(w, http.StatusOK, res.Services, err)
}

// restartService restarts a service the services page lists. Which
// services exist and which can be restarted is the agent's to say; the
// panel's rule is that it restarts only what it shows.
func (s *Server) restartService(ctx context.Context, name string) (map[string]string, error) {
	caps, err := s.capabilities(ctx)
	if err != nil {
		return nil, err
	}
	if sv, ok := caps.Service(name); !ok || !sv.Listed || !sv.Restartable {
		return nil, decline(http.StatusBadRequest, "unknown service")
	}
	return agentrpc.Call(ctx, s.Agent, agentrpc.RestartService, rpc.ServiceParams{Name: name})
}

func (s *Server) handleRestartService(w http.ResponseWriter, r *http.Request, c *call) {
	name := r.PathValue("name")
	out, err := s.restartService(r.Context(), name)
	if err == nil {
		s.audit(c.Actor(), "service.restart", name, "")
		s.Events.Publish(events.ServiceChanged, 0, map[string]any{"names": []string{name}})
	}
	answer(w, http.StatusOK, out, err)
}

// tail reads the end of a log through the agent; lines that is not a
// positive number means 200.
func (s *Server) tail(ctx context.Context, source, site, lines string) (rpc.TailLogResult, error) {
	n, err := strconv.Atoi(lines)
	if err != nil || n < 1 {
		n = 200
	}
	return agentrpc.Call(ctx, s.Agent, agentrpc.TailLog, rpc.TailLogParams{Source: source, Site: site, Lines: n})
}

// handleReadLog tails any log the agent knows, the server-wide ones (nginx,
// MariaDB, the panel itself) included, which cover every site: it is a
// global route, and an operator reads its own sites' logs through
// handleSiteLog instead.
func (s *Server) handleReadLog(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	res, err := s.tail(r.Context(), q.Get("source"), q.Get("site"), q.Get("lines"))
	answer(w, http.StatusOK, res, err)
}

// handleSiteLog tails one of the site's own logs. The query names the kind
// only; the site is the route's, already checked against the caller.
func (s *Server) handleSiteLog(w http.ResponseWriter, r *http.Request, c *call) {
	kind := r.URL.Query().Get("source")
	switch kind {
	case "access", "error", "php", "cleanup":
	default:
		respondErr(w, http.StatusBadRequest, "source must be access, error, php or cleanup")
		return
	}
	res, err := s.tail(r.Context(), "site:"+c.Site.Domain+":"+kind, c.Site.Domain, r.URL.Query().Get("lines"))
	answer(w, http.StatusOK, res, err)
}

func (s *Server) handleFirewallStatus(w http.ResponseWriter, r *http.Request) {
	res, err := agentrpc.Call(r.Context(), s.Agent, agentrpc.FirewallStatus, agentrpc.None{})
	answer(w, http.StatusOK, res, err)
}

// handleFirewallRule adds or removes a rule. The audit names the rule the
// agent applied and its source, so the log answers who opened which port.
func (s *Server) handleFirewallRule(w http.ResponseWriter, r *http.Request, c *call) {
	var rule rpc.FirewallRuleParams
	if !decodeOr400(w, r, &rule) {
		return
	}
	out, err := agentrpc.Call(r.Context(), s.Agent, agentrpc.FirewallRule, rule)
	if err == nil {
		subject := out["rule"]
		if rule.From != "" {
			subject += " from " + rule.From
		}
		s.audit(c.Actor(), "firewall.rule", subject, "")
	}
	answer(w, http.StatusOK, out, err)
}

// handleFirewallEnable turns ufw on after allowing SSH, the sites and the
// panel (system.FirewallEnable). The body names the rules the operator was
// shown; the agent refuses (409) when its plan is no longer that.
func (s *Server) handleFirewallEnable(w http.ResponseWriter, r *http.Request, c *call) {
	var req rpc.FirewallEnableParams
	if !decodeOr400(w, r, &req) {
		return
	}
	if err := req.Validate(); err != nil {
		respondErr(w, http.StatusBadRequest, err.Error())
		return
	}
	out, err := agentrpc.Call(r.Context(), s.Agent, agentrpc.FirewallEnable, req)
	if err == nil {
		s.audit(c.Actor(), "firewall.enable", "ufw", "allowed "+strings.Join(req.Expect, " "))
	}
	answer(w, http.StatusOK, out, err)
}

// panelCertificate saves the panel's domain and ACME e-mail (the renewal
// sweep needs both) and starts the certificate's issue. An e-mail left
// empty is the one in the settings.
func (s *Server) panelCertificate(domain, email string) (jobs.Task, error) {
	if email == "" {
		ps, err := s.Store.PanelSettings()
		if err != nil {
			s.logIfErr(err, "PanelSettings")
			return jobs.Task{}, decline(http.StatusInternalServerError, "could not read the settings")
		}
		email = ps.ACMEEmail
	}
	if domain == "" || email == "" {
		return jobs.Task{}, decline(http.StatusBadRequest, "panel domain and acme email are required")
	}
	_, err := s.Store.UpdatePanelSettings(func(p *state.PanelSettings) error {
		p.PanelDomain, p.ACMEEmail = domain, email
		return nil
	})
	s.logIfErr(err, "UpdatePanelSettings")
	agent := s.Agent
	return s.runJob(jobReq{kind: "panel.certificate", locks: []string{jobs.PanelLock}}, func(ctx context.Context, _ state.Site, step func(int, string)) error {
		step(30, "Requesting certificate for "+domain)
		if err := agentrpc.Into(ctx, agent, agentrpc.PanelCert, rpc.PanelCertParams{Domain: domain, Email: email}, nil); err != nil {
			return err
		}
		step(100, "Panel certificate installed — the panel will restart")
		return nil
	})
}

func (s *Server) handlePanelCertificate(w http.ResponseWriter, r *http.Request, c *call) {
	var req struct {
		Domain string `json:"domain"`
		Email  string `json:"email"`
	}
	if !decodeOr400(w, r, &req) {
		return
	}
	task, err := s.panelCertificate(req.Domain, req.Email)
	if err == nil {
		s.audit(c.Actor(), "panel.certificate", req.Domain, "")
	}
	accepted(w, task, err)
}

// handleSelfUpdate installs a release. The request names at most the
// version the operator was offered: where the release comes from and the
// key it must be signed with are compiled into the agent, out of reach of
// an admin session or a forged request. The attempt is audited before the
// call, which ends with the agent restarting.
func (s *Server) handleSelfUpdate(w http.ResponseWriter, r *http.Request, c *call) {
	var req struct {
		Version string `json:"version"`
	}
	if !decodeOr400(w, r, &req) {
		return
	}
	asked := req.Version
	if asked == "" {
		asked = "latest"
	}
	ps, err := s.Store.PanelSettings()
	if err != nil {
		s.logIfErr(err, "PanelSettings")
		respondErr(w, http.StatusInternalServerError, "could not read the settings")
		return
	}
	// The update restarts both services: a backup, a restore or a job that
	// cannot resume would be cut (internal/jobs SPEC §11).
	active, err := s.Store.ActiveJobs()
	if err != nil {
		s.logIfErr(err, "ActiveJobs")
		respondErr(w, http.StatusInternalServerError, "could not read the job queue")
		return
	}
	for _, j := range active {
		if j.Status == state.JobRunning && jobs.BlocksPanelChange(j.Kind) {
			respondErr(w, http.StatusConflict, fmt.Sprintf("task %d (%s) is running: update once it has finished", j.ID, j.Kind))
			return
		}
	}
	s.audit(c.Actor(), "panel.update", asked, "")
	res, err := agentrpc.Call(r.Context(), s.Agent, agentrpc.SelfUpdate, rpc.SelfUpdateParams{Version: req.Version, Channel: ps.UpdateChannel})
	if err != nil {
		fail(w, err)
		return
	}
	// The agent restarts onto the new build after this answer; the
	// capabilities cache waits for its new start time.
	s.expectAgentRestart()
	respond(w, http.StatusOK, res)
}
