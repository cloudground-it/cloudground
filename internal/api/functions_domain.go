package api

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/functions"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// The functions' addresses (SPEC.md §17.4, §17.5): a function's own
// domains, the functions domain, their DNS and their certificates.

// cleanFunctionDomain is a host name as typed, checked: lower-cased, no
// trailing dot, a plain host name and not an address.
func cleanFunctionDomain(d string) (string, error) {
	d = strings.TrimSuffix(cleanHost(d), ".")
	if err := site.ValidateDomain(d); err != nil {
		return "", err
	}
	if _, err := netip.ParseAddr(d); err == nil {
		return "", fmt.Errorf("%s is an address: a host name is required", d)
	}
	return d, nil
}

// fnCertificate is a lineage's certificate as the routes show it.
type fnCertificate struct {
	Exists   bool   `json:"exists"`
	NotAfter string `json:"not_after,omitempty"`
	Issuer   string `json:"issuer,omitempty"`
}

func (s *Server) lineageStatus(ctx context.Context, name string) fnCertificate {
	var st rpc.CertStatusResult
	if err := agentrpc.Into(ctx, s.Agent, agentrpc.CertStatus, rpc.CertStatusParams{Names: []string{name}}, &st); err != nil {
		return fnCertificate{}
	}
	info := st.Certs[name]
	if !info.Exists {
		return fnCertificate{}
	}
	return fnCertificate{Exists: true, NotAfter: info.NotAfter.UTC().Format("2006-01-02T15:04:05Z"), Issuer: info.Issuer}
}

// dnsOf is a name's DNS check when the caller's limit allows one.
func (s *Server) dnsOf(ctx context.Context, c *call, name string) *dnsCheck {
	if c == nil || !s.dnsChecks.admit(c.User.ID) {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, dnsCheckTimeout)
	defer cancel()
	d := s.checkDNS(cctx, name)
	return &d
}

func (s *Server) handleSetFunctionDomains(w http.ResponseWriter, r *http.Request, c *call) {
	var req struct {
		Domains []string `json:"domains"`
	}
	if !decodeOr400(w, r, &req) {
		return
	}
	if len(req.Domains) > functions.MaxDomains {
		respondErr(w, http.StatusBadRequest, fmt.Sprintf("at most %d domains", functions.MaxDomains))
		return
	}
	clean := []string{}
	for _, d := range req.Domains {
		cd, err := cleanFunctionDomain(d)
		if err != nil {
			respondErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if slices.Contains(clean, cd) {
			respondErr(w, http.StatusBadRequest, cd+" is listed twice")
			return
		}
		clean = append(clean, cd)
	}
	slices.Sort(clean)
	s.fnMu.Lock()
	defer s.fnMu.Unlock()
	f, ok := s.getFunction(w, r.PathValue("name"))
	if !ok {
		return
	}
	aliasesMu.Lock()
	defer aliasesMu.Unlock()
	taken, err := hostOwners(s.Store, clean, nameSelf{function: f.Name})
	if err != nil {
		internalError(w, "hostOwners", err, "")
		return
	}
	for _, d := range clean {
		if owner, ok := taken[d]; ok {
			respondErr(w, http.StatusConflict, d+" is already used by "+owner)
			return
		}
	}
	old := f.Domains
	f.Domains = clean
	if err := s.renderFunctions(r.Context(), renderWith{fn: &f}); err != nil {
		respondAgentErr(w, 0, "", err)
		return
	}
	if err := s.Store.SetFunctionDomains(f.ID, clean); err != nil {
		// Rendered already: put the files back as stored.
		f.Domains = old
		s.logIfErr(s.renderFunctions(context.WithoutCancel(r.Context()), renderWith{fn: &f}), "renderFunctions")
		internalError(w, "SetFunctionDomains", err, "could not save the domains")
		return
	}
	var task *jobs.Task
	added := slices.ContainsFunc(clean, func(d string) bool { return !slices.Contains(old, d) })
	switch {
	case len(clean) == 0 && len(old) > 0:
		if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.DeleteFunctionsCertificate, rpc.FunctionsCertParams{Name: functions.FunctionLineage(f.ID)}, nil); err != nil {
			s.Log.Warn("could not delete a function's certificate", "function", f.Name, "err", err)
		}
	case added:
		if t, ok := s.queueFunctionsCert(functions.FunctionLineage(f.ID)); ok {
			task = &t
		}
	}
	s.audit(c.Actor(), "function.domains", f.Name, strings.Join(clean, ", "))
	stored, err := s.Store.GetFunction(f.Name)
	if err != nil {
		internalError(w, "GetFunction", err, "")
		return
	}
	respond(w, http.StatusOK, struct {
		functionView
		Task *jobs.Task `json:"task,omitempty"`
	}{s.functionViewOf(stored, s.functionsDomain()), task})
}

