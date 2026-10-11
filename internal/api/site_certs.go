package api

// A site's certificate (SPEC.md §3.9): Let's Encrypt through the queue,
// the status the pages show, and a certificate the operator brings.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

func (s *Server) handleIssueCertificate(w http.ResponseWriter, r *http.Request, c *call) {
	ps, err := s.Store.PanelSettings()
	if err != nil {
		replyFailure(w, err)
		return
	}
	if ps.ACMEEmail == "" {
		respondErr(w, http.StatusPreconditionFailed, "set acme_email in settings first")
		return
	}
	var b struct {
		Wildcard bool `json:"wildcard"`
	}
	if err := decode(r, &b); err != nil && !errors.Is(err, io.EOF) {
		respondErr(w, http.StatusBadRequest, "malformed request")
		return
	}
	task, err := s.siteSvc().IssueCertificate(c.Actor(), c.Site, ps.ACMEEmail, b.Wildcard)
	if err != nil {
		replyFailure(w, err)
		return
	}
	respond(w, http.StatusAccepted, map[string]any{"task": task})
}

// IssueCertificate queues a Let's Encrypt certificate for the site, a
// wildcard one through Cloudflare DNS when asked.
func (svc siteService) IssueCertificate(actor string, target state.Site, email string, wildcard bool) (jobs.Task, error) {
	task, err := svc.queueIssue(target, email, wildcard, false)
	if err != nil {
		return jobs.Task{}, err
	}
	svc.srv.audit(actor, "certificate.issue", target.Domain, "")
	return task, nil
}

// firstCertificate, from the job that creates a site (a new one, or a
// staging copy as it is cloned or recloned), requests its Let's Encrypt
// certificate when an ACME e-mail is set. A failure is a line in the job's
// log, not its failure: DNS may not point at this server yet, and the site
// serves on the default certificate until an issue succeeds.
func (s *Server) firstCertificate(ctx context.Context, rec state.Site, progress func(int, string)) {
	ps, _ := s.Store.PanelSettings()
	if ps.ACMEEmail == "" {
		return
	}
	progress(80, "Requesting Let's Encrypt certificate for "+rec.Domain)
	var cert rpc.ApplyResult
	if err := agentrpc.Into(ctx, s.Agent, agentrpc.IssueCert, rpc.CertificateParams{Site: rec, Email: ps.ACMEEmail}, &cert); err != nil {
		progress(85, "Certificate pending (DNS may not point here yet): "+err.Error())
		return
	}
	s.recordFiles(cert.Files)
}

// msgCertNotReissued begins the error of a reissue that failed.
const msgCertNotReissued = "the certificate was not reissued for the new aliases; the site keeps serving its old certificate"

// queueReissue, from an apply job that rendered changed aliases, queues the
// certificate for the new names; it runs after this job (same site lock).
// What stops it is a line in the apply job's log, not its failure: the
// settings are applied, and the old certificate is still served.
func (s *Server) queueReissue(r *jobs.Run, cur state.Site) {
	ps, err := s.Store.PanelSettings()
	if err != nil || ps.ACMEEmail == "" {
		r.Progress(90, "warning: "+msgCertNotReissued+" (no acme_email)")
		return
	}
	task, err := s.siteSvc().queueIssue(cur, ps.ACMEEmail, cur.Config.WildcardCert, true)
	if err != nil {
		r.Progress(90, "warning: "+msgCertNotReissued+": "+err.Error())
		return
	}
	s.audit("panel (alias change)", "certificate.issue", cur.Domain, "reissue for new aliases")
	r.Progress(90, fmt.Sprintf("Certificate reissue for the new aliases queued (task %d)", task.ID))
}

// queueIssue queues the certificate.issue job. reissue marks one queued by
// an alias change: its failure says the old certificate is still served.
func (svc siteService) queueIssue(target state.Site, email string, wildcard, reissue bool) (jobs.Task, error) {
	srv := svc.srv
	params := rpc.CertificateParams{Email: email}
	if wildcard {
		token, err := srv.Store.Secret(state.SecretCloudflareToken)
		if err != nil {
			return jobs.Task{}, errors.New("secret store: " + err.Error())
		}
		if token == "" {
			return jobs.Task{}, refuse(probPrecondition, "set cloudflare_api_token in settings first (a token with Zone:DNS:Edit on this domain)")
		}
		params.Wildcard, params.CloudflareToken = true, token
	}
	task, err := srv.startJob(jobReq{kind: "certificate.issue", siteID: target.ID, locks: siteLocks(target.ID)},
		func(ctx context.Context, rec state.Site, progress func(int, string)) error {
			progress(30, "Requesting certificate for "+rec.Domain)
			// Issuing renders the site: what is stored now, not what the
			// request read.
			cur, unlock, err := srv.freshSite(rec.ID)
			defer unlock()
			if err != nil {
				return err
			}
			params.Site = cur
			var res rpc.ApplyResult
			if err := agentrpc.Into(ctx, srv.Agent, agentrpc.IssueCert, params, &res); err != nil {
				if reissue {
					return fmt.Errorf("%s: %w", msgCertNotReissued, err)
				}
				return err
			}
			srv.logIfErr(srv.Store.MarkApplied(cur.ID, cur.DesiredGeneration, nil), "MarkApplied")
			if wildcard && !cur.Config.WildcardCert {
				// Already rendered by the agent.
				if _, err := srv.Store.MutateSiteApplied(cur.ID, func(st *state.Site) error {
					st.Config.WildcardCert = true
					return nil
				}); err != nil {
					return err
				}
			}
			srv.recordFiles(res.Files)
			progress(100, "Certificate installed")
			return nil
		})
	if err != nil {
		return jobs.Task{}, queueFailure{err}
	}
	return task, nil
}

