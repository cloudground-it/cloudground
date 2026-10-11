package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/cloudground-it/cloudground/internal/state"
	"github.com/cloudground-it/cloudground/internal/version"
)

// Uptime (SPEC.md §11.4, issue #32): every minute the panel requests each
// monitored site from its public address, as a visitor would, and a site
// that fails twice in a row is an alert, which goes out through the
// webhook like every other. The results live in memory: a restart begins
// a new round, and until that round has run the uptime alerts are left as
// they were (neither raised nor cleared).

const (
	defaultUptimeEvery = time.Minute
	// uptimeTimeout bounds one check: a site slower than this is down for
	// its visitors too.
	uptimeTimeout = 10 * time.Second
	// uptimeFailures is how many failed checks in a row make an alert: one
	// lost packet or a reload is not an outage.
	uptimeFailures = 2
	// uptimeParallel is how many checks run at once.
	uptimeParallel = 8
)

// uptimePathRe is the path a check may request: plain segments, no query.
var uptimePathRe = regexp.MustCompile(`^(/[A-Za-z0-9._~-]*)+$`)

// uptimeResult is a site's last check.
type uptimeResult struct {
	CheckedAt time.Time `json:"checked_at"`
	Up        bool      `json:"up"`
	Status    int       `json:"status,omitempty"`
	LatencyMS int64     `json:"latency_ms"`
	Error     string    `json:"error,omitempty"`
	// DownSince is the first failed check of the current streak; Failures
	// how many there have been.
	DownSince *time.Time `json:"down_since,omitempty"`
	Failures  int        `json:"failures,omitempty"`
}

// alerting reports whether the result is an outage worth an alert.
func (r uptimeResult) alerting() bool { return !r.Up && r.Failures >= uptimeFailures }

// uptimeState is every monitored site's last check. ready: a whole round
// has run since the start.
type uptimeState struct {
	mu    sync.Mutex
	ready bool
	sites map[int64]uptimeResult
}

func (u *uptimeState) get(id int64) (uptimeResult, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	r, ok := u.sites[id]
	return r, ok
}

// monitored reports whether the panel checks site.
func monitored(site state.Site) bool {
	return site.Status == state.SiteActive && site.Config.Uptime.Enabled
}

// uptimeURL is what a check of site requests.
func uptimeURL(site state.Site) string {
	path := site.Config.Uptime.Path
	if path == "" {
		path = "/"
	}
	return "https://" + site.Domain + path
}

// uptimeHTTP is the checks' client: no redirect is followed (a 3xx is an
// answer), so a check never leaves the site's own address, and nothing of
// what it reads is kept but the status.
func (s *Server) uptimeHTTP() *http.Client {
	if s.uptimeClient != nil {
		return s.uptimeClient
	}
	return defaultUptimeClient
}

var defaultUptimeClient = &http.Client{
	Timeout:       uptimeTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Transport: &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		TLSHandshakeTimeout: 5 * time.Second,
		DisableKeepAlives:   true,
	},
}

// checkSite requests the site once. A site is up when it answers below
// 500: a 401 or a 404 is the site answering; a refused connection, a
// timeout, a certificate a browser would refuse or a 5xx is not.
func (s *Server) checkSite(ctx context.Context, site state.Site) uptimeResult {
	ctx, cancel := context.WithTimeout(ctx, uptimeTimeout)
	defer cancel()
	start := time.Now()
	r := uptimeResult{CheckedAt: start.UTC()}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uptimeURL(site), nil)
	if err != nil {
		r.Error = "the address is not valid"
		return r
	}
	req.Header.Set("User-Agent", "CloudGround-Uptime/"+version.Version)
	resp, err := s.uptimeHTTP().Do(req)
	r.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		// The cause without the URL, which the alert names already.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		if errors.Is(err, context.DeadlineExceeded) {
			r.Error = fmt.Sprintf("no answer within %d s", int(uptimeTimeout.Seconds()))
		} else {
			r.Error = err.Error()
		}
		return r
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	r.Status = resp.StatusCode
	r.Up = resp.StatusCode < 500
	if !r.Up {
		r.Error = "HTTP " + resp.Status
	}
	return r
}

// uptimeRound checks every monitored site once, at most uptimeParallel at a
// time, and records each result after the site's previous one (the streak
// of failures). Sites no longer monitored are forgotten. It reports whether
// an alert may have changed.
func (s *Server) uptimeRound(ctx context.Context) bool {
	sites, err := s.Store.ListSites()
	if err != nil {
		s.logIfErr(err, "uptime: ListSites")
		return false
	}
	var list []state.Site
	for _, site := range sites {
		if monitored(site) {
			list = append(list, site)
		}
	}
	results := make([]uptimeResult, len(list))
	slots := make(chan struct{}, uptimeParallel)
	var wg sync.WaitGroup
	for i, site := range list {
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer func() { <-slots; wg.Done() }()
			results[i] = s.checkSite(ctx, site)
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return false
	}
	u := &s.uptime
	u.mu.Lock()
	defer u.mu.Unlock()
	changed := !u.ready
	next := make(map[int64]uptimeResult, len(list))
	for i, site := range list {
		r, prev := results[i], u.sites[site.ID]
		if !r.Up {
			r.Failures = prev.Failures + 1
			r.DownSince = prev.DownSince
			if r.DownSince == nil {
				at := r.CheckedAt
				r.DownSince = &at
			}
		}
		changed = changed || r.alerting() != prev.alerting()
		next[site.ID] = r
	}
	for id, prev := range u.sites {
		if _, still := next[id]; !still && prev.alerting() {
			changed = true
		}
	}
	u.sites, u.ready = next, true
	return changed
}

