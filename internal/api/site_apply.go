package api

// The job that applies a settings change (SPEC.md §3.5). The request has
// already written the new settings; the job renders them. Its payload holds
// what the request set and what it replaced, so a render that fails, or a
// job canceled before it ran, can put the request's fields back wherever no
// later change replaced them. Built from the payload alone, it survives a
// restart and runs again: rendering is idempotent.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// The kinds of the apply job: PATCH's, and those the routes it replaced
// queued (PUT /config, PUT /php, POST /wp/object-cache, removed in R-8.2).
// Their jobs may still sit in the queue of an upgraded panel, so the kinds
// stay registered and run their payloads as before.
const (
	kindSiteUpdate  = "site.update"
	kindApplyConfig = "site.apply-config"
	kindApplyPHP    = "site.php"
	kindObjectCache = "wp.object_cache"
)

// scalars are the editable settings but the aliases. The JSON names of the
// first nine are those of the payloads written before R-4.3, which
// therefore read the same way.
type scalars struct {
	Profile      state.Profile `json:"profile"`
	CacheEnabled bool          `json:"cache_enabled"`
	CacheTTLSec  int           `json:"cache_ttl_sec"`
	PHPWorkers   int           `json:"php_workers"`
	HSTS         bool          `json:"hsts"`
	PHPVersion   string        `json:"php_version"`
	MemoryMB     int           `json:"memory_limit_mb"`
	UploadMB     int           `json:"upload_max_mb"`
	MaxExecSec   int           `json:"max_execution_seconds"`
	PublicRoot   string        `json:"public_root,omitempty"`
	Upstream     string        `json:"proxy_upstream,omitempty"`
	ObjectCache  bool          `json:"object_cache,omitempty"`
	// BypassCookies is the site's own bypass cookie names, comma-joined
	// (a scalar, so a payload compares as one value).
	BypassCookies string `json:"bypass_cookies,omitempty"`
	// The slice's limits (#19): written into its unit by the render.
	MemoryMaxMB int `json:"memory_max_mb,omitempty"`
	CPUQuotaPct int `json:"cpu_quota_pct,omitempty"`
	TasksMax    int `json:"tasks_max,omitempty"`
	// The usage alert thresholds (#18): the evaluator reads them from the
	// store, no file depends on them.
	UsageAlerts state.UsageAlerts `json:"usage_alerts,omitzero"`
}

// editable is every setting a patch can change, as a job payload stores it.
type editable struct {
	scalars
	Aliases []string `json:"aliases,omitempty"`
}

func editableOf(st state.Site) editable {
	c := st.Config
	return editable{
		scalars: scalars{
			Profile: st.Profile, CacheEnabled: c.CacheEnabled, CacheTTLSec: c.CacheTTLSec,
			PHPWorkers: c.Resources.PHPWorkers, HSTS: c.HSTS, PHPVersion: st.PHPVersion,
			MemoryMB: c.PHP.MemoryLimitMB, UploadMB: c.PHP.UploadMaxMB, MaxExecSec: c.PHP.MaxExecutionSeconds,
			PublicRoot: c.PublicRoot, Upstream: c.ProxyUpstream, ObjectCache: c.ObjectCache,
			BypassCookies: strings.Join(c.BypassCookies, ","),
			MemoryMaxMB:   c.Resources.MemoryMaxMB, CPUQuotaPct: c.Resources.CPUQuotaPct, TasksMax: c.Resources.TasksMax,
			UsageAlerts: c.Resources.Alerts,
		},
		Aliases: slices.Clone(st.Aliases),
	}
}

// sameAside reports whether a and b agree on everything but the object
// cache and the usage alert thresholds, the two settings no rendered file
// depends on.
func sameAside(a, b editable) bool {
	x, y := a.scalars, b.scalars
	x.ObjectCache, y.ObjectCache = false, false
	x.UsageAlerts, y.UsageAlerts = state.UsageAlerts{}, state.UsageAlerts{}
	return x == y && slices.Equal(a.Aliases, b.Aliases)
}

type applyPayload struct {
	Ours     editable `json:"ours"`
	Previous editable `json:"previous"`
	// The site's generation before the request's change and after it.
	PrevGen int64 `json:"prev_generation"`
	OurGen  int64 `json:"our_generation"`
	// ReissueCert: the aliases changed on a site with a Let's Encrypt
	// certificate; once rendered, the certificate is issued again for the
	// new names.
	ReissueCert bool `json:"reissue_cert,omitempty"`
}

