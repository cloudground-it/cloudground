package api

// The single settings change (PATCH /api/sites/{id}, SPEC.md §3.4): the
// patch, its validation field by field, and the store write that starts its
// job (site_apply.go).

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
	"github.com/cloudground-it/cloudground/internal/velocity"
)

// sitePatch is a change to a site's editable settings. A nil field is not
// part of the change.
type sitePatch struct {
	Aliases        *[]string `json:"aliases"`
	Profile        *string   `json:"profile"`
	CacheEnabled   *bool     `json:"cache_enabled"`
	CacheTTLSec    *int      `json:"cache_ttl_sec"`
	PHPWorkers     *int      `json:"php_workers"`
	HSTS           *bool     `json:"hsts"`
	PHPVersion     *string   `json:"php_version"`
	MemoryLimitMB  *int      `json:"memory_limit_mb"`
	UploadMaxMB    *int      `json:"upload_max_mb"`
	MaxExecSeconds *int      `json:"max_execution_seconds"`
	PublicRoot     *string   `json:"public_root"`
	ProxyUpstream  *string   `json:"proxy_upstream"`
	ObjectCache    *bool     `json:"object_cache"`
	// BypassCookies replaces the site's own page-cache bypass cookies
	// (cookie names; [] clears them).
	BypassCookies *[]string `json:"bypass_cookies"`
	// The limits of the site's slice (#19); 0 is the measured default.
	MemoryMaxMB *int `json:"memory_max_mb"`
	CPUQuotaPct *int `json:"cpu_quota_pct"`
	TasksMax    *int `json:"tasks_max"`
	// UsageAlerts replaces the site's usage alert thresholds (#18).
	UsageAlerts *state.UsageAlerts `json:"usage_alerts"`
	// AllowOverBudget lets an administrator set PHP workers and a memory
	// limit whose product is beyond the site's memory budget. It is a flag
	// of the request, not a setting.
	AllowOverBudget bool `json:"allow_over_budget"`
}

// Limits of the settings (also the bounds the PHP page shows).
const (
	maxAliases    = 50
	maxCacheTTL   = 365 * 24 * 3600
	maxPHPWorkers = 64
)

// Messages kept from the routes the patch replaces.
const (
	msgNoPHP        = "this site does not run PHP"
	msgNotWordPress = "not a WordPress site"
	msgAdminFields  = "only an administrator can change a site's aliases, proxy upstream or resource limits"
	msgNeedCert     = "issue a certificate for this site before enabling HSTS"
)

// patchCheck is what validating one field may look at.
type patchCheck struct {
	ctx  context.Context
	srv  *Server
	site state.Site
	caps *rpc.Capabilities
}

// capabilities reads the agent's answer once per validation.
func (pc *patchCheck) capabilities() (rpc.Capabilities, error) {
	if pc.caps == nil {
		c, err := pc.srv.capabilities(pc.ctx)
		if err != nil {
			return rpc.Capabilities{}, agentFailure{err}
		}
		pc.caps = &c
	}
	return *pc.caps, nil
}

// patchField is one editable setting: whether a patch names it, how it is
// checked (a message for the caller, or an error that is not the caller's
// fault), and how it is written.
type patchField struct {
	name      string
	adminOnly bool
	named     func(p *sitePatch) bool
	check     func(pc *patchCheck, p *sitePatch) (string, error)
	write     func(p *sitePatch, st *state.Site)
}

func intRange(v *int, lo, hi int, msg string) string {
	if *v < lo || *v > hi {
		return msg
	}
	return ""
}

// zeroOrRange is intRange with 0 also allowed: the default.
func zeroOrRange(v *int, lo, hi int, msg string) string {
	if *v == 0 {
		return ""
	}
	return intRange(v, lo, hi, msg)
}

func needsPHP(pc *patchCheck) string {
	if pc.site.PHPVersion == "" {
		return msgNoPHP
	}
	return ""
}