func (s *Server) handleCertificateStatus(w http.ResponseWriter, r *http.Request, c *call) {
	out, err := s.siteSvc().CertificateStatus(r.Context(), c.Site)
	if err != nil {
		replyFailure(w, err)
		return
	}
	respond(w, http.StatusOK, out)
}

// CertificateStatus is the live certificate of the site and the last
// renewal error, if any.
func (svc siteService) CertificateStatus(ctx context.Context, target state.Site) (map[string]any, error) {
	var res rpc.CertStatusResult
	if err := agentrpc.Into(ctx, svc.srv.Agent, agentrpc.CertStatus, rpc.CertStatusParams{Names: []string{target.Domain}}, &res); err != nil {
		return nil, agentFailure{err}
	}
	cert := res.Certs[target.Domain]
	out := map[string]any{"exists": cert.Exists, "hsts": target.Config.HSTS}
	if cert.Exists {
		out["not_after"] = cert.NotAfter.UTC().Format(time.RFC3339)
		out["days_left"] = int(time.Until(cert.NotAfter).Hours() / 24)
		out["issuer"] = cert.Issuer
		out["custom"] = cert.Custom
	}
	if cs, err := svc.srv.Store.CertStateOf(target.ID); err == nil && cs.Error != "" {
		out["last_renewal_error"] = cs.Error
	}
	return out, nil
}

// handleCustomCertificate installs a certificate the operator brings (PUT)
// or removes it to go back to Let's Encrypt (DELETE).
func (s *Server) handleCustomCertificate(w http.ResponseWriter, r *http.Request, c *call) {
	params := rpc.CustomCertParams{Site: c.Site, Remove: r.Method == http.MethodDelete}
	if !params.Remove {
		var b struct {
			Fullchain string `json:"fullchain"`
			Key       string `json:"key"`
		}
		if err := decode(r, &b); err != nil || b.Fullchain == "" || b.Key == "" {
			respondErr(w, http.StatusBadRequest, "fullchain and key (PEM) are required")
			return
		}
		params.Fullchain, params.Key = b.Fullchain, b.Key
	}
	var res rpc.ApplyResult
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.CustomCert, params, &res); err != nil {
		// A certificate the agent refuses is the caller's to fix.
		respondAgentErr(w, http.StatusBadRequest, "", err)
		return
	}
	s.recordFiles(res.Files)
	action := "certificate.custom.install"
	if params.Remove {
		action = "certificate.custom.remove"
	}
	s.audit(c.Actor(), action, c.Site.Domain, "")
	if !params.Remove {
		respond(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	respond(w, http.StatusOK, s.backToLetsEncrypt(c.Actor(), c.Site))
}

// msgNoACMEForLetsEncrypt is the warning of a return to Let's Encrypt with
// no ACME e-mail to issue with.
const msgNoACMEForLetsEncrypt = "set acme_email in settings, then issue the certificate: the site serves the certificate it had before the custom one, which may not cover every name"

// backToLetsEncrypt, after a custom certificate is removed, queues the Let's
// Encrypt certificate for the site's names as they are now: aliases added
// while the custom certificate was installed were never issued for (an
// alias change does not reissue over a custom certificate), so the
// certificate the site goes back to may not cover them.
func (s *Server) backToLetsEncrypt(actor string, site state.Site) map[string]any {
	out := map[string]any{"status": "ok"}
	ps, err := s.Store.PanelSettings()
	if err != nil || ps.ACMEEmail == "" {
		out["warning"] = msgNoACMEForLetsEncrypt
		return out
	}
	task, err := s.siteSvc().queueIssue(site, ps.ACMEEmail, site.Config.WildcardCert, false)
	if err != nil {
		out["warning"] = "the Let's Encrypt certificate was not requested: " + err.Error()
		return out
	}
	s.audit(actor, "certificate.issue", site.Domain, "back to Let's Encrypt")
	out["task"] = task
	return out
}
