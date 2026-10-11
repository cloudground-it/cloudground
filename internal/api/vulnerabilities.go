package api

// The WordPress vulnerability check (#31, SPEC.md §11.6): once a day per
// WordPress site, and on demand, the versions of its core, plugins and
// themes are looked up in a public database (vulndb.go); what is found is
// kept per site, shown on the site's Security page and raised as an alert
// of kind "vulnerability", which the webhook sends like every other.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

const (
	// vulnEvery is how often each site is checked.
	vulnEvery = 24 * time.Hour
	// vulnLock lets one check run at a time on the server: each runs
	// wp-cli in its site's sandbox and sends its requests, and a server
	// with many sites spreads them out instead of starting them together.
	vulnLock = "security:vulncheck"
	// vulnAlertNames is how many components an alert names.
	vulnAlertNames = 3
)

func vulnScheduleKey(id int64) string { return "vulncheck:site:" + strconv.FormatInt(id, 10) }

var errVulnOff = decline(http.StatusConflict, "the vulnerability check is turned off in the settings (vulnerability_check)")

// vulnTarget is a site the check covers: an active WordPress site.
func vulnTarget(st state.Site) bool {
	return st.Status == state.SiteActive && isWordPress(st.Type)
}

// vulnDB is the server's database client, made on first use.
func (s *Server) vulnDB() *vulnDB {
	s.vulnOnce.Do(func() {
		if s.vulnSource == nil {
			s.vulnSource = newVulnDB()
		}
	})
	return s.vulnSource
}

// vulnCheckOn reports whether the panel's switch is on.
func (s *Server) vulnCheckOn() (bool, error) {
	p, err := s.Store.PanelSettings()
	return p.VulnerabilityCheck, err
}

// runSecuritySweep is the security.sweep job (every maintenance tick): it
// queues the due vulnerability checks and malware scans (malware.go).
func (s *Server) runSecuritySweep(ctx context.Context, r *jobs.Run) error {
	sites, err := s.Store.ListSites()
	if err != nil {
		return err
	}
	checks, err := s.queueVulnChecks(sites)
	if err != nil {
		return err
	}
	scans, err := s.queueMalwareScans(sites, time.Now())
	if err != nil {
		return err
	}
	r.Progress(100, fmt.Sprintf("%d vulnerability checks and %d malware scans queued", checks, scans))
	return nil
}

// queueVulnChecks queues the check of each WordPress site whose last one is
// a day old. The key is set when the check is queued, so a site is checked
// once a day from when it was first seen, not all at the same hour; a
// check that fails waits for the next day too (its error stays on the
// site's page and the failed task is an alert).
func (s *Server) queueVulnChecks(sites []state.Site) (int, error) {
	if on, err := s.vulnCheckOn(); err != nil || !on {
		return 0, err
	}
	queued := 0
	for _, st := range sites {
		if !vulnTarget(st) {
			continue
		}
		key := vulnScheduleKey(st.ID)
		if last, _ := s.Store.LastScheduleRun(key); time.Since(last) < vulnEvery {
			continue
		}
		if _, err := s.enqueueVulnCheck(st.ID, true); err != nil {
			return queued, err
		}
		s.logIfErr(s.Store.MarkScheduleRun(key), "MarkScheduleRun")
		queued++
	}
	return queued, nil
}

// enqueueVulnCheck queues the check of a site; one already queued or
// running is joined.
func (s *Server) enqueueVulnCheck(siteID int64, background bool) (state.Job, error) {
	return s.Jobs.Enqueue(jobs.Spec{
		Kind: "wp.vulncheck", SiteID: siteID, Locks: []string{vulnLock}, Background: background,
		DedupKey: "wp.vulncheck:" + strconv.FormatInt(siteID, 10),
		Payload:  sitePayload{SiteID: siteID},
	})
}

