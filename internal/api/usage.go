package api

// Per-site resource usage (#18, SPEC.md §11.5): the sites.usage job reads
// every active site's slice counters every five minutes (and its disk use
// every hour), the store keeps a day of samples and 30 days of hours, the
// site's usage page reads them, and a site that stays over a threshold is
// an alert of kind "usage".

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
	"github.com/cloudground-it/cloudground/internal/sysprobe"
)

const (
	// usageDiskEvery is how often the sites' directories are walked: far
	// dearer than the counters, and disk use moves slowly.
	usageDiskEvery = time.Hour
	// usageSustain is how many samples in a row (five minutes apart) a site
	// stays over its CPU or memory threshold before it is an alert: a busy
	// quarter of an hour, not a spike.
	usageSustain = 3
	// usageGap is the longest gap between two samples a rate is taken over;
	// after a longer one (the panel was down) the counters only start again.
	usageGap = 15 * time.Minute
)

// usageTargets are the sites whose usage is read: the active ones.
func usageTargets(sites []state.Site) []rpc.UsageTarget {
	var out []rpc.UsageTarget
	for _, st := range sites {
		if st.Status == state.SiteActive && site.ValidSystemUser(st.SystemUser) {
			out = append(out, rpc.UsageTarget{ID: st.ID, SystemUser: st.SystemUser, Domain: st.Domain})
		}
	}
	return out
}

// usageMinSpacing is the least time between two samples of a site: a rate
// over a few milliseconds says nothing, and two maintenance ticks run back
// to back would otherwise count as two of the usageSustain samples.
const usageMinSpacing = 20 * time.Second

// usageWalks is when each site's directories were last walked. In memory:
// after a restart of the panel each site is walked once more, which is
// cheaper than a column for it.
type usageWalks struct {
	mu   sync.Mutex
	last map[int64]time.Time
}

// runSiteUsage is the sites.usage job.
func (s *Server) runSiteUsage(ctx context.Context, r *jobs.Run) error {
	sites, err := s.Store.ListSites()
	if err != nil {
		return err
	}
	prev, err := s.Store.LatestUsage()
	if err != nil {
		return err
	}
	now := time.Now()
	w := &s.usageWalks
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.last == nil {
		w.last = map[int64]time.Time{}
	}
	var p rpc.SiteUsageParams
	for _, t := range usageTargets(sites) {
		if at := prev[t.ID].SampledAt; !at.IsZero() && now.Sub(at) < usageMinSpacing {
			continue
		}
		t.Disk = now.Sub(w.last[t.ID]) >= usageDiskEvery
		p.Sites = append(p.Sites, t)
	}
	if len(p.Sites) == 0 {
		return nil
	}
	var res rpc.SiteUsageResult
	if err := agentrpc.Into(ctx, s.Agent, agentrpc.SiteUsage, p, &res); err != nil {
		return err
	}
	samples := make([]state.UsageSample, 0, len(res.Sites))
	for _, u := range res.Sites {
		if u.DiskBytes >= 0 {
			w.last[u.ID] = now
		}
		samples = append(samples, usageSample(u, prev[u.ID], now))
	}
	if err := s.Store.RecordUsage(samples); err != nil {
		return err
	}
	s.kickAlerts()
	return nil
}

// usageSample turns one reading into a sample: rates against the previous
// sample when it is recent and the counters only grew (a restarted slice
// starts them again), and the last known disk use carried over between two
// walks.
func usageSample(u rpc.SiteUsage, prev state.UsageSample, now time.Time) state.UsageSample {
	out := state.UsageSample{SiteID: u.ID, SampledAt: now, CPUUsec: u.CPUUsec, IOReadBytes: u.IOReadBytes,
		IOWriteBytes: u.IOWriteBytes, MemBytes: u.MemoryBytes, MemMaxBytes: u.MemoryMaxBytes, Tasks: u.Tasks,
		OOMKills: u.OOMKills, DiskBytes: u.DiskBytes, DiskInodes: u.DiskInodes}
	if dt := now.Sub(prev.SampledAt).Seconds(); !prev.SampledAt.IsZero() && dt > 0 && dt <= usageGap.Seconds() &&
		u.CPUUsec >= prev.CPUUsec && u.IOReadBytes >= prev.IOReadBytes && u.IOWriteBytes >= prev.IOWriteBytes {
		out.CPUCores = float64(u.CPUUsec-prev.CPUUsec) / 1e6 / dt
		out.IOReadBps = float64(u.IOReadBytes-prev.IOReadBytes) / dt
		out.IOWriteBps = float64(u.IOWriteBytes-prev.IOWriteBytes) / dt
	}
	if out.DiskBytes < 0 && prev.DiskBytes >= 0 && prev.SiteID == u.ID {
		out.DiskBytes, out.DiskInodes = prev.DiskBytes, prev.DiskInodes
	}
	return out
}