// checkBypassCookies refuses names that are not cookie names the page cache
// can match (velocity.ValidBypassCookie), drops duplicates and sorts them.
func checkBypassCookies(pc *patchCheck, p *sitePatch) (string, error) {
	if pc.site.Type == state.SiteStatic || pc.site.Type == state.SiteProxy || pc.site.Type.IsApp() {
		return "this site has no page cache", nil
	}
	if len(*p.BypassCookies) > velocity.MaxBypassCookies {
		return fmt.Sprintf("at most %d bypass cookies", velocity.MaxBypassCookies), nil
	}
	clean := make([]string, 0, len(*p.BypassCookies))
	for _, c := range *p.BypassCookies {
		c = strings.TrimSpace(c)
		if !velocity.ValidBypassCookie(c) {
			return fmt.Sprintf("bypass cookie %q: a cookie name of letters, digits, '.', '_' and '-', at most 64", c), nil
		}
		if !slices.Contains(clean, c) {
			clean = append(clean, c)
		}
	}
	slices.Sort(clean)
	*p.BypassCookies = clean
	return "", nil
}

// patchFields is every editable setting, in the order the checks run and
// the problems are reported.
var patchFields = []patchField{
	{name: "aliases", adminOnly: true,
		named: func(p *sitePatch) bool { return p.Aliases != nil },
		check: checkAliases,
		write: func(p *sitePatch, st *state.Site) { st.Aliases = slices.Clone(*p.Aliases) }},
	{name: "profile",
		named: func(p *sitePatch) bool { return p.Profile != nil },
		check: func(_ *patchCheck, p *sitePatch) (string, error) {
			if !knownProfile(state.Profile(*p.Profile)) {
				return "invalid profile", nil
			}
			return "", nil
		},
		write: func(p *sitePatch, st *state.Site) { st.Profile = state.Profile(*p.Profile) }},
	{name: "bypass_cookies",
		named: func(p *sitePatch) bool { return p.BypassCookies != nil },
		check: checkBypassCookies,
		write: func(p *sitePatch, st *state.Site) {
			st.Config.BypassCookies = nil
			if len(*p.BypassCookies) > 0 {
				st.Config.BypassCookies = slices.Clone(*p.BypassCookies)
			}
		}},
	{name: "cache_enabled",
		named: func(p *sitePatch) bool { return p.CacheEnabled != nil },
		write: func(p *sitePatch, st *state.Site) { st.Config.CacheEnabled = *p.CacheEnabled }},
	{name: "cache_ttl_sec",
		named: func(p *sitePatch) bool { return p.CacheTTLSec != nil },
		check: func(_ *patchCheck, p *sitePatch) (string, error) {
			return intRange(p.CacheTTLSec, 0, maxCacheTTL, fmt.Sprintf("cache TTL must be 0–%d seconds (0 is the profile's default)", maxCacheTTL)), nil
		},
		write: func(p *sitePatch, st *state.Site) { st.Config.CacheTTLSec = *p.CacheTTLSec }},
	{name: "php_workers",
		named: func(p *sitePatch) bool { return p.PHPWorkers != nil },
		check: func(_ *patchCheck, p *sitePatch) (string, error) {
			return intRange(p.PHPWorkers, 0, maxPHPWorkers, fmt.Sprintf("PHP workers must be 0–%d (0 sizes them automatically)", maxPHPWorkers)), nil
		},
		write: func(p *sitePatch, st *state.Site) { st.Config.Resources.PHPWorkers = *p.PHPWorkers }},
	// The limits bound what the site may take from every other site: an
	// operator of the site cannot raise them (§3.4).
	{name: "memory_max_mb", adminOnly: true,
		named: func(p *sitePatch) bool { return p.MemoryMaxMB != nil },
		check: func(_ *patchCheck, p *sitePatch) (string, error) {
			return zeroOrRange(p.MemoryMaxMB, site.MinMemoryMaxMB, site.MaxMemoryMaxMB,
				fmt.Sprintf("memory limit must be 0 (the default) or %d–%d MB", site.MinMemoryMaxMB, site.MaxMemoryMaxMB)), nil
		},
		write: func(p *sitePatch, st *state.Site) { st.Config.Resources.MemoryMaxMB = *p.MemoryMaxMB }},
	{name: "cpu_quota_pct", adminOnly: true,
		named: func(p *sitePatch) bool { return p.CPUQuotaPct != nil },
		check: func(_ *patchCheck, p *sitePatch) (string, error) {
			return zeroOrRange(p.CPUQuotaPct, site.MinCPUQuotaPct, site.MaxCPUQuotaPct,
				fmt.Sprintf("CPU quota must be 0 (none) or %d–%d %% (100 %% is one CPU)", site.MinCPUQuotaPct, site.MaxCPUQuotaPct)), nil
		},
		write: func(p *sitePatch, st *state.Site) { st.Config.Resources.CPUQuotaPct = *p.CPUQuotaPct }},
	{name: "tasks_max", adminOnly: true,
		named: func(p *sitePatch) bool { return p.TasksMax != nil },
		check: func(_ *patchCheck, p *sitePatch) (string, error) {
			return zeroOrRange(p.TasksMax, site.MinTasksMax, site.MaxTasksMax,
				fmt.Sprintf("process limit must be 0 (the default) or %d–%d", site.MinTasksMax, site.MaxTasksMax)), nil
		},
		write: func(p *sitePatch, st *state.Site) { st.Config.Resources.TasksMax = *p.TasksMax }},
	{name: "usage_alerts",
		named: func(p *sitePatch) bool { return p.UsageAlerts != nil },
		check: func(_ *patchCheck, p *sitePatch) (string, error) {
			a := p.UsageAlerts
			if a.CPUPct < 0 || a.CPUPct > 100 || a.MemoryPct < 0 || a.MemoryPct > 100 {
				return "alert thresholds must be 0 (the default) or 1–100 %", nil
			}
			if a.DiskMB < 0 || a.DiskMB > site.MaxMemoryMaxMB*64 {
				return "the disk alert must be 0 (none) or a size in MB", nil
			}
			return "", nil
		},
		write: func(p *sitePatch, st *state.Site) { st.Config.Resources.Alerts = *p.UsageAlerts }},
	{name: "hsts",
		named: func(p *sitePatch) bool { return p.HSTS != nil },
		write: func(p *sitePatch, st *state.Site) { st.Config.HSTS = *p.HSTS }},
	{name: "php_version",
		named: func(p *sitePatch) bool { return p.PHPVersion != nil },
		check: func(pc *patchCheck, p *sitePatch) (string, error) {
			if msg := needsPHP(pc); msg != "" {
				return msg, nil
			}
			// The version the site runs passes even when it is gone from
			// the server, so its limits can still change.
			if *p.PHPVersion == pc.site.PHPVersion {
				return "", nil
			}
			caps, err := pc.capabilities()
			if err != nil {
				return "", err
			}
			return phpVersionError(caps, *p.PHPVersion), nil
		},
		write: func(p *sitePatch, st *state.Site) { st.PHPVersion = *p.PHPVersion }},
	{name: "memory_limit_mb",
		named: func(p *sitePatch) bool { return p.MemoryLimitMB != nil },
		check: func(pc *patchCheck, p *sitePatch) (string, error) {
			if msg := needsPHP(pc); msg != "" {
				return msg, nil
			}
			return intRange(p.MemoryLimitMB, 64, 4096, "memory limit must be 64–4096 MB"), nil
		},
		write: func(p *sitePatch, st *state.Site) { st.Config.PHP.MemoryLimitMB = *p.MemoryLimitMB }},
	{name: "upload_max_mb",
		named: func(p *sitePatch) bool { return p.UploadMaxMB != nil },
		check: func(pc *patchCheck, p *sitePatch) (string, error) {
			if msg := needsPHP(pc); msg != "" {
				return msg, nil
			}
			return intRange(p.UploadMaxMB, 1, 2048, "upload limit must be 1–2048 MB"), nil
		},
		write: func(p *sitePatch, st *state.Site) { st.Config.PHP.UploadMaxMB = *p.UploadMaxMB }},
	{name: "max_execution_seconds",
		named: func(p *sitePatch) bool { return p.MaxExecSeconds != nil },
		check: func(pc *patchCheck, p *sitePatch) (string, error) {
			if msg := needsPHP(pc); msg != "" {
				return msg, nil
			}
			return intRange(p.MaxExecSeconds, 10, 600, "execution time must be 10–600 seconds"), nil
		},
		write: func(p *sitePatch, st *state.Site) { st.Config.PHP.MaxExecutionSeconds = *p.MaxExecSeconds }},
	{name: "public_root",
		named: func(p *sitePatch) bool { return p.PublicRoot != nil },
		check: func(pc *patchCheck, p *sitePatch) (string, error) {
			if pc.site.Type == state.SiteProxy || pc.site.Type.IsApp() {
				return "a proxy site serves no files of its own", nil
			}
			root := strings.Trim(strings.TrimSpace(*p.PublicRoot), "/")
			if !site.ValidPublicRoot(root) {
				return `public root must be a folder inside the site, such as "public" (letters, digits, - and _)`, nil
			}
			*p.PublicRoot = root
			if root == "" {
				return "", nil
			}
			// The agent refuses a public root that passes through a link
			// (another tenant's tree); it checks again at every render.
			probe := pc.site
			probe.Config.PublicRoot = root
			if err := agentrpc.Into(pc.ctx, pc.srv.Agent, agentrpc.CheckDocRoot, rpc.SiteRef{Site: probe}, nil); err != nil {
				if agentrpc.CodeOf(err) == agentrpc.CodeInvalid {
					return agentrpc.Message(err, "invalid public root"), nil
				}
				return "", agentFailure{err}
			}
			return "", nil
		},
		write: func(p *sitePatch, st *state.Site) { st.Config.PublicRoot = *p.PublicRoot }},
	{name: "proxy_upstream", adminOnly: true,
		named: func(p *sitePatch) bool { return p.ProxyUpstream != nil },
		check: func(pc *patchCheck, p *sitePatch) (string, error) {
			if pc.site.Type != state.SiteProxy {
				return "only a proxy site has an upstream", nil
			}
			up, err := site.NormalizeProxyUpstream(*p.ProxyUpstream)
			if err != nil {
				return err.Error(), nil
			}
			*p.ProxyUpstream = up
			return "", nil
		},
		write: func(p *sitePatch, st *state.Site) { st.Config.ProxyUpstream = *p.ProxyUpstream }},
	{name: "object_cache",
		named: func(p *sitePatch) bool { return p.ObjectCache != nil },
		check: func(pc *patchCheck, _ *sitePatch) (string, error) {
			if !isWordPress(pc.site.Type) {
				return msgNotWordPress, nil
			}
			return "", nil
		},
		write: func(p *sitePatch, st *state.Site) { st.Config.ObjectCache = *p.ObjectCache }},
}

