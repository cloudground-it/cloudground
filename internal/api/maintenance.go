package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
	"github.com/cloudground-it/cloudground/internal/version"
)

// The panel's recurring maintenance, each piece a job of its own with its
// own schedule (internal/jobs/SPEC.md §8–9).

// StartScheduler starts the watchers (events SPEC §4) and the job
// scheduler. The scheduler stops in DrainJobs; the watchers when ctx ends.
func (s *Server) StartScheduler(ctx context.Context) {
	s.startWatchers(ctx)
	s.Init()
	s.sched.Start()
}

// maintenanceWait is how long POST /api/system/maintenance holds the
// request for its tick before answering 202 with the tick's job IDs.
const maintenanceWait = 10 * time.Second

// handleRunMaintenance runs one tick of the scheduler now: the reconcile
// after an upgrade or a change in the number of sites, a capacity sample,
// the backup and retention sweeps (the backups they start are not waited
// for), the certificate renewals if due, the drift scan, then the alerts. A
// job of the tick already queued or running is joined, not doubled. It
// answers 200 once the tick's jobs have ended, or, if that takes longer
// than maintenanceWait (maintenance waiting behind long jobs), 202 with
// the IDs of the jobs to poll (GET /api/tasks/{id}); the tick goes on.
func (s *Server) handleRunMaintenance(w http.ResponseWriter, r *http.Request, c *call) {
	start := time.Now()
	// The tick outlives the request when it answers 202.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Minute)
	ids, done, err := s.sched.StartTick(ctx)
	if err != nil {
		cancel()
		if errors.Is(err, jobs.ErrDraining) {
			respondJobErr(w, err)
			return
		}
		respondErr(w, http.StatusInternalServerError, "the maintenance run did not start")
		return
	}
	wait := s.maintenanceWait
	if wait <= 0 {
		wait = maintenanceWait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case err := <-done:
		cancel()
		if err != nil {
			respondErr(w, http.StatusInternalServerError, "the maintenance run did not complete")
			return
		}
		took := time.Since(start)
		s.audit(c.Actor(), "system.maintenance", "scheduler", fmt.Sprintf("one tick run now, %d ms", took.Milliseconds()))
		respond(w, http.StatusOK, map[string]any{"ran": true, "duration_ms": took.Milliseconds()})
	case <-timer.C:
		go func() { <-done; cancel() }()
		s.audit(c.Actor(), "system.maintenance", "scheduler", "one tick queued now; still running")
		respond(w, http.StatusAccepted, map[string]any{"ran": false, "jobs": ids})
	}
}

// --- reconcile ---

func (s *Server) runReconcile(ctx context.Context, r *jobs.Run) error {
	s.reconcileAfterUpgrade(ctx)
	return nil
}

// reconcileAfterUpgrade renders every active site again once per panel
// version: the rendered files depend on the code too, so a new template or
// a tightened ownership rule must reach the sites that exist, not only new
// ones. A change in the number of active sites re-renders only the pools
// sized from it (budgetFollowsSiteCount), with reloads only where a file
// changed (R-0.7): nothing else rendered depends on the count. The marker is
// "<version>/<active sites>" and moves only when every site rendered, so a
// failure is retried at the next run. ctx stops the walk between sites.
func (s *Server) reconcileAfterUpgrade(ctx context.Context) {
	sites, err := s.Store.ListSites()
	if err != nil {
		s.logIfErr(err, "ListSites")
		return
	}
	active := 0
	for _, site := range sites {
		if site.Status == state.SiteActive {
			active++
		}
	}
	target := version.Version + "/" + strconv.Itoa(active)
	current, err := s.Store.RenderedVersion()
	if err != nil {
		s.logIfErr(err, "RenderedVersion")
		return
	}
	if current == target {
		return
	}
	poolsOnly := strings.HasPrefix(current, version.Version+"/")
	rendered, failures := 0, 0
	if !s.pruneOrphanManagedFiles(sites) {
		failures++ // the pass is not done: try again at the next run
	}
	for _, site := range sites {
		if ctx.Err() != nil {
			return
		}
		if poolsOnly && !budgetFollowsSiteCount(site) {
			continue
		}
		did, err := s.rerenderSite(ctx, site.ID, poolsOnly)
		if did {
			rendered++
		}
		if err != nil {
			failures++
			s.Log.Warn("re-rendering a site after an upgrade or a site-count change failed", "site", site.Domain, "err", err)
		}
	}
	if failures > 0 {
		return
	}
	s.logIfErr(s.Store.SetRenderedVersion(target), "SetRenderedVersion")
	if poolsOnly {
		s.audit("system", "sites.resized", version.Version, fmt.Sprintf("%d active sites; %d PHP pools re-rendered", active, rendered))
	} else {
		s.audit("system", "upgrade.reconciled", version.Version, fmt.Sprintf("%d sites", len(sites)))
	}
}