// cpuAllowance is the CPUs a site may use: its quota, or the whole server.
func cpuAllowance(st state.Site, cpus int) float64 {
	if q := st.Config.Resources.CPUQuotaPct; q > 0 {
		return float64(q) / 100
	}
	return float64(max(cpus, 1))
}

// usageConditions are the usage alerts (SPEC.md §11.5): a site over its CPU
// or memory threshold for usageSustain samples in a row, or whose last
// measured disk use is over its disk threshold. Each alert's fingerprint is
// when its run of samples began, so it keeps its id while it lasts.
func usageConditions(sites []state.Site, samples map[int64][]state.UsageSample, cpus int) []state.Alert {
	var out []state.Alert
	for _, st := range sites {
		a := st.Config.Resources.Alerts
		list := samples[st.ID]
		if st.Status != state.SiteActive || a.Off || len(list) == 0 {
			continue
		}
		subject := ":site:" + strconv.FormatInt(st.ID, 10)
		allow := cpuAllowance(st, cpus)
		cpuOver := func(u state.UsageSample) bool { return u.CPUCores*100 >= float64(a.CPU())*allow }
		if since, ok := sustained(list, cpuOver); ok {
			pct := int(math.Round(list[len(list)-1].CPUCores * 100 / allow))
			out = append(out, state.Alert{Key: "usage.cpu" + subject, SiteID: st.ID, Level: "warning", Fingerprint: since, At: since,
				Message: fmt.Sprintf("the site has used %d%% of its CPU for at least %d minutes", pct, usageSustain*5),
				Text:    state.AlertText{Key: "alert.siteCPU", Args: []string{strconv.Itoa(pct), strconv.Itoa(usageSustain * 5)}}})
		}
		memOver := func(u state.UsageSample) bool {
			return u.MemMaxBytes > 0 && u.MemBytes*100 >= int64(a.Memory())*u.MemMaxBytes
		}
		if since, ok := sustained(list, memOver); ok {
			last := list[len(list)-1]
			pct := int(last.MemBytes * 100 / last.MemMaxBytes)
			out = append(out, state.Alert{Key: "usage.memory" + subject, SiteID: st.ID, Level: "warning", Fingerprint: since, At: since,
				Message: fmt.Sprintf("the site has used %d%% of its memory limit for at least %d minutes", pct, usageSustain*5),
				Text:    state.AlertText{Key: "alert.siteMemory", Args: []string{strconv.Itoa(pct), strconv.Itoa(usageSustain * 5)}}})
		}
		if last := list[len(list)-1]; a.DiskMB > 0 && last.DiskBytes >= int64(a.DiskMB)<<20 {
			mb := strconv.FormatInt(last.DiskBytes>>20, 10)
			out = append(out, state.Alert{Key: "usage.disk" + subject, SiteID: st.ID, Level: "warning",
				Fingerprint: strconv.Itoa(a.DiskMB),
				Message:     fmt.Sprintf("the site uses %s MB of disk, over its threshold of %d MB", mb, a.DiskMB),
				Text:        state.AlertText{Key: "alert.siteDisk", Args: []string{mb, strconv.Itoa(a.DiskMB)}}})
		}
	}
	return out
}