// checkAliases normalises the aliases and refuses any the site cannot have.
func checkAliases(pc *patchCheck, p *sitePatch) (string, error) {
	if pc.site.StagingOf != 0 {
		return "a staging site has no aliases", nil
	}
	if len(*p.Aliases) > maxAliases {
		return fmt.Sprintf("at most %d aliases", maxAliases), nil
	}
	clean := make([]string, 0, len(*p.Aliases))
	for _, a := range *p.Aliases {
		a = cleanHost(a)
		if err := site.ValidateDomain(a); err != nil {
			return err.Error(), nil
		}
		if a == pc.site.Domain {
			return "alias " + a + " is the site's own domain", nil
		}
		if slices.Contains(clean, a) {
			return "alias " + a + " is listed twice", nil
		}
		clean = append(clean, a)
	}
	// The caller holds aliasesMu from here to the write.
	taken, err := nameOwners(pc.srv.Store, clean, pc.site.ID)
	if err != nil {
		return "", err
	}
	for _, a := range clean {
		if owner, ok := taken[a]; ok {
			return "alias " + a + " is already used by " + owner, nil
		}
	}
	*p.Aliases = clean
	return "", nil
}

// named lists the fields the patch sets, in table order.
func (p *sitePatch) named() []string {
	var out []string
	for _, f := range patchFields {
		if f.named(p) {
			out = append(out, f.name)
		}
	}
	return out
}

