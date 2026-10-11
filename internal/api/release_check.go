package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cloudground-it/cloudground/internal/version"
)

// The release check (SPEC.md §7.2). The panel tells an operator a newer
// release exists and links to what changed; installing it is a button
// (POST /api/panel/update), never automatic. It asks only when someone
// opens the panel, never on a timer, and fetches a public file without
// sending anything about the server, which keeps the front page's "no
// phone-home" honest; update_check = 0 turns it off.

const (
	changelogURL = "https://github.com/cloudground-it/cloudground/blob/main/CHANGELOG.md"
	// goodFor is how long a successful check answers, failedFor a failed
	// one; refreshGap is the least time between two forced checks.
	goodFor    = 6 * time.Hour
	failedFor  = 10 * time.Minute
	refreshGap = time.Minute
	// feedTimeout bounds one check (version.Feed caps the sizes).
	feedTimeout = 15 * time.Second
)

// releaseCheck caches the newest published release. Its lock is never held
// across the fetch: while one check runs, every other caller gets the
// cached answer at once.
type releaseCheck struct {
	mu      sync.Mutex
	tag     string    // the newest release, "" before a success
	at      time.Time // when the last check ended
	failure string    // why the last check failed, "" when it did not
	busy    bool      // a check is running
	channel string    // the channel the cached answer is for
}

// follow makes the cache answer for channel: a cached answer for another
// channel is dropped, so a switch to beta is seen at the next look.
func (c *releaseCheck) follow(channel string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.channel != channel {
		c.channel, c.tag, c.at, c.failure = channel, "", time.Time{}, ""
	}
}

// latest answers from the cache, checking first when the cached answer is
// stale (or force asks, at most once per refreshGap) and no check runs.
func (c *releaseCheck) latest(force bool, fetch func() (string, string)) (tag string, at time.Time, failure string) {
	c.mu.Lock()
	age := time.Since(c.at)
	var stale bool
	switch {
	case c.busy:
		stale = false
	case force:
		stale = age >= refreshGap
	case c.failure != "":
		stale = age >= failedFor
	default:
		stale = c.tag == "" || age >= goodFor
	}
	if !stale {
		defer c.mu.Unlock()
		return c.tag, c.at, c.failure
	}
	c.busy = true
	asked := c.channel
	c.mu.Unlock()

	got, why := fetch()

	c.mu.Lock()
	defer c.mu.Unlock()
	c.busy = false
	if c.channel != asked {
		// The channel changed during the fetch: its answer is for the
		// old one.
		return c.tag, c.at, c.failure
	}
	c.at, c.failure = time.Now(), why
	if why == "" {
		c.tag = got
	}
	return c.tag, c.at, c.failure
}

// fetchRelease is the check on the stable channel.
func fetchRelease() (tag, failure string) { return fetchChannel(version.Stable) }

// fetchChannel asks the release feed for the newest release of channel and
// returns the version its signed manifest names, or why it could not. The
// signature is checked here too, so a spoofed feed cannot even announce an
// update, and a beta is never announced on the stable channel.
func fetchChannel(channel string) (tag, failure string) {
	ctx, cancel := context.WithTimeout(context.Background(), feedTimeout)
	defer cancel()
	feed := version.Feed{Client: &http.Client{Timeout: feedTimeout}}
	latest, err := feed.Latest(ctx, channel)
	if err != nil {
		return "", "could not reach the release feed"
	}
	m, err := feed.FetchManifest(ctx, latest)
	switch {
	case errors.Is(err, version.ErrNoManifest):
		return "", "could not reach the release feed"
	case errors.Is(err, version.ErrNoSignature):
		return "", "the release feed has no signature"
	case err != nil || version.CheckChannel(channel, m.Version) != nil:
		return "", "the published release is not validly signed"
	}
	return m.Version, ""
}

// changelogAnchor is the id GitHub gives a release's heading in the
// changelog: "## v0.2.10" renders as id="v0210".
func changelogAnchor(tag string) string {
	return "v" + strings.ReplaceAll(strings.TrimPrefix(tag, "v"), ".", "")
}

// updateStatus is GET /api/system/update.
type updateStatus struct {
	Current   string `json:"current"`
	Latest    string `json:"latest,omitempty"`
	Available bool   `json:"update_available"`
	NotesURL  string `json:"notes_url,omitempty"`
	CheckedAt string `json:"checked_at,omitempty"`
	Enabled   bool   `json:"check_enabled"`
	Channel   string `json:"channel"`
	Error     string `json:"error,omitempty"`
}

// updateStatus is the running version and, unless the check is off or the
// build has nowhere to check, the newest release. Only an admin's refresh
// forces a check: anyone else could make the server fetch on every request.
func (s *Server) updateStatus(refresh bool, role string) updateStatus {
	out := updateStatus{Current: version.Version, Channel: version.Stable}
	ps, err := s.Store.PanelSettings()
	if err == nil && !ps.UpdateCheck {
		return out
	}
	if ch, ok := version.ParseChannel(ps.UpdateChannel); err == nil && ok {
		out.Channel = ch
	}
	channel := out.Channel
	if err := version.CanUpdate(); err != nil {
		out.Error = err.Error()
		return out
	}
	out.Enabled = true
	s.updates.follow(channel)
	tag, at, failure := s.updates.latest(refresh && role == roleAdmin, func() (string, string) { return fetchChannel(channel) })
	out.Error, out.Latest = failure, tag
	if !at.IsZero() {
		out.CheckedAt = at.UTC().Format(time.RFC3339)
	}
	if tag != "" {
		out.Available = version.Newer(tag, version.Version)
		out.NotesURL = changelogURL + "#" + changelogAnchor(tag)
	}
	return out
}

func (s *Server) handleUpdateStatus(w http.ResponseWriter, r *http.Request, c *call) {
	respond(w, http.StatusOK, s.updateStatus(r.URL.Query().Get("refresh") == "1", c.User.Role))
}