// runVulnCheck is the wp.vulncheck job: the site's versions from wp-cli
// (WPPlugins, in the site's sandbox), each looked up, the findings stored.
// A check that cannot complete keeps what the last one found and records
// why.
func (s *Server) runVulnCheck(ctx context.Context, r *jobs.Run) error {
	site, err := s.Store.GetSite(r.Job().SiteID)
	if errors.Is(err, state.ErrNotFound) || (err == nil && !vulnTarget(site)) {
		return nil
	}
	if err != nil {
		return err
	}
	r.Progress(10, "Reading the versions of WordPress, its plugins and themes")
	inv, err := agentrpc.Call(ctx, s.Agent, agentrpc.WPPlugins, rpc.WPParams{Site: site})
	if err == nil {
		r.Progress(40, fmt.Sprintf("Looking up %d components in %s", 1+len(inv.Plugins)+len(inv.Themes), vulnDBName))
		var rep state.VulnReport
		rep, err = s.checkInventory(ctx, inv)
		if err == nil {
			rep.SiteID = site.ID
			if err := s.Store.RecordVulnCheck(rep); err != nil {
				return err
			}
			s.kickAlerts()
			r.Progress(100, fmt.Sprintf("%d known vulnerabilities in %d components", len(rep.Findings), rep.Components))
			return nil
		}
	}
	s.logIfErr(s.Store.RecordVulnCheckError(site.ID, firstLine(err.Error())), "RecordVulnCheckError")
	return err
}

// vulnItem is one component to look up.
type vulnItem struct{ component, slug, version string }

// vulnItems are the components of an inventory the database can be asked
// about: core, and the plugins and themes with a slug and a version. A
// must-use plugin or a drop-in is not a WordPress.org slug.
func vulnItems(inv rpc.WPPluginsResult) []vulnItem {
	var out []vulnItem
	if vulnVersionRe.MatchString(inv.Core) {
		out = append(out, vulnItem{component: "core", version: inv.Core})
	}
	add := func(component string, list []rpc.WPPlugin) {
		for _, p := range list {
			if p.Status == "must-use" || p.Status == "dropin" || !vulnSlugRe.MatchString(p.Name) || p.Version == "" {
				continue
			}
			out = append(out, vulnItem{component: component, slug: p.Name, version: p.Version})
		}
	}
	add("plugin", inv.Plugins)
	add("theme", inv.Themes)
	return out
}