// writeTo applies the patch to a site record.
func (p *sitePatch) writeTo(st *state.Site) {
	for _, f := range patchFields {
		if f.named(p) {
			f.write(p, st)
		}
	}
}

// updateBy is who asks for a change.
type updateBy struct {
	actor string
	admin bool
}

// validate refuses a patch the caller may not make or the site cannot take
// (§3.4 steps 2–4). It normalises the fields it accepts.
func (svc siteService) validate(ctx context.Context, target state.Site, p *sitePatch, by updateBy) error {
	names := p.named()
	if len(names) == 0 {
		return refuse(probInvalid, "name at least one setting to change")
	}
	if p.AllowOverBudget && !by.admin {
		return &problem{kind: probForbidden, msg: msgOverBudgetAdmin, fields: map[string]string{"allow_over_budget": msgOverBudgetAdmin}}
	}
	if !by.admin {
		denied := map[string]string{}
		for _, f := range patchFields {
			if f.adminOnly && f.named(p) {
				denied[f.name] = msgAdminFields
			}
		}
		if len(denied) > 0 {
			return &problem{kind: probForbidden, msg: msgAdminFields, fields: denied}
		}
	}
	pc := &patchCheck{ctx: ctx, srv: svc.srv, site: target}
	bad := map[string]string{}
	first := ""
	for _, f := range patchFields {
		if !f.named(p) || f.check == nil {
			continue
		}
		msg, err := f.check(pc, p)
		if err != nil {
			return err
		}
		if msg != "" {
			bad[f.name] = msg
			if first == "" {
				first = msg
			}
		}
	}
	if len(bad) > 0 {
		return &problem{kind: probInvalid, msg: first, fields: bad}
	}
	if err := svc.withinBudget(ctx, target, p); err != nil {
		return err
	}
	if p.HSTS != nil && *p.HSTS {
		return svc.certificateHolds(ctx, target)
	}
	return nil
}