// sustained reports whether the newest usageSustain samples all meet over,
// and when that run began (the oldest sample of it, walking back as far as
// the samples go).
func sustained(list []state.UsageSample, over func(state.UsageSample) bool) (string, bool) {
	if len(list) < usageSustain {
		return "", false
	}
	i := len(list)
	for i > 0 && over(list[i-1]) {
		i--
	}
	if len(list)-i < usageSustain {
		return "", false
	}
	return list[i].SampledAt.UTC().Format(time.RFC3339), true
}

// usageAlerts reads what usageConditions needs.
func (s *Server) usageAlerts(sites []state.Site) ([]state.Alert, error) {
	samples, err := s.Store.UsageSince(time.Now().Add(-state.UsageSampleKeep))
	if err != nil {
		return nil, err
	}
	return usageConditions(sites, samples, runtime.NumCPU()), nil
}

// usagePoint is one point of the usage page's charts.
type usagePoint struct {
	At time.Time `json:"at"`
	// CPUPct is the share of the site's CPU allowance used (its quota, or
	// every CPU); MemPct of its memory limit.
	CPUPct     float64 `json:"cpu_pct"`
	CPUMaxPct  float64 `json:"cpu_max_pct,omitempty"`
	MemBytes   int64   `json:"mem_bytes"`
	MemPct     float64 `json:"mem_pct"`
	IOReadBps  float64 `json:"io_read_bps"`
	IOWriteBps float64 `json:"io_write_bps"`
	DiskBytes  int64   `json:"disk_bytes"`
}

// usageView is GET /api/sites/{id}/usage.
type usageView struct {
	Range  string       `json:"range"`
	CPUs   int          `json:"cpus"`
	Limits site.Limits  `json:"limits"`
	Points []usagePoint `json:"points"`
	// Latest is the newest sample, for the figures above the charts.
	Latest *state.UsageSample `json:"latest,omitempty"`
}

func pct(part, whole float64) float64 {
	if whole <= 0 {
		return 0
	}
	return math.Round(part*1000/whole) / 10
}

func (s *Server) handleSiteUsage(w http.ResponseWriter, r *http.Request, c *call) {
	rng := r.URL.Query().Get("range")
	if rng == "" {
		rng = "24h"
	}
	if rng != "24h" && rng != "30d" {
		respondErr(w, http.StatusBadRequest, "range must be 24h or 30d")
		return
	}
	cpus := runtime.NumCPU()
	f, _ := sysprobe.Probe("/")
	v := usageView{Range: rng, CPUs: cpus, Limits: site.EffectiveLimits(c.Site.Config.Resources, f.MemTotalMB), Points: []usagePoint{}}
	allow := cpuAllowance(c.Site, cpus)
	now := time.Now()
	day, err := s.Store.SiteUsageSince(c.Site.ID, now.Add(-state.UsageSampleKeep))
	if err != nil {
		respondErr(w, http.StatusInternalServerError, "could not read the site's usage")
		return
	}
	if len(day) > 0 {
		v.Latest = &day[len(day)-1]
	}
	if rng == "24h" {
		for _, u := range day {
			v.Points = append(v.Points, usagePoint{At: u.SampledAt, CPUPct: pct(u.CPUCores, allow), MemBytes: u.MemBytes,
				MemPct: pct(float64(u.MemBytes), float64(u.MemMaxBytes)), IOReadBps: u.IOReadBps, IOWriteBps: u.IOWriteBps, DiskBytes: u.DiskBytes})
		}
	} else {
		hours, err := s.Store.SiteUsageHours(c.Site.ID, now.Add(-state.UsageHourKeep))
		if err != nil {
			respondErr(w, http.StatusInternalServerError, "could not read the site's usage")
			return
		}
		for _, h := range hours {
			v.Points = append(v.Points, usagePoint{At: h.Hour, CPUPct: pct(h.CPUCores, allow), CPUMaxPct: pct(h.CPUCoresMax, allow),
				MemBytes: h.MemBytes, MemPct: pct(float64(h.MemBytes), float64(h.MemMaxBytes)), IOReadBps: h.IOReadBps,
				IOWriteBps: h.IOWriteBps, DiskBytes: h.DiskBytes})
		}
	}
	respond(w, http.StatusOK, v)
}
