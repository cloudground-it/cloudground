package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/accounts"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
	"github.com/cloudground-it/cloudground/internal/throttle"
)

// Quarry's ways in (SPEC.md §6.4): a database user's own name and
// password, checked by MariaDB; or a one-time handoff from a panel session.

// The sign-in's limits (SPEC.md §6.4). Each check of a password is a
// sign-in attempt on the database server.
const (
	// Per client network (an IPv4 address, an IPv6 /64), whatever user.
	studioPerNetwork = 20
	// Per IPv6 /48.
	studioPerWide = 80
	// Per network and database user.
	studioPerPair = 6
	// studioQueue attempts for one database user from one network may be
	// queued for their check at once.
	studioQueue = 4
	// studioFloor is the least a refused sign-in takes, so that how long
	// the answer takes does not tell a database user that exists from one
	// that does not.
	studioFloor = 300 * time.Millisecond
	// Bounds of what the sign-in reads before anything is checked.
	maxStudioUser     = 80
	maxStudioPassword = 256
)

const (
	msgStudioBadLogin = "invalid credentials"
	msgStudioTooMany  = "too many attempts — wait a minute and try again"
	msgStudioBusy     = "too many attempts for this database user at once — wait a moment and try again"
)

// studioGate is the sign-in's counters: the windows, the delays and the
// one check per user at a time.
type studioGate struct {
	network, wide, pair *throttle.Window
	// delays: refused sign-ins per database user in 15 minutes, per client
	// network (and /48) and from anywhere, on the panel's schedules
	// (accounts.NewDelays). An attempt sleeps the longer of the two before
	// it queues for its check: a delay, never a lock, and someone failing
	// from one network adds at most accounts.NameMaxDelay for the site's
	// own user signing in from another.
	delays *throttle.Delays
	lanes  *throttle.Lanes
	// sleep waits out a delay or returns ctx's error (tests).
	sleep func(ctx context.Context, d time.Duration) error
}

func newStudioGate() *studioGate {
	return &studioGate{
		network: throttle.New(studioPerNetwork, time.Minute),
		wide:    throttle.New(studioPerWide, time.Minute),
		pair:    throttle.New(studioPerPair, time.Minute),
		delays:  accounts.NewDelays(),
		lanes:   throttle.NewLanes(studioQueue),
	}
}

// allow counts an attempt from o for user against the windows, in order; a
// window that refuses it does not count it in the next.
func (g *studioGate) allow(o accounts.Origin, user string) bool {
	network := o.Network
	if network == "" {
		network = o.Address
	}
	return g.network.Allow(network) && (o.Wide == "" || g.wide.Allow(o.Wide)) && g.pair.Allow(network+"|"+user)
}