// certificateHolds refuses HSTS until the site has a real certificate that
// is not about to lapse: browsers then refuse anything but valid HTTPS for
// a year.
func (svc siteService) certificateHolds(ctx context.Context, target state.Site) error {
	var res rpc.CertStatusResult
	if err := agentrpc.Into(ctx, svc.srv.Agent, agentrpc.CertStatus, rpc.CertStatusParams{Names: []string{target.Domain}}, &res); err != nil {
		return agentFailure{err}
	}
	if c := res.Certs[target.Domain]; !c.Exists || time.Until(c.NotAfter) < 14*24*time.Hour {
		return refuse(probConflict, msgNeedCert)
	}
	return nil
}

// msgCustomCertAliases warns that changed aliases are not covered by a
// certificate the panel cannot reissue.
const msgCustomCertAliases = "this site serves a custom certificate, which may not cover the new aliases: install one that does"

// certificatePlan says what changing the aliases means for the site's
// certificate (SPEC.md §3.4): a Let's Encrypt one is reissued for the new
// names once the render has run; a custom one cannot be, and the caller is
// warned. No certificate, or aliases that do not change, mean nothing.
func (svc siteService) certificatePlan(ctx context.Context, target state.Site, p *sitePatch) (reissue bool, warnings []string, err error) {
	if p.Aliases == nil || slices.Equal(*p.Aliases, target.Aliases) || (len(*p.Aliases) == 0 && len(target.Aliases) == 0) {
		return false, nil, nil
	}
	var res rpc.CertStatusResult
	if err := agentrpc.Into(ctx, svc.srv.Agent, agentrpc.CertStatus, rpc.CertStatusParams{Names: []string{target.Domain}}, &res); err != nil {
		return false, nil, agentFailure{err}
	}
	switch cert := res.Certs[target.Domain]; {
	case !cert.Exists:
		return false, nil, nil
	case cert.Custom:
		return false, []string{msgCustomCertAliases}, nil
	}
	ps, err := svc.srv.Store.PanelSettings()
	if err != nil {
		return false, nil, err
	}
	if ps.ACMEEmail == "" {
		return false, []string{"set acme_email in settings, then issue the certificate again to cover the new aliases"}, nil
	}
	return true, nil, nil
}

