package api

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/api/events"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Stream tuning; tests shorten them through the Server fields.
const (
	defaultHeartbeat = 25 * time.Second
	// viewerMaxAge is how old the stream's idea of its user (role, sites,
	// session) may be when it delivers an event.
	viewerMaxAge = 5 * time.Second
	// streamWriteTimeout bounds one write to a client that stopped reading.
	streamWriteTimeout = 30 * time.Second
	streamBuffer       = 256
)

// sameOriginRead refuses a request another site's page made: the stream
// carries the panel's state, and a cookie still goes along with a request
// from a sibling subdomain (the sites the panel hosts can be one).
func sameOriginRead(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || origin == "null" || !strings.EqualFold(u.Host, r.Host) {
			return false
		}
	}
	return true
}

// viewer is who a stream delivers to, re-read from the session as it goes
// (internal/api/events/SPEC.md §5).
type viewer struct {
	s       *Server
	token   string
	bearer  bool // token is an API token, not a session
	user    state.User
	sites   map[int64]bool // an operator's sites, staging copies included
	checked time.Time
	global  bool // counted in Server.globalStreams
}

// refresh re-reads the session, the user's role and an operator's sites.
// false: the stream must end.
func (v *viewer) refresh() bool {
	var u state.User
	var err error
	if v.bearer {
		u, _, err = v.s.Store.UserForAPIToken(v.token) // a revoked token ends the stream
	} else {
		u, err = v.s.Store.PeekSessionUser(v.token)
	}
	if err != nil || v.s.secondFactorPending(u) {
		return false
	}
	v.user = u
	v.sites = nil
	if u.Role == "operator" {
		sites, err := v.s.Store.ListSitesForUser(u.ID)
		if err != nil {
			return false
		}
		v.sites = make(map[int64]bool, len(sites))
		for _, site := range sites {
			v.sites[site.ID] = true
		}
	}
	v.checked = time.Now()
	v.setGlobal(u.Role == "admin" || u.Role == "readonly")
	return true
}

func (v *viewer) setGlobal(g bool) {
	if g == v.global {
		return
	}
	v.global = g
	if g {
		v.s.globalStreams.Add(1)
	} else {
		v.s.globalStreams.Add(-1)
	}
}

// may reports whether the viewer may see ev. Fails closed: an unknown role
// sees nothing.
func (v *viewer) may(ev events.Event) bool {
	if ev.Internal() {
		return false
	}
	switch v.user.Role {
	case "admin", "readonly":
		return true
	case "operator":
		return ev.SiteID != 0 && v.sites[ev.SiteID]
	}
	return false
}

// dataInt reads an integer field of an event published in-process.
func dataInt(ev events.Event, key string) int64 {
	m, _ := ev.Data.(map[string]any)
	n, _ := m[key].(int64)
	return n
}

func dataString(ev events.Event, key string) string {
	m, _ := ev.Data.(map[string]any)
	str, _ := m[key].(string)
	return str
}

// handleEvents streams the event bus as Server-Sent Events, filtered to what
// the signed-in user may see (internal/api/events/SPEC.md §5).
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if !sameOriginRead(r) {
		respondErr(w, http.StatusForbidden, "cross-origin request refused")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok || s.Events == nil {
		respondErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	// The credential the router authenticated: an API token when the request
	// carries one (a cookie next to it is ignored, as everywhere), else the
	// session cookie.
	v := &viewer{s: s}
	if secret, ok := bearerToken(r); ok {
		v.token, v.bearer = secret, true
	} else if cookie, err := r.Cookie(s.sessionCookieName()); err == nil {
		v.token = cookie.Value
	} else {
		respondErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	defer v.setGlobal(false)
	if !v.refresh() {
		respondErr(w, http.StatusUnauthorized, "authentication required")
		return
	}

	// The panel's http.Server has a 10 min WriteTimeout, which would cut
	// the stream: move it along before every write. A read deadline left in
	// place would cancel the request's context once it passed (the
	// connection's background read fails); the server sets none past the
	// headers, and this lifts any all the same.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Now().Add(streamWriteTimeout))

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	// nginx: never buffer this response, whatever the location says.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	write := func(id string, payload []byte) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(streamWriteTimeout))
		var err error
		if id != "" {
			_, err = fmt.Fprintf(w, "id: %s\ndata: %s\n\n", id, payload)
		} else {
			_, err = fmt.Fprintf(w, "data: %s\n\n", payload)
		}
		if err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	resync := func() bool { return write("", events.Marshal(events.Resync, nil, time.Now())) }

	// deliver applies an event to the viewer and writes it if the viewer may
	// see it. false: the stream must end.
	deliver := func(ev events.Event) bool {
		switch ev.Type {
		case events.SessionRevoked, events.UserChanged:
			if uid := dataInt(ev, "user_id"); uid == 0 || uid == v.user.ID {
				if !v.refresh() {
					return false
				}
			}
		case events.SiteChanged:
			// A staging copy of an assigned site is the operator's too.
			if v.user.Role == "operator" && !v.sites[ev.SiteID] && dataString(ev, "reason") == "created" {
				if !v.refresh() {
					return false
				}
			}
		}
		if ev.Internal() {
			return true
		}
		if time.Since(v.checked) > viewerMaxAge && !v.refresh() {
			return false
		}
		if !v.may(ev) {
			return true
		}
		if !write(ev.ID, ev.JSON) {
			return false
		}
		if ev.Type == events.SiteDeleted && v.sites != nil {
			delete(v.sites, ev.SiteID)
		}
		return true
	}

	lastID := r.Header.Get("Last-Event-ID")
	if lastID == "" {
		// A client that reconnects with a new EventSource cannot set the
		// header; it passes the ID it last saw here.
		lastID = r.URL.Query().Get("last_event_id")
	}
	sub, replay, resumed := s.Events.Subscribe(lastID, streamBuffer)
	defer func() { sub.Close() }()
	// The browser's own reconnect delay, should the UI not take over.
	_, _ = fmt.Fprint(w, "retry: 5000\n\n")
	flusher.Flush()
	if !resumed {
		if !resync() {
			return
		}
	}
	for _, ev := range replay {
		if !deliver(ev) {
			return
		}
	}

	every := s.heartbeat
	if every <= 0 {
		every = defaultHeartbeat
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-s.Shutdown:
			return
		case <-r.Context().Done():
			return
		case <-ticker.C:
			// The session may have ended with nothing published since.
			if !v.refresh() || !write("", events.Marshal(events.Ping, nil, time.Now())) {
				return
			}
		case ev, ok := <-sub.C():
			if !ok {
				if !sub.Dropped() {
					return
				}
				// Too slow to keep up: start over from now, and have the
				// browser refetch what it shows.
				sub, _, _ = s.Events.Subscribe("", streamBuffer)
				if !resync() {
					return
				}
				continue
			}
			if !deliver(ev) {
				return
			}
		}
	}
}