// budgetFollowsSiteCount: a site that runs PHP with no memory limit of its
// own gets a share of the server's RAM divided among the sites, and its pool
// is sized from that share.
func budgetFollowsSiteCount(site state.Site) bool {
	if site.Type == state.SiteStatic || site.Type == state.SiteProxy || site.PHPVersion == "" {
		return false
	}
	return site.Config.Resources.MemoryHighMB <= 0
}

// rerenderSite renders one site as stored now, under its lock (it may have
// changed or gone since the list was read). poolsOnly asks the agent to
// reload only what came out different, and leaves the crontab alone. It
// reports whether the site was rendered at all.
func (s *Server) rerenderSite(ctx context.Context, id int64, poolsOnly bool) (bool, error) {
	site, unlock, err := s.freshSite(id)
	defer unlock()
	if err != nil || site.Status != state.SiteActive {
		return false, nil
	}
	var res rpc.ApplyResult
	err = agentrpc.Into(ctx, s.Agent, agentrpc.ApplySiteConfig, rpc.SiteRef{Site: site, ReloadOnlyIfChanged: poolsOnly}, &res)
	s.logIfErr(s.Store.MarkApplied(site.ID, site.DesiredGeneration, err), "MarkApplied")
	if err != nil {
		return true, err
	}
	s.recordRendered(site, res.Files)
	// The crontab is rendered by the agent: a new version may change it.
	if !poolsOnly && s.hasCron(site) {
		if err := s.applyCron(ctx, site); err != nil {
			return true, fmt.Errorf("crontab: %w", err)
		}
	}
	return true, nil
}

// --- metrics ---

func (s *Server) runMetricSample(ctx context.Context, r *jobs.Run) error {
	var st rpc.SystemStatusResult
	if err := agentrpc.Into(ctx, s.Agent, agentrpc.SystemStatus, agentrpc.None{}, &st); err != nil {
		return err
	}
	return s.Store.RecordMetric(state.MetricSample{
		CPUHeadroomPct: st.CPUHeadroomPct, MemHeadroomPct: st.MemHeadroomPct,
		DiskHeadroomPct: st.DiskHeadroomPct, Load1: st.Load1,
	})
}

// --- drift and alerts ---

func (s *Server) runDriftScan(ctx context.Context, r *jobs.Run) error {
	_, err := s.scanDrift(ctx)
	return err
}

// --- certificates ---

// certRenewWindow is how close to its expiry a certificate is renewed: 30
// days of a 90-day Let's Encrypt certificate leave room for several failed
// attempts before anything lapses.
const certRenewWindow = 30 * 24 * time.Hour