// updated is what a settings change answers.
type updated struct {
	site     state.Site
	task     jobs.Task
	warnings []string
}

// Update validates a patch, writes it to the site as stored now and queues
// the one job that applies it. It answers the site after the change.
func (svc siteService) Update(ctx context.Context, target state.Site, p sitePatch, by updateBy) (updated, error) {
	srv := svc.srv
	unlockNames := func() {}
	if p.Aliases != nil {
		// Checked and written under one lock: no other write takes the
		// names in between. Let go after the write: a put-back takes it.
		aliasesMu.Lock()
		unlockNames = sync.OnceFunc(aliasesMu.Unlock)
		defer unlockNames()
	}
	if err := svc.validate(ctx, target, &p, by); err != nil {
		return updated{}, err
	}
	reissue, warnings, err := svc.certificatePlan(ctx, target, &p)
	if err != nil {
		return updated{}, err
	}
	var before state.Site
	after, err := srv.Store.MutateSite(target.ID, func(st *state.Site) error {
		before = *st
		before.Aliases = slices.Clone(st.Aliases)
		p.writeTo(st)
		return nil
	})
	unlockNames()
	if err != nil {
		return updated{}, err
	}
	task, err := srv.enqueueApplyJob(kindSiteUpdate, after, before, reissue)
	if err != nil {
		// Nothing will apply it: the change is undone as a cancel is.
		srv.undoChange(target.ID, applyPayloadOf(after, before, false), 0, nil, nil, true)
		return updated{}, queueFailure{err}
	}
	srv.audit(by.actor, kindSiteUpdate, after.Domain, strings.Join(p.named(), ","))
	return updated{site: after, task: task, warnings: warnings}, nil
}

// msgOverBudgetAdmin refuses the override to anyone but an administrator.
const msgOverBudgetAdmin = "only an administrator can allow PHP workers beyond the site's memory budget"

// withinBudget refuses PHP workers whose memory limits add up to more than
// the memory the site may use (its own limit, else the server's memory),
// unless an administrator says allow_over_budget. Workers left at 0 are
// sized by the agent within the budget and are never refused.
func (svc siteService) withinBudget(ctx context.Context, target state.Site, p *sitePatch) error {
	if p.PHPWorkers == nil && p.MemoryLimitMB == nil {
		return nil
	}
	workers := target.Config.Resources.PHPWorkers
	if p.PHPWorkers != nil {
		workers = *p.PHPWorkers
	}
	memory := target.Config.PHP.MemoryLimitMB
	if p.MemoryLimitMB != nil {
		memory = *p.MemoryLimitMB
	}
	if memory == 0 {
		memory = 256 // the pool's default (512 for a store)
		if target.Type == state.SiteWooCommerce {
			memory = 512
		}
	}
	if workers == 0 || p.AllowOverBudget {
		return nil
	}
	budget := int64(target.Config.Resources.MemoryHighMB)
	what := "the site's memory limit"
	if budget <= 0 {
		var st rpc.SystemStatusResult
		if err := agentrpc.Into(ctx, svc.srv.Agent, agentrpc.SystemStatus, agentrpc.None{}, &st); err != nil {
			return agentFailure{err}
		}
		budget, what = st.MemTotalMB, "the server's memory"
	}
	if budget <= 0 || int64(workers)*int64(memory) <= budget {
		return nil
	}
	msg := fmt.Sprintf("%d PHP workers × %d MB is %d MB, more than %s (%d MB): lower one of them, or (an administrator) send allow_over_budget",
		workers, memory, workers*memory, what, budget)
	fields := map[string]string{}
	if p.PHPWorkers != nil {
		fields["php_workers"] = msg
	}
	if p.MemoryLimitMB != nil {
		fields["memory_limit_mb"] = msg
	}
	return &problem{kind: probInvalid, msg: msg, fields: fields}
}
