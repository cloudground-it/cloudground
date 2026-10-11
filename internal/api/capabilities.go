package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/site"
)

// The machine's facts (PHP versions, services, the sites' root, …) are the
// agent's: the API asks for them with Capabilities and keeps only its
// policies over them (internal/rpc SPEC.md §4).

// capsTTL is how long an answer is used before the agent is asked again: a
// PHP version installed by hand, or an agent restarted by hand onto a new
// build, shows within this time.
const capsTTL = 30 * time.Second

// capsRetry is how long a failed ask stands for every caller: an agent that
// is down is asked again after this, not by each waiting request in turn.
const capsRetry = 5 * time.Second

// After a self-update the agent restarts on its own schedule, so the answer
// is refreshed every capsRestartTTL until its start time changes, for at
// most capsRestartWatch.
const (
	capsRestartTTL   = 2 * time.Second
	capsRestartWatch = 5 * time.Minute
)

// capsCache holds the agent's last answer, and the one ask in flight.
type capsCache struct {
	mu       sync.Mutex
	val      rpc.Capabilities
	have     bool      // val holds an answer
	at       time.Time // when it came; zero: ask again before using it
	failedAt time.Time // the last ask that failed, while it stands
	failErr  error
	flight   *capsFlight
	// watchUntil: a self-update was asked for; until the agent reports a
	// new start time (or this passes), answers live capsRestartTTL.
	watchUntil time.Time
	watchFrom  time.Time // the start time before the update
}

// capsFlight is one ask of the agent, shared by every caller that comes
// while it runs.
type capsFlight struct {
	done chan struct{}
	val  rpc.Capabilities
	err  error
}

// capabilities returns the agent's capabilities, asking it when the cached
// answer is missing or stale. Concurrent callers share one ask, and each
// stops waiting when its own ctx ends. When the agent cannot answer, the
// last answer (however old) is used; with none, the error is returned, and
// the failure stands for capsRetry.
func (s *Server) capabilities(ctx context.Context) (rpc.Capabilities, error) {
	c := &s.caps
	c.mu.Lock()
	now := time.Now()
	ttl := capsTTL
	if now.Before(c.watchUntil) {
		ttl = capsRestartTTL
	}
	if c.have && !c.at.IsZero() && now.Sub(c.at) < ttl {
		v := c.val
		c.mu.Unlock()
		return v, nil
	}
	if !c.failedAt.IsZero() && now.Sub(c.failedAt) < capsRetry {
		v, have, err := c.val, c.have, c.failErr
		c.mu.Unlock()
		if have {
			return v, nil
		}
		return rpc.Capabilities{}, err
	}
	f := c.flight
	if f == nil {
		f = &capsFlight{done: make(chan struct{})}
		c.flight = f
		go s.askCapabilities(f)
	}
	c.mu.Unlock()
	select {
	case <-f.done:
	case <-ctx.Done():
		return rpc.Capabilities{}, ctx.Err()
	}
	if f.err != nil {
		c.mu.Lock()
		v, have := c.val, c.have
		c.mu.Unlock()
		if have {
			return v, nil
		}
		return rpc.Capabilities{}, f.err
	}
	return f.val, nil
}

// askCapabilities runs one flight. It does not take any caller's context:
// a caller that gives up must not fail the others (the method table bounds
// the call).
func (s *Server) askCapabilities(f *capsFlight) {
	var v rpc.Capabilities
	err := agentrpc.Into(context.Background(), s.Agent, agentrpc.Capabilities, agentrpc.None{}, &v)
	c := &s.caps
	c.mu.Lock()
	if err != nil {
		c.failedAt, c.failErr = time.Now(), err
		if c.have {
			s.capsLog().Warn("agent capabilities: using the last answer", "err", err)
		}
	} else {
		if c.have && !v.StartedAt.Equal(c.val.StartedAt) {
			s.capsLog().Info("the agent restarted; capabilities refreshed", "version", v.AgentVersion)
		}
		if !c.watchUntil.IsZero() && !v.StartedAt.Equal(c.watchFrom) {
			c.watchUntil = time.Time{} // the restart was seen
		}
		c.val, c.have, c.at = v, true, time.Now()
		c.failedAt, c.failErr = time.Time{}, nil
	}
	f.val, f.err = v, err
	c.flight = nil
	c.mu.Unlock()
	close(f.done)
}