// checkInventory looks every component up, vulnLookups at a time. One
// lookup that fails fails the check: a partial answer would read as "fewer
// vulnerabilities".
func (s *Server) checkInventory(ctx context.Context, inv rpc.WPPluginsResult) (state.VulnReport, error) {
	items := vulnItems(inv)
	if inv.Core != "" && (len(items) == 0 || items[0].component != "core") {
		return state.VulnReport{}, fmt.Errorf("the WordPress version %q cannot be looked up", inv.Core)
	}
	db := s.vulnDB()
	found := make([][]state.VulnFinding, len(items))
	errs := make([]error, len(items))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	slots := make(chan struct{}, vulnLookups)
	var wg sync.WaitGroup
	for i, it := range items {
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer func() { <-slots; wg.Done() }()
			name := it.slug
			if it.component == "core" {
				name = it.version
			}
			list, err := db.lookup(ctx, it.component, name)
			if err != nil {
				errs[i] = err
				cancel()
				return
			}
			for _, rec := range list {
				if rec.affects(it.version) {
					found[i] = append(found[i], rec.finding(it.component, it.slug, it.version))
				}
			}
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil && !errors.Is(err, context.Canceled) {
			return state.VulnReport{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return state.VulnReport{}, err
	}
	rep := state.VulnReport{Core: inv.Core, Components: len(items), Findings: []state.VulnFinding{}}
	for _, f := range found {
		rep.Findings = append(rep.Findings, f...)
	}
	slices.SortStableFunc(rep.Findings, func(a, b state.VulnFinding) int {
		if c := severityRank(b.Severity) - severityRank(a.Severity); c != 0 {
			return c
		}
		return strings.Compare(a.Component+a.Slug, b.Component+b.Slug)
	})
	return rep, nil
}

func severityRank(s string) int {
	return map[string]int{"low": 1, "medium": 2, "high": 3, "critical": 4}[s]
}

// vulnConditions are the vulnerability alerts (SPEC.md §11.1): one per
// active WordPress site with findings, an error when one of them is high
// or critical. The fingerprint is the set of findings, so a new
// vulnerability is a new alert (shown again, sent again) and an update that
// fixes some but not all is too.
func vulnConditions(sites []state.Site, reports map[int64]state.VulnReport) []state.Alert {
	var out []state.Alert
	for _, st := range sites {
		rep, ok := reports[st.ID]
		if !vulnTarget(st) || !ok || len(rep.Findings) == 0 {
			continue
		}
		level := "warning"
		ids := make([]string, 0, len(rep.Findings))
		var names []string
		seen := map[string]bool{}
		for _, f := range rep.Findings {
			if severityRank(f.Severity) >= severityRank("high") {
				level = "error"
			}
			ids = append(ids, f.Component+":"+f.Slug+":"+f.Version+":"+f.ID)
			name := f.Slug + " " + f.Version
			if f.Component == "core" {
				name = "WordPress " + f.Version
			}
			if !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
		slices.Sort(ids)
		sum := sha256.Sum256([]byte(strings.Join(ids, "\n")))
		if len(names) > vulnAlertNames {
			names = append(names[:vulnAlertNames], "…")
		}
		list := strings.Join(names, ", ")
		n := len(rep.Findings)
		out = append(out, state.Alert{Key: "vulnerability:site:" + strconv.FormatInt(st.ID, 10), SiteID: st.ID, Level: level,
			Fingerprint: hex.EncodeToString(sum[:8]), At: rep.CheckedAt.UTC().Format(time.RFC3339),
			Message: fmt.Sprintf("%d known vulnerabilities in WordPress, its plugins or themes: %s", n, list),
			Text:    state.AlertText{Key: "alert.vulns", Counted: true, Count: n, Args: []string{list}}})
	}
	return out
}

// vulnView is GET /api/sites/{id}/vulnerabilities.
type vulnView struct {
	// Enabled is the panel's switch; Source names the database, as its
	// licence asks nothing but the panel credits it anyway.
	Enabled bool              `json:"enabled"`
	Source  map[string]string `json:"source"`
	// Checked says whether the site has been checked at all.
	Checked bool `json:"checked"`
	state.VulnReport
	EverySeconds int `json:"interval_seconds"`
}

func (s *Server) handleGetVulnerabilities(w http.ResponseWriter, r *http.Request, c *call) {
	if !isWordPress(c.Site.Type) {
		fail(w, errNotWordPress)
		return
	}
	on, err := s.vulnCheckOn()
	if err != nil {
		fail(w, storeFailure{err})
		return
	}
	rep, ok, err := s.Store.SiteVulnReport(c.Site.ID)
	if err != nil {
		fail(w, storeFailure{err})
		return
	}
	respond(w, http.StatusOK, vulnView{Enabled: on, Checked: ok, VulnReport: rep, EverySeconds: int(vulnEvery.Seconds()),
		Source: map[string]string{"name": vulnDBName, "url": vulnDBSite, "license": "CC0 1.0"}})
}

// handleCheckVulnerabilities checks the site now.
func (s *Server) handleCheckVulnerabilities(w http.ResponseWriter, r *http.Request, c *call) {
	if !isWordPress(c.Site.Type) {
		fail(w, errNotWordPress)
		return
	}
	if c.Site.Status != state.SiteActive {
		fail(w, decline(http.StatusConflict, "the site is not active"))
		return
	}
	on, err := s.vulnCheckOn()
	if err != nil {
		fail(w, storeFailure{err})
		return
	}
	if !on {
		fail(w, errVulnOff)
		return
	}
	j, err := s.enqueueVulnCheck(c.Site.ID, false)
	if err != nil {
		respondJobErr(w, err)
		return
	}
	s.audit(c.Actor(), "site.vulncheck", c.Site.Domain, "")
	accepted(w, jobs.View(j), nil)
}