// objectCacheChanged and renderNeeded decide the job's two steps.
func (p applyPayload) objectCacheChanged() bool { return p.Ours.ObjectCache != p.Previous.ObjectCache }

func renderNeeded(kind string, p applyPayload) bool {
	if kind == kindObjectCache {
		return false
	}
	quiet := p.objectCacheChanged() || p.Ours.UsageAlerts != p.Previous.UsageAlerts
	return !(quiet && sameAside(p.Ours, p.Previous))
}

// enqueueApply queues the job of kind for a change that turned previous
// into site.
func (s *Server) enqueueApply(kind string, site, previous state.Site) (jobs.Task, error) {
	return s.enqueueApplyJob(kind, site, previous, false)
}

// enqueueApplyJob is enqueueApply, with the certificate reissued after the
// render when reissue is set.
func (s *Server) enqueueApplyJob(kind string, site, previous state.Site, reissue bool) (jobs.Task, error) {
	return s.enqueue(jobs.Spec{
		Kind: kind, SiteID: site.ID, Locks: siteLocks(site.ID),
		Payload: applyPayloadOf(site, previous, reissue),
	})
}

// applyPayloadOf is the payload of the change that turned previous into
// site.
func applyPayloadOf(site, previous state.Site, reissue bool) applyPayload {
	return applyPayload{
		Ours: editableOf(site), Previous: editableOf(previous),
		PrevGen: previous.DesiredGeneration, OurGen: site.DesiredGeneration,
		ReissueCert: reissue,
	}
}

// applyKinds are the kinds whose payload is an applyPayload.
var applyKinds = []string{kindSiteUpdate, kindApplyConfig, kindApplyPHP, kindObjectCache}

// putters write one field of a payload back into a site, by the field's
// JSON name.
var putters = map[string]func(st *state.Site, v editable){
	"aliases":               func(st *state.Site, v editable) { st.Aliases = slices.Clone(v.Aliases) },
	"profile":               func(st *state.Site, v editable) { st.Profile = v.Profile },
	"cache_enabled":         func(st *state.Site, v editable) { st.Config.CacheEnabled = v.CacheEnabled },
	"cache_ttl_sec":         func(st *state.Site, v editable) { st.Config.CacheTTLSec = v.CacheTTLSec },
	"php_workers":           func(st *state.Site, v editable) { st.Config.Resources.PHPWorkers = v.PHPWorkers },
	"hsts":                  func(st *state.Site, v editable) { st.Config.HSTS = v.HSTS },
	"php_version":           func(st *state.Site, v editable) { st.PHPVersion = v.PHPVersion },
	"memory_limit_mb":       func(st *state.Site, v editable) { st.Config.PHP.MemoryLimitMB = v.MemoryMB },
	"upload_max_mb":         func(st *state.Site, v editable) { st.Config.PHP.UploadMaxMB = v.UploadMB },
	"max_execution_seconds": func(st *state.Site, v editable) { st.Config.PHP.MaxExecutionSeconds = v.MaxExecSec },
	"public_root":           func(st *state.Site, v editable) { st.Config.PublicRoot = v.PublicRoot },
	"proxy_upstream":        func(st *state.Site, v editable) { st.Config.ProxyUpstream = v.Upstream },
	"object_cache":          func(st *state.Site, v editable) { st.Config.ObjectCache = v.ObjectCache },
	"bypass_cookies":        func(st *state.Site, v editable) { st.Config.BypassCookies = splitCookies(v.BypassCookies) },
	"memory_max_mb":         func(st *state.Site, v editable) { st.Config.Resources.MemoryMaxMB = v.MemoryMaxMB },
	"cpu_quota_pct":         func(st *state.Site, v editable) { st.Config.Resources.CPUQuotaPct = v.CPUQuotaPct },
	"tasks_max":             func(st *state.Site, v editable) { st.Config.Resources.TasksMax = v.TasksMax },
	"usage_alerts":          func(st *state.Site, v editable) { st.Config.Resources.Alerts = v.UsageAlerts },
}