func (s *Server) handleFunctionDomainsStatus(w http.ResponseWriter, r *http.Request, c *call) {
	f, ok := s.getFunction(w, r.PathValue("name"))
	if !ok {
		return
	}
	type entry struct {
		Domain string    `json:"domain"`
		DNS    *dnsCheck `json:"dns,omitempty"`
	}
	list := []entry{}
	for _, d := range f.Domains {
		list = append(list, entry{Domain: d, DNS: s.dnsOf(r.Context(), c, d)})
	}
	cert := fnCertificate{}
	if len(f.Domains) > 0 {
		cert = s.lineageStatus(r.Context(), functions.FunctionLineage(f.ID))
	}
	respond(w, http.StatusOK, map[string]any{"domains": list, "certificate": cert})
}

// functionsDomainStatus is GET /api/functions/domain/status's answer.
func (s *Server) functionsDomainStatus(ctx context.Context, c *call) (map[string]any, error) {
	ps, err := s.Store.PanelSettings()
	if err != nil {
		return nil, err
	}
	out := map[string]any{"domain": ps.FunctionsDomain, "acme_email": ps.ACMEEmail != "", "certificate": fnCertificate{}}
	if ps.FunctionsDomain != "" {
		if d := s.dnsOf(ctx, c, ps.FunctionsDomain); d != nil {
			out["dns"] = d
		}
		out["certificate"] = s.lineageStatus(ctx, functions.DomainLineage)
	}
	return out, nil
}

func (s *Server) handleFunctionsDomainStatus(w http.ResponseWriter, r *http.Request, c *call) {
	out, err := s.functionsDomainStatus(r.Context(), c)
	if err != nil {
		internalError(w, "PanelSettings", err, "could not read the settings")
		return
	}
	respond(w, http.StatusOK, out)
}