// runCertificateSweep reads every certificate's real expiry (twice a day, on
// its schedule) and queues a renewal for each Let's Encrypt lineage inside
// the window: certbot runs only for those, so tenants' PHP is not reloaded
// for nothing, and a failure is recorded where the site's page shows it.
// It renews the panel's own certificate itself, under the panel lock.
func (s *Server) runCertificateSweep(ctx context.Context, r *jobs.Run) error {
	ps, err := s.Store.PanelSettings()
	if err != nil {
		return err
	}
	if ps.ACMEEmail == "" {
		r.Progress(100, "No ACME e-mail configured: nothing to renew")
		return nil
	}
	sites, err := s.Store.ListSites()
	if err != nil {
		return err
	}
	const panelName = "cloudground-panel"
	var names []string
	for _, site := range sites {
		if site.Status == state.SiteActive && site.Type != state.SiteProxy {
			names = append(names, site.Domain)
		}
	}
	if ps.PanelDomain != "" {
		names = append(names, panelName)
	}
	var status rpc.CertStatusResult
	if err := agentrpc.Into(ctx, s.Agent, agentrpc.CertStatus, rpc.CertStatusParams{Names: names}, &status); err != nil {
		return err
	}
	due := 0
	for _, site := range sites {
		info, ok := status.Certs[site.Domain]
		if !ok || !info.Exists {
			continue
		}
		s.logIfErr(s.Store.SetCertExpiry(site.ID, info.NotAfter), "SetCertExpiry")
		// A first certificate is the operator's to request; one they brought
		// is theirs to renew (certbot would issue one the vhost never serves).
		if info.Custom || time.Until(info.NotAfter) >= certRenewWindow {
			continue
		}
		due++
		if _, err := s.Jobs.Enqueue(jobs.Spec{
			Kind: "certificate.renew", SiteID: site.ID, Locks: siteLocks(site.ID), Background: true,
			DedupKey: "certificate.renew:" + strconv.FormatInt(site.ID, 10),
			Payload:  sitePayload{SiteID: site.ID},
		}); err != nil {
			return err
		}
	}
	if info, ok := status.Certs[panelName]; ok && info.Exists && time.Until(info.NotAfter) < certRenewWindow {
		if err := agentrpc.Into(ctx, s.Agent, agentrpc.PanelCert, rpc.PanelCertParams{Domain: ps.PanelDomain, Email: ps.ACMEEmail}, nil); err != nil {
			s.Log.Warn("panel certificate renewal failed", "err", err)
			s.audit("system", "cert.renew_failed", ps.PanelDomain, err.Error())
		}
	}
	r.Progress(100, fmt.Sprintf("%d site certificates due for renewal", due))
	return nil
}

// runCertificateRenew renews one site's certificate, rendering the site as
// stored now.
func (s *Server) runCertificateRenew(ctx context.Context, r *jobs.Run) error {
	site, unlock, err := s.freshSite(r.Job().SiteID)
	defer unlock()
	if errors.Is(err, state.ErrNotFound) || (err == nil && site.Status != state.SiteActive) {
		return nil
	}
	if err != nil {
		return err
	}
	ps, err := s.Store.PanelSettings()
	if err != nil {
		return err
	}
	params := rpc.CertificateParams{Site: site, Email: ps.ACMEEmail}
	if site.Config.WildcardCert {
		token, err := s.Store.Secret(state.SecretCloudflareToken)
		s.logIfErr(err, "Secret")
		params.Wildcard, params.CloudflareToken = true, token
	}
	var res rpc.ApplyResult
	if err := agentrpc.Into(ctx, s.Agent, agentrpc.IssueCert, params, &res); err != nil {
		s.Log.Warn("certificate renewal failed", "site", site.Domain, "err", err)
		s.audit("system", "cert.renew_failed", site.Domain, err.Error())
		s.logIfErr(s.Store.SetCertError(site.ID, err.Error()), "SetCertError")
		return err
	}
	// Issuing rendered the site at this generation.
	s.logIfErr(s.Store.MarkApplied(site.ID, site.DesiredGeneration, nil), "MarkApplied")
	s.logIfErr(s.Store.SetCertError(site.ID, ""), "SetCertError")
	s.audit("system", "cert.renewed", site.Domain, "")
	for _, f := range res.Files {
		s.logIfErr(s.Store.RecordManagedFile(f.Path, f.SHA256), "RecordManagedFile")
	}
	return nil
}