// splitCookies is the list a payload's comma-joined bypass cookies stand
// for (none for "").
func splitCookies(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// differing names the fields on which a and b differ.
func differing(a, b editable) []string {
	var out []string
	if !slices.Equal(a.Aliases, b.Aliases) {
		out = append(out, "aliases")
	}
	t, va, vb := reflect.TypeOf(a.scalars), reflect.ValueOf(a.scalars), reflect.ValueOf(b.scalars)
	for i := range t.NumField() {
		if !va.Field(i).Equal(vb.Field(i)) {
			name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
			out = append(out, name)
		}
	}
	return out
}

// laterFields names the fields a change made after generation ourGen set:
// the fields each later settings job of the site changed, unless that job
// failed or was canceled (its own change was then put back). Compared by
// generation, not by value: a value set back to ours by a later request
// is still that request's (A sets 256, B 128, C 256 again: A's put-back
// must not touch it). Unreadable jobs count as every field set.
func (s *Server) laterFields(siteID, ourGen, self int64) map[string]bool {
	later := map[string]bool{}
	list, err := s.Store.SiteJobs(siteID, applyKinds...)
	if err != nil {
		s.logIfErr(err, "SiteJobs")
		for name := range putters {
			later[name] = true
		}
		return later
	}
	for _, j := range list {
		if j.ID == self || j.Status == state.JobFailed || j.Status == state.JobCanceled {
			continue
		}
		var q applyPayload
		if json.Unmarshal([]byte(j.Payload), &q) != nil || q.OurGen <= ourGen {
			continue
		}
		for _, f := range differing(q.Ours, q.Previous) {
			later[f] = true
		}
	}
	return later
}

// undoChange puts back the fields the change p made (only those in only,
// when only is set) that no later change set since. An alias another site
// has taken meanwhile is left off, and note says so. With asRendered (a
// failed object-cache switch, a cancel, a refused enqueue), when nothing
// changed the site after p and what was there before p had been rendered,
// the site is as rendered again and is marked applied (the put-back is a
// new generation, but the same configuration). A failed render passes
// false: its error stays on the site.
func (s *Server) undoChange(siteID int64, p applyPayload, self int64, only map[string]bool, note func(string), asRendered bool) {
	later := s.laterFields(siteID, p.OurGen, self)
	var fields []string
	for _, f := range differing(p.Ours, p.Previous) {
		if (only == nil || only[f]) && !later[f] {
			fields = append(fields, f)
		}
	}
	if len(fields) == 0 {
		return
	}
	restore := p.Previous
	dropped := false
	if slices.Contains(fields, "aliases") {
		aliasesMu.Lock()
		defer aliasesMu.Unlock()
		taken, err := nameOwners(s.Store, restore.Aliases, siteID)
		if err != nil {
			s.logIfErr(err, "nameOwners")
			fields = slices.DeleteFunc(fields, func(f string) bool { return f == "aliases" })
			dropped = true
		}
		var keep []string
		for _, a := range restore.Aliases {
			if owner, ok := taken[a]; ok {
				dropped = true
				if note != nil {
					note("alias " + a + " is left off: " + owner + " uses it now")
				}
				continue
			}
			keep = append(keep, a)
		}
		restore.Aliases = keep
	}
	before, err := s.Store.GetSite(siteID)
	if err != nil {
		return
	}
	after, err := s.Store.MutateSite(siteID, func(st *state.Site) error {
		for _, f := range fields {
			putters[f](st, restore)
		}
		return nil
	})
	if err != nil {
		if !errors.Is(err, state.ErrNotFound) {
			s.logIfErr(err, "MutateSite (put back)")
		}
		return
	}
	untouched := before.DesiredGeneration == p.OurGen
	rendered := p.PrevGen > 0 && before.AppliedGeneration >= p.PrevGen
	if asRendered && untouched && rendered && !dropped && len(later) == 0 {
		s.logIfErr(s.Store.MarkApplied(after.ID, after.DesiredGeneration, nil), "MarkApplied")
	}
}

// cancelApply undoes a change whose job was canceled before it ran.
func (s *Server) cancelApply(j state.Job) {
	var p applyPayload
	if err := json.Unmarshal([]byte(j.Payload), &p); err != nil {
		s.logIfErr(err, "decode apply payload")
		return
	}
	s.undoChange(j.SiteID, p, j.ID, nil, nil, true)
}

// errUnknownIntent ends a job queued by a version that did not write what
// it was to do (an object-cache toggle before R-4.3 stored no payload).
var errUnknownIntent = errors.New("this change was queued by an earlier version of the panel without saying what to apply; make it again")

// applyWords are the progress lines of each kind.
var applyWords = map[string][2]string{
	kindSiteUpdate:  {"Applying settings for ", "Settings applied"},
	kindApplyConfig: {"Re-rendering configuration for ", "Configuration applied"},
	kindApplyPHP:    {"Applying PHP settings for ", "PHP settings applied"},
}

// runApply is the job of every apply kind: render the site as stored now
// under its lock, then switch the object cache if the change was to it.
func (s *Server) runApply(ctx context.Context, r *jobs.Run) error {
	var probe map[string]json.RawMessage
	if json.Unmarshal([]byte(r.Job().Payload), &probe) != nil || probe["ours"] == nil {
		return errUnknownIntent
	}
	var p applyPayload
	if err := r.Payload(&p); err != nil {
		return err
	}
	kind, self := r.Job().Kind, r.Job().ID
	note := func(line string) { r.Progress(-1, line) }
	cur, unlock, err := s.freshSite(r.Job().SiteID)
	defer unlock()
	if errors.Is(err, state.ErrNotFound) {
		return errSiteGone
	}
	if err != nil {
		return err
	}
	if slices.Contains(differing(p.Ours, p.Previous), "aliases") {
		// The names are checked again: none may have gone to another site.
		aliasesMu.Lock()
		taken, err := nameOwners(s.Store, cur.Aliases, cur.ID)
		aliasesMu.Unlock()
		if err != nil {
			return err
		}
		for _, a := range cur.Aliases {
			if owner, ok := taken[a]; ok {
				s.undoChange(cur.ID, p, self, nil, note, false)
				return fmt.Errorf("alias %s is used by %s: the change was put back", a, owner)
			}
		}
	}
	rendered := renderNeeded(kind, p)
	if rendered {
		words := applyWords[kind]
		r.Progress(40, words[0]+cur.Domain)
		var res rpc.ApplyResult
		err := agentrpc.Into(ctx, s.Agent, agentrpc.ApplySiteConfig, rpc.SiteRef{Site: cur}, &res)
		s.logIfErr(s.Store.MarkApplied(cur.ID, cur.DesiredGeneration, err), "MarkApplied")
		if err != nil {
			// The record would claim a configuration that never ran (a PHP
			// version without its FPM on this host, say).
			s.undoChange(cur.ID, p, self, nil, note, false)
			return err
		}
		s.recordRendered(cur, res.Files)
		if p.ReissueCert {
			s.queueReissue(r, cur)
		}
	}
	if kind == kindObjectCache || p.objectCacheChanged() {
		on := cur.Config.ObjectCache
		if on {
			// APCu: the drop-in needs no daemon of its own.
			r.Progress(30, "Installing the APCu object-cache drop-in")
		}
		if err := agentrpc.Into(ctx, s.Agent, agentrpc.WPObjectCache, rpc.WPParams{Site: cur, Enable: on}, nil); err != nil {
			s.logIfErr(s.Store.MarkApplied(cur.ID, cur.DesiredGeneration, err), "MarkApplied")
			s.undoChange(cur.ID, p, self, map[string]bool{"object_cache": true}, note, true)
			return err
		}
		if !rendered && p.OurGen > 0 && cur.AppliedGeneration >= p.PrevGen {
			// The agent made the change itself.
			s.logIfErr(s.Store.MarkApplied(cur.ID, p.OurGen, nil), "MarkApplied")
		}
		if kind == kindObjectCache {
			r.Progress(100, map[bool]string{true: "Object cache enabled", false: "Object cache disabled"}[on])
			return nil
		}
	}
	if !rendered && !p.objectCacheChanged() && p.OurGen > 0 && cur.AppliedGeneration >= p.PrevGen {
		// Only the usage alert thresholds changed: no file depends on
		// them, so the site is as rendered.
		s.logIfErr(s.Store.MarkApplied(cur.ID, p.OurGen, nil), "MarkApplied")
	}
	r.Progress(100, applyWords[kind][1])
	return nil
}

// registerApplyKinds adds the apply job's kinds to the queue.
func (s *Server) registerApplyKinds(q *jobs.Queue) {
	for _, kind := range applyKinds {
		q.Register(jobs.Kind{Name: kind, Run: s.runApply, Rerun: true, Canceled: s.cancelApply})
	}
}