func (s *Server) handleSetFunctionsDomain(w http.ResponseWriter, r *http.Request, c *call) {
	var req struct {
		Domain string `json:"domain"`
	}
	if !decodeOr400(w, r, &req) {
		return
	}
	d := ""
	if strings.TrimSpace(req.Domain) != "" {
		var err error
		if d, err = cleanFunctionDomain(req.Domain); err != nil {
			respondErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	s.fnMu.Lock()
	defer s.fnMu.Unlock()
	aliasesMu.Lock()
	defer aliasesMu.Unlock()
	if d != "" {
		taken, err := hostOwners(s.Store, []string{d}, nameSelf{functionsDomain: true})
		if err != nil {
			internalError(w, "hostOwners", err, "")
			return
		}
		if owner, ok := taken[d]; ok {
			respondErr(w, http.StatusConflict, d+" is already used by "+owner)
			return
		}
	}
	ps, err := s.Store.PanelSettings()
	if err != nil {
		internalError(w, "PanelSettings", err, "")
		return
	}
	old := ps.FunctionsDomain
	if err := s.renderFunctions(r.Context(), renderWith{domain: &d}); err != nil {
		respondAgentErr(w, 0, "", err)
		return
	}
	if _, err := s.Store.UpdatePanelSettings(func(p *state.PanelSettings) error {
		p.FunctionsDomain = d
		return nil
	}); err != nil {
		s.logIfErr(s.renderFunctions(context.WithoutCancel(r.Context()), renderWith{domain: &old}), "renderFunctions")
		internalError(w, "UpdatePanelSettings", err, "could not save the settings")
		return
	}
	var task any
	switch {
	case d == "" && old != "":
		if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.DeleteFunctionsCertificate, rpc.FunctionsCertParams{Name: functions.DomainLineage}, nil); err != nil {
			s.Log.Warn("could not delete the functions domain's certificate", "err", err)
		}
	case d != "":
		if t, ok := s.queueFunctionsCert(functions.DomainLineage); ok {
			task = t
		}
	}
	s.audit(c.Actor(), "functions.domain", d, "")
	out, err := s.functionsDomainStatus(r.Context(), c)
	if err != nil {
		internalError(w, "PanelSettings", err, "")
		return
	}
	if task != nil {
		out["task"] = task
	}
	respond(w, http.StatusOK, out)
}

// queueFunctionsCert queues functions.certificate for a lineage when an
// ACME e-mail is set (§17.7); false when nothing was queued.
func (s *Server) queueFunctionsCert(lineage string) (jobs.Task, bool) {
	ps, err := s.Store.PanelSettings()
	if err != nil || ps.ACMEEmail == "" {
		return jobs.Task{}, false
	}
	task, err := s.startJob(jobReq{kind: "functions.certificate", dedupKey: "functions.certificate:" + lineage},
		func(ctx context.Context, _ state.Site, progress func(int, string)) error {
			return s.issueFunctionsCert(ctx, lineage, progress)
		})
	if err != nil {
		s.Log.Warn("could not queue the functions' certificate", "lineage", lineage, "err", err)
		return jobs.Task{}, false
	}
	return task, true
}

// lineageDomains are the domains a lineage is for, as stored now.
func (s *Server) lineageDomains(lineage string) ([]string, error) {
	if lineage == functions.DomainLineage {
		ps, err := s.Store.PanelSettings()
		if err != nil || ps.FunctionsDomain == "" {
			return nil, err
		}
		return []string{ps.FunctionsDomain}, nil
	}
	list, err := s.Store.ListFunctions()
	if err != nil {
		return nil, err
	}
	for _, f := range list {
		if functions.FunctionLineage(f.ID) == lineage {
			return f.Domains, nil
		}
	}
	return nil, nil
}

// issueFunctionsCert issues a lineage for its domains as stored now, then
// renders so the files point at it.
func (s *Server) issueFunctionsCert(ctx context.Context, lineage string, progress func(int, string)) error {
	ps, err := s.Store.PanelSettings()
	if err != nil {
		return err
	}
	doms, err := s.lineageDomains(lineage)
	if err != nil {
		return err
	}
	if len(doms) == 0 || ps.ACMEEmail == "" {
		progress(100, "Nothing to issue: no domain or no ACME e-mail")
		return nil
	}
	progress(20, "Requesting a certificate for "+strings.Join(doms, ", "))
	if err := agentrpc.Into(ctx, s.Agent, agentrpc.IssueFunctionsCertificate, rpc.FunctionsCertParams{Name: lineage, Domains: doms, Email: ps.ACMEEmail}, nil); err != nil {
		return err
	}
	progress(80, "Pointing nginx at the certificate")
	s.fnMu.Lock()
	defer s.fnMu.Unlock()
	return s.renderFunctions(ctx, renderWith{})
}

// renewFunctionsCerts renews, from certificate.sweep, every functions
// lineage inside the window (§17.7).
func (s *Server) renewFunctionsCerts(ctx context.Context, ps state.PanelSettings, certs map[string]rpc.CertInfo) {
	for name, info := range certs {
		if !functions.ValidLineage(name) || !info.Exists || time.Until(info.NotAfter) >= certRenewWindow {
			continue
		}
		doms, err := s.lineageDomains(name)
		if err != nil || len(doms) == 0 {
			continue
		}
		if err := agentrpc.Into(ctx, s.Agent, agentrpc.IssueFunctionsCertificate, rpc.FunctionsCertParams{Name: name, Domains: doms, Email: ps.ACMEEmail}, nil); err != nil {
			s.Log.Warn("functions certificate renewal failed", "lineage", name, "err", err)
			s.audit("system", "cert.renew_failed", name, err.Error())
		}
	}
}

// functionLineages are the functions lineages the sweep reads.
func (s *Server) functionLineages(ps state.PanelSettings) []string {
	var names []string
	if ps.FunctionsDomain != "" {
		names = append(names, functions.DomainLineage)
	}
	list, err := s.Store.ListFunctions()
	if err != nil {
		return names
	}
	for _, f := range list {
		if len(f.Domains) > 0 {
			names = append(names, functions.FunctionLineage(f.ID))
		}
	}
	return names
}