// wait sleeps an attempt's delay, or returns ctx's error.
func (g *studioGate) wait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	if g.sleep != nil {
		return g.sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// studioView is a Quarry session as the interface sees it.
type studioView struct {
	Mode string `json:"mode"` // login | panel
	// User is the database user, or the panel account's e-mail.
	User string `json:"user"`
	Site struct {
		ID     int64  `json:"id"`
		Domain string `json:"domain"`
	} `json:"site"`
	Database string `json:"database"`
	// ReadOnly: opened from the panel by a read-only account, which sees
	// the tables and their structure only (SR-T4).
	ReadOnly bool `json:"read_only"`
	// CanWrite: the panel would let the caller write. A database user's
	// grants decide for it, so it is true there.
	CanWrite    bool      `json:"can_write"`
	IdleSeconds int       `json:"idle_seconds"`
	ExpiresAt   time.Time `json:"expires_at"`
	Databases   []string  `json:"databases,omitempty"`
}

func (s *Server) studioViewOf(c *call) studioView {
	st := c.Studio
	v := studioView{Mode: st.mode.String(), Database: st.database, IdleSeconds: int(studioIdle / time.Second),
		ExpiresAt: st.started.Add(studioLife).UTC()}
	v.Site.ID, v.Site.Domain = c.Site.ID, c.Site.Domain
	if st.mode == studioLogin {
		v.User, v.CanWrite = st.dbUser, true
	} else {
		v.User = c.User.Email
		v.ReadOnly = c.ReadOnly()
		v.CanWrite = !v.ReadOnly
	}
	return v
}

// studioDBName is the shape of a database name Quarry passes on: what
// the agent accepts (the server's own schemas are refused there).
var studioDBName = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

// handleStudioLogin signs a database user in (SPEC.md §6.4).
func (s *Server) handleStudioLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		User     string `json:"user"`
		Password string `json:"password"`
		Database string `json:"database"`
	}
	if !decodeOr400(w, r, &in) {
		return
	}
	if in.User == "" || in.Password == "" || len(in.User) > maxStudioUser || len(in.Password) > maxStudioPassword ||
		(in.Database != "" && !studioDBName.MatchString(in.Database)) {
		respondErr(w, http.StatusBadRequest, "give a database user, its password and, if you like, a database")
		return
	}
	g := s.studioGate
	o := clientOrigin(r)
	if !g.allow(o, in.User) {
		respondErr(w, http.StatusTooManyRequests, msgStudioTooMany)
		return
	}
	// The user's delay first, holding no slot; then one check per user at
	// a time, queued per network.
	network := o.Network
	if network == "" {
		network = o.Address
	}
	attempt, err := g.delays.Begin(in.User, network, o.Wide)
	if err != nil {
		respondErr(w, http.StatusTooManyRequests, msgStudioBusy)
		return
	}
	defer attempt.End()
	if err := g.wait(r.Context(), attempt.Wait()); err != nil {
		return
	}
	done, err := g.lanes.Enter(r.Context(), in.User, network)
	if errors.Is(err, throttle.ErrQueueFull) {
		respondErr(w, http.StatusTooManyRequests, msgStudioBusy)
		return
	}
	if err != nil {
		return
	}
	defer done()
	start := time.Now()
	refuse := func() {
		attempt.Fail()
		// The floor is waited out of the queue: it hides the time, and
		// holds up nobody else.
		done()
		s.audit(studioActor(auditName(in.User)), "quarry.login_failed", "quarry", o.Address)
		if d := studioFloor - time.Since(start); d > 0 {
			select {
			case <-time.After(d):
			case <-r.Context().Done():
			}
		}
		respondErr(w, http.StatusUnauthorized, msgStudioBadLogin)
	}
	if !site.ValidDBName(in.User) {
		refuse()
		return
	}
	siteID, err := s.Store.DBUserSite(in.User)
	if errors.Is(err, state.ErrNotFound) {
		refuse()
		return
	}
	if err != nil {
		internalError(w, "DBUserSite", err, "")
		return
	}
	target, err := s.Store.GetSite(siteID)
	if err != nil {
		internalError(w, "GetSite", err, "could not read the site")
		return
	}
	// Room for the session is checked before the agent keeps one, which
	// would otherwise end another to make room. Without room, the
	// password is still checked (keeping nothing): the reason is given
	// only to someone the server takes, and a refusal reads the same for
	// a full site as for any other.
	release, full, dead := s.studio.reserveLogin(target.ID)
	for _, x := range dead {
		s.closeLogin(x)
	}
	if release != nil {
		defer release()
	}
	var res rpc.DBStudioOpenResult
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.DBStudioOpen, rpc.DBStudioOpenParams{
		Site: target, User: in.User, Password: in.Password, CheckOnly: full != "",
	}, &res); err != nil {
		respondAgentErr(w, 0, "", err)
		return
	}
	if res.Refused || (res.Login == "" && full == "") {
		refuse()
		return
	}
	attempt.Succeed()
	if full != "" {
		respondErr(w, http.StatusTooManyRequests, full)
		return
	}
	st := &studioSession{mode: studioLogin, siteID: target.ID, dbUser: in.User, login: res.Login, database: in.Database, address: o.Address}
	if st.database == "" && len(res.Databases) > 0 {
		st.database = res.Databases[0]
		for _, d := range res.Databases {
			if d == target.Config.Database.Name {
				st.database = d
			}
		}
	}
	s.startStudio(w, st)
	s.audit(studioActor(in.User), "quarry.login", target.Domain, o.Address)
	v := s.studioViewOf(&call{s: s, Studio: st, Site: target})
	v.Databases = res.Databases
	if v.Databases == nil {
		v.Databases = []string{}
	}
	respond(w, http.StatusOK, v)
}