func (s *Server) capsLog() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// forgetCapabilities makes the next use ask the agent again (keeping the
// answer for when the agent cannot reply).
func (s *Server) forgetCapabilities() {
	s.caps.mu.Lock()
	s.caps.at, s.caps.failedAt = time.Time{}, time.Time{}
	s.caps.mu.Unlock()
}

// expectAgentRestart forgets the answer and keeps refreshing it briefly
// until the agent reports a new start time: a self-update restarts the agent
// after it has answered.
func (s *Server) expectAgentRestart() {
	c := &s.caps
	c.mu.Lock()
	c.at, c.failedAt = time.Time{}, time.Time{}
	c.watchFrom = c.val.StartedAt
	c.watchUntil = time.Now().Add(capsRestartWatch)
	c.mu.Unlock()
}

// errNoSitesRoot is an agent that did not say where sites live.
var errNoSitesRoot = errors.New("the agent did not report where sites live")

// siteRoot is where the site for domain lives, by the agent's layout.
func (s *Server) siteRoot(ctx context.Context, domain string) (string, error) {
	c, err := s.capabilities(ctx)
	if err != nil {
		return "", err
	}
	if c.Paths.SitesRoot == "" {
		return "", errNoSitesRoot
	}
	return filepath.Join(c.Paths.SitesRoot, domain), nil
}

// phpVersionError is why version cannot be given to a site, or "" when it
// can: a well-formed version this server has installed.
func phpVersionError(c rpc.Capabilities, version string) string {
	if !site.ValidPHPVersion(version) {
		return "unsupported PHP version"
	}
	if !c.HasPHP(version) {
		return "PHP " + version + " is not installed on this server"
	}
	return ""
}

// defaultPHP is the version a new site gets when the request names none:
// the configured default (CLOUDGROUND_PHP_VERSION, set by the installer)
// when this server has it, else the newest one installed.
func (s *Server) defaultPHP(c rpc.Capabilities) string {
	v := s.DefaultPHP
	if v == "" {
		v = "8.4"
	}
	if !c.HasPHP(v) {
		v = c.PHPVersions[0] // HasPHP is false only with a non-empty list
	}
	return v
}

// capabilitiesView is what GET /api/system/capabilities shows: the facts
// the pages need to offer only what the server can do.
type capabilitiesView struct {
	PHPVersions []string                `json:"php_versions"`
	DefaultPHP  string                  `json:"default_php"`
	Services    []rpc.ServiceCapability `json:"services"`
	SitesRoot   string                  `json:"sites_root"`
	HTTP3       bool                    `json:"http3"`
	// TimeZone is the panel's own zone ("UTC", "CEST"), the one a backup
	// policy's hour is in.
	TimeZone string `json:"time_zone"`
}

// timeZoneName is now's zone abbreviation, or its offset when the zone has
// none ("+0530").
func timeZoneName(now time.Time) string {
	name, _ := now.Zone()
	if name == "" || name[0] == '+' || name[0] == '-' {
		return now.Format("-0700")
	}
	return name
}

func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	c, err := s.capabilities(r.Context())
	if err != nil {
		respondAgentErr(w, 0, "", err)
		return
	}
	php := c.PHPVersions
	if php == nil {
		php = []string{}
	}
	services := []rpc.ServiceCapability{}
	for _, sv := range c.Services {
		if sv.Listed {
			services = append(services, sv)
		}
	}
	respond(w, http.StatusOK, capabilitiesView{
		PHPVersions: php,
		DefaultPHP:  s.defaultPHP(c),
		Services:    services,
		SitesRoot:   c.Paths.SitesRoot,
		HTTP3:       c.Features.HTTP3,
		TimeZone:    timeZoneName(time.Now()),
	})
}