// watchUptime runs a round now, then every minute (or at once when asked),
// until ctx ends, and has the alerts evaluated when a site goes down or
// comes back.
func (s *Server) watchUptime(ctx context.Context) {
	every := s.uptimeEvery
	if every <= 0 {
		every = defaultUptimeEvery
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		if s.uptimeRound(ctx) {
			s.kickAlerts()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.uptimeKick:
		}
	}
}

// kickUptime asks for a round now.
func (s *Server) kickUptime() {
	select {
	case s.uptimeKick <- struct{}{}:
	default:
	}
}

// uptimeConditions are the alerts of the sites down for uptimeFailures
// checks in a row, and false until a round has run. The fingerprint is
// when the outage began: the alert keeps its id while the site stays down,
// whatever the reason says, and an outage after a recovery is a new one.
func (s *Server) uptimeConditions(sites []state.Site) ([]state.Alert, bool) {
	u := &s.uptime
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.ready {
		return nil, false
	}
	var out []state.Alert
	for _, site := range sites {
		r, ok := u.sites[site.ID]
		if !ok || !monitored(site) || !r.alerting() {
			continue
		}
		since := r.DownSince.UTC().Format(time.RFC3339)
		out = append(out, state.Alert{Key: "uptime:site:" + strconv.FormatInt(site.ID, 10), SiteID: site.ID, Level: "error",
			Fingerprint: since, At: since, Message: "the site does not answer from its public address: " + r.Error,
			Text: state.AlertText{Key: "alert.siteDown", Args: []string{r.Error}}})
	}
	return out, true
}

// uptimeView is GET/PUT /api/sites/{id}/uptime's answer.
type uptimeView struct {
	Enabled         bool          `json:"enabled"`
	Path            string        `json:"path"`
	URL             string        `json:"url"`
	IntervalSeconds int           `json:"interval_seconds"`
	Last            *uptimeResult `json:"last,omitempty"`
}

func (s *Server) uptimeViewOf(site state.Site) uptimeView {
	path := site.Config.Uptime.Path
	if path == "" {
		path = "/"
	}
	every := s.uptimeEvery
	if every <= 0 {
		every = defaultUptimeEvery
	}
	v := uptimeView{Enabled: site.Config.Uptime.Enabled, Path: path, URL: uptimeURL(site), IntervalSeconds: int(every.Seconds())}
	if r, ok := s.uptime.get(site.ID); ok && v.Enabled {
		v.Last = &r
	}
	return v
}

func (s *Server) handleGetUptime(w http.ResponseWriter, r *http.Request, c *call) {
	respond(w, http.StatusOK, s.uptimeViewOf(c.Site))
}

// handleSetUptime turns a site's check on or off and sets its path; nothing
// is rendered, so it is a store write, and a round runs at once so the page
// shows a result.
func (s *Server) handleSetUptime(w http.ResponseWriter, r *http.Request, c *call) {
	var req struct {
		Enabled *bool   `json:"enabled"`
		Path    *string `json:"path"`
	}
	if !decodeOr400(w, r, &req) {
		return
	}
	if req.Enabled == nil && req.Path == nil {
		respondErr(w, http.StatusBadRequest, "name enabled, path or both")
		return
	}
	if req.Path != nil && *req.Path != "" && (len(*req.Path) > 200 || !uptimePathRe.MatchString(*req.Path)) {
		replyFailure(w, &problem{kind: probInvalid, msg: "path must start with / and hold only letters, digits and . _ ~ - between slashes",
			fields: map[string]string{"path": "path must start with / and hold only letters, digits and . _ ~ - between slashes"}})
		return
	}
	site, err := s.Store.MutateSiteApplied(c.Site.ID, func(st *state.Site) error {
		if req.Enabled != nil {
			st.Config.Uptime.Enabled = *req.Enabled
		}
		if req.Path != nil {
			st.Config.Uptime.Path = *req.Path
			if st.Config.Uptime.Path == "/" {
				st.Config.Uptime.Path = ""
			}
		}
		return nil
	})
	if errors.Is(err, state.ErrNotFound) {
		respondErr(w, http.StatusNotFound, "site not found")
		return
	}
	if err != nil {
		internalError(w, "MutateSiteApplied", err, "")
		return
	}
	v := s.uptimeViewOf(site)
	s.audit(c.Actor(), "site.uptime", site.Domain, fmt.Sprintf("enabled=%t path=%s", v.Enabled, v.Path))
	s.kickUptime()
	if !v.Enabled {
		// A site no longer checked has no outage to report.
		s.kickAlerts()
	}
	respond(w, http.StatusOK, v)
}