// auditName is a name a stranger typed, safe for the audit log.
func auditName(name string) string {
	var b strings.Builder
	for _, c := range name {
		if c < 0x20 || c == 0x7f {
			c = '?'
		}
		b.WriteRune(c)
	}
	return b.String()
}

// handleStudioHandoffMint gives a panel session a one-time pass into DB
// Studio for the site (SPEC.md §6.4).
func (s *Server) handleStudioHandoffMint(w http.ResponseWriter, r *http.Request, c *call) {
	var in struct {
		Database string `json:"database"`
	}
	if !decodeOr400(w, r, &in) {
		return
	}
	database := in.Database
	if database != "" {
		if _, err := s.Store.SiteDatabaseByName(c.Site.ID, database); errors.Is(err, state.ErrNotFound) {
			respondErr(w, http.StatusNotFound, "this site has no database \""+database+"\"")
			return
		} else if err != nil {
			internalError(w, "SiteDatabaseByName", err, "")
			return
		}
	} else if localDB(c.Site) {
		database = c.Site.Config.Database.Name
	}
	token := randomToken(32)
	err := s.studio.mint(state.TokenHash(token), handoff{
		siteID: c.Site.ID, userID: c.User.ID, panelKey: state.TokenHash(s.sessionToken(r)),
		database: database, expires: s.studio.clock().Add(handoffTTL),
	})
	if err != nil {
		respondErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	s.audit(c.Actor(), "quarry.handoff", c.Site.Domain, database)
	respond(w, http.StatusCreated, map[string]any{
		"token": token, "expires_in": int(handoffTTL / time.Second), "url": "/quarry/handoff#" + token,
	})
}

// handleStudioHandoff exchanges a handoff for a Quarry session: once,
// before it expires, alongside the panel session it was minted for, by an
// account that still reaches the site.
func (s *Server) handleStudioHandoff(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"token"`
	}
	if !decodeOr400(w, r, &in) {
		return
	}
	h, ok := s.studio.take(state.TokenHash(in.Token))
	token := s.sessionToken(r)
	if !ok || token == "" || state.TokenHash(token) != h.panelKey {
		respondErr(w, http.StatusUnauthorized, msgHandoffGone)
		return
	}
	user, err := s.sessionUser(r, token)
	if err != nil || user.ID != h.userID {
		respondErr(w, http.StatusUnauthorized, msgHandoffGone)
		return
	}
	target, err := s.Store.GetSite(h.siteID)
	c := &call{s: s, User: user}
	if err != nil || !c.CanAccess(target.ID) {
		respondErr(w, http.StatusUnauthorized, msgHandoffGone)
		return
	}
	if s.secondFactorPending(user) {
		respondErr(w, http.StatusForbidden, msgSecondFactorRequired)
		return
	}
	st := &studioSession{mode: studioPanel, siteID: target.ID, panelUser: user.ID, panelKey: h.panelKey,
		database: h.database, address: clientIP(r)}
	s.startStudio(w, st)
	s.audit(user.Email, "quarry.open", target.Domain, h.database)
	c.Studio, c.Site = st, target
	respond(w, http.StatusOK, s.studioViewOf(c))
}

// handleStudioSession answers the request's Quarry session, or
// {"session": null}: never 401, it is the page's first request.
func (s *Server) handleStudioSession(w http.ResponseWriter, r *http.Request) {
	c := &call{s: s}
	if status, _ := s.studioCall(w, r, policyStudio, c); status != 0 {
		respond(w, http.StatusOK, map[string]any{"session": nil})
		return
	}
	respond(w, http.StatusOK, map[string]any{"session": s.studioViewOf(c)})
}

// handleStudioLogout ends the session: here, on the agent (which forgets
// the password), and its cookie.
func (s *Server) handleStudioLogout(w http.ResponseWriter, r *http.Request, c *call) {
	s.endStudio(w, c.Studio)
	s.audit(c.Actor(), "quarry.logout", c.Site.Domain, "")
	respond(w, http.StatusOK, map[string]string{"status": "signed out"})
}
