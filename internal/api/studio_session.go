package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Quarry's own sessions (SPEC.md §6.4). Quarry is a service of its
// own at /quarry, signed in apart from the panel: with a database user's
// name and password, or from the panel through a one-time handoff. Its
// sessions live in this process's memory only (a restart signs everyone
// out of Quarry, not of the panel) and travel in their own cookie.

// The cookie. __Host- makes a browser take it only with Secure, Path=/ and
// no Domain: a site hosted on a sibling subdomain can neither set it (fix
// a victim's session to one of its own) nor replace it. That prefix needs
// Path=/, so the cookie also goes with the panel's requests, which never
// read it, as the panel's goes with Quarry's; only the handoff and the
// sessions opened from the panel read both. Plain-HTTP development uses
// the bare name, as the panel's own cookie does.
const (
	studioCookieName     = "__Host-cg-quarry"
	insecureStudioCookie = "cg-quarry"
)

// Lifetimes and bounds (SPEC.md §6.4).
const (
	// studioIdle ends a session nobody used for this long.
	studioIdle = 30 * time.Minute
	// studioLife ends any session this long after it began.
	studioLife = 12 * time.Hour
	// handoffTTL is how long a handoff from the panel can be exchanged.
	handoffTTL = 60 * time.Second
	// At most this many sessions (and per site); past it the one unused
	// the longest ends. Signed-in database users' sessions, which the
	// agent also holds, have bounds of their own, below these
	// (rpc.DBStudioMaxLogins, rpc.DBStudioMaxSiteLogins): past them a
	// sign-in is refused (reserveLogin).
	maxStudioSessions     = 1024
	maxStudioSiteSessions = 64
	// maxHandoffs waiting to be exchanged.
	maxHandoffs = 1024
)

// The refusals of a Quarry route.
const (
	msgStudioSignIn   = "sign in to Quarry"
	msgStudioPanel    = "the panel session that opened Quarry has ended: open it again from the panel"
	msgStudioReadOnly = "this is a read-only account: Quarry shows it the tables and their structure, not their rows"
	msgHandoffGone    = "this link to Quarry has expired or was used: open Quarry again from the panel"
)

// studioMode is how a session was opened.
type studioMode uint8

const (
	// studioPanel: from the panel, by an account that may reach the site.
	studioPanel studioMode = iota + 1
	// studioLogin: with a database user's own name and password.
	studioLogin
)

func (m studioMode) String() string {
	if m == studioLogin {
		return "login"
	}
	return "panel"
}

// studioSession is one Quarry session.
type studioSession struct {
	key    string // the SHA-256 of the cookie's token
	mode   studioMode
	siteID int64
	// studioLogin: the database user, and the agent's handle of its
	// session (the agent holds the password, sealed; this process never
	// does after the sign-in).
	dbUser, login string
	// studioPanel: the account, and the hash of the panel session that
	// handed off: the Quarry session works only alongside it.
	panelUser int64
	panelKey  string
	// database is the one it opened on.
	database string
	address  string
	started  time.Time

	mu   sync.Mutex
	used time.Time
	// files are the exports and uploads ("exports/<name>",
	// "imports/<name>") a signed-in user made: the only ones it is shown.
	files map[string]bool
	// tasks are the jobs it started, which it may follow.
	tasks map[int64]bool
}

func (st *studioSession) lastUse() time.Time {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.used
}

func (st *studioSession) touch(at time.Time) {
	st.mu.Lock()
	st.used = at
	st.mu.Unlock()
}

// dead reports whether the session is past its idle time or its life.
func (st *studioSession) dead(at time.Time) bool {
	return at.Sub(st.lastUse()) > studioIdle || at.Sub(st.started) > studioLife
}

func (st *studioSession) addFile(dir, name string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.files == nil {
		st.files = map[string]bool{}
	}
	st.files[dir+"/"+name] = true
}

func (st *studioSession) dropFile(dir, name string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.files, dir+"/"+name)
}

// owns reports whether the session may see a file of the site's Quarry
// folders: one opened from the panel sees them all; a signed-in user only
// what it made.
func (st *studioSession) owns(dir, name string) bool {
	if st.mode == studioPanel {
		return true
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.files[dir+"/"+name]
}

func (st *studioSession) addTask(id int64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.tasks == nil {
		st.tasks = map[int64]bool{}
	}
	st.tasks[id] = true
}

func (st *studioSession) hasTask(id int64) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.tasks[id]
}

// handoff is a one-time pass from a panel session into Quarry.
type handoff struct {
	siteID, userID int64
	panelKey       string
	database       string
	expires        time.Time
}

// studioSessions holds the sessions and the handoffs. The zero value is
// ready.
type studioSessions struct {
	mu       sync.Mutex
	byKey    map[string]*studioSession
	handoffs map[string]handoff
	// signingIn counts, per site, the sign-ins that have room reserved
	// and are not yet kept.
	signingIn map[int64]int
	// now is the clock (tests).
	now func() time.Time
}

func (m *studioSessions) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

// add keeps a new session, ending the least recently used past the bounds.
// It returns the sessions it ended (the caller ends their agent side).
func (m *studioSessions) add(st *studioSession) []*studioSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.byKey == nil {
		m.byKey = map[string]*studioSession{}
	}
	at := m.clock()
	var ended []*studioSession
	for k, x := range m.byKey {
		if x.dead(at) {
			delete(m.byKey, k)
			ended = append(ended, x)
		}
	}
	ended = append(ended, m.evict(func(x *studioSession) bool { return x.siteID == st.siteID }, maxStudioSiteSessions-1)...)
	ended = append(ended, m.evict(func(*studioSession) bool { return true }, maxStudioSessions-1)...)
	m.byKey[st.key] = st
	return ended
}

// Why a sign-in is refused when Quarry has no room for its session.
var (
	msgStudioSiteFull = fmt.Sprintf("Quarry already has %d signed-in sessions on this site: sign out of one, or wait until one has gone %d minutes unused", rpc.DBStudioMaxSiteLogins, int(studioIdle/time.Minute))
	msgStudioFull     = fmt.Sprintf("Quarry already has %d signed-in sessions: sign out of one, or wait until one has gone %d minutes unused", rpc.DBStudioMaxLogins, int(studioIdle/time.Minute))
)

// reserveLogin keeps room for one more signed-in session of site: the live
// signed-in sessions and the sign-ins under way count against the bounds
// the agent's pool has too, so the agent never has to end a session the
// API still holds to make room. Without room it returns why; with room,
// release, to call once the session is kept or the sign-in has failed.
// The dead sessions it forgets are returned (the caller ends their agent
// side).
func (m *studioSessions) reserveLogin(siteID int64) (release func(), full string, ended []*studioSession) {
	m.mu.Lock()
	defer m.mu.Unlock()
	at := m.clock()
	site, all := m.signingIn[siteID], 0
	for _, n := range m.signingIn {
		all += n
	}
	for k, x := range m.byKey {
		switch {
		case x.dead(at):
			delete(m.byKey, k)
			ended = append(ended, x)
		case x.mode == studioLogin:
			all++
			if x.siteID == siteID {
				site++
			}
		}
	}
	switch {
	case site >= rpc.DBStudioMaxSiteLogins:
		return nil, msgStudioSiteFull, ended
	case all >= rpc.DBStudioMaxLogins:
		return nil, msgStudioFull, ended
	}
	if m.signingIn == nil {
		m.signingIn = map[int64]int{}
	}
	m.signingIn[siteID]++
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.signingIn[siteID]--; m.signingIn[siteID] <= 0 {
				delete(m.signingIn, siteID)
			}
		})
	}, "", ended
}

// evict ends the least recently used sessions that match until keep are
// left. Callers hold mu.
func (m *studioSessions) evict(match func(*studioSession) bool, keep int) []*studioSession {
	var list []*studioSession
	for _, x := range m.byKey {
		if match(x) {
			list = append(list, x)
		}
	}
	if len(list) <= keep {
		return nil
	}
	sort.Slice(list, func(i, j int) bool { return list[i].lastUse().Before(list[j].lastUse()) })
	list = list[:len(list)-keep]
	for _, x := range list {
		delete(m.byKey, x.key)
	}
	return list
}

// get returns a live session; a dead one is removed and returned as ended.
func (m *studioSessions) get(key string) (st, ended *studioSession) {
	m.mu.Lock()
	defer m.mu.Unlock()
	x, ok := m.byKey[key]
	if !ok {
		return nil, nil
	}
	if x.dead(m.clock()) {
		delete(m.byKey, key)
		return nil, x
	}
	return x, nil
}

// remove forgets a session; it reports whether it was there.
func (m *studioSessions) remove(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.byKey[key]
	delete(m.byKey, key)
	return ok
}

// claim gives st the file dir/name unless another signed-in user's session
// of the same site already has it: the check and the grant are one step,
// so two users starting the same name at once cannot both own it. A
// session opened from the panel sees every file and claims nothing.
func (m *studioSessions) claim(st *studioSession, dir, name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.byKey {
		if x != st && x.siteID == st.siteID && x.mode == studioLogin && x.owns(dir, name) {
			return false
		}
	}
	st.addFile(dir, name)
	return true
}

// own gives st a file the agent has just created for it (an export, made
// with O_EXCL under a name no file had): any other session of the site that
// still lists that name, from a file of the same name since removed, loses
// it.
func (m *studioSessions) own(st *studioSession, dir, name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.byKey {
		if x != st && x.siteID == st.siteID {
			x.dropFile(dir, name)
		}
	}
	st.addFile(dir, name)
}

// count is how many sessions are kept (tests).
func (m *studioSessions) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.byKey)
}

// errTooManyHandoffs is a full handoff table.
var errTooManyHandoffs = errors.New("too many Quarry links are waiting to be opened")

// mint keeps a handoff under the hash of its token.
func (m *studioSessions) mint(key string, h handoff) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.handoffs == nil {
		m.handoffs = map[string]handoff{}
	}
	at := m.clock()
	for k, x := range m.handoffs {
		if !at.Before(x.expires) {
			delete(m.handoffs, k)
		}
	}
	if len(m.handoffs) >= maxHandoffs {
		return errTooManyHandoffs
	}
	m.handoffs[key] = h
	return nil
}

// take spends a handoff: it is gone after the first try, whatever follows.
func (m *studioSessions) take(key string) (handoff, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.handoffs[key]
	delete(m.handoffs, key)
	if !ok || !m.clock().Before(h.expires) {
		return handoff{}, false
	}
	return h, true
}

// ---- the Server's side ----

func (s *Server) studioCookie() string {
	if s.InsecureCookies {
		return insecureStudioCookie
	}
	return studioCookieName
}

func (s *Server) setStudioCookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: s.studioCookie(), Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: !s.InsecureCookies, SameSite: http.SameSiteStrictMode,
	})
}

// startStudio keeps st under a new token and sets its cookie.
func (s *Server) startStudio(w http.ResponseWriter, st *studioSession) {
	token := randomToken(32)
	st.key = state.TokenHash(token)
	at := s.studio.clock()
	st.started, st.used = at, at
	for _, x := range s.studio.add(st) {
		s.closeLogin(x)
	}
	s.setStudioCookie(w, token, int(studioLife/time.Second))
}

// endStudio ends st: forgotten here, its agent side closed, its cookie
// expired.
func (s *Server) endStudio(w http.ResponseWriter, st *studioSession) {
	s.studio.remove(st.key)
	s.closeLogin(st)
	if w != nil {
		s.setStudioCookie(w, "", -1)
	}
}

// closeLogin ends a signed-in user's session on the agent, which forgets
// the password. Best effort: the agent ends an unused one by itself.
func (s *Server) closeLogin(st *studioSession) {
	if st == nil || st.mode != studioLogin || st.login == "" || s.Agent == nil {
		return
	}
	site, err := s.Store.GetSite(st.siteID)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = agentrpc.Into(ctx, s.Agent, agentrpc.DBStudioClose, rpc.DBStudioLoginParams{Site: site, Login: st.login}, nil)
	s.logIfErr(err, "DBStudioClose")
}

// studioFromCookie is the request's live Quarry session, nil without
// one. A request marked as background (SPEC.md §2.2) is not a use.
func (s *Server) studioFromCookie(w http.ResponseWriter, r *http.Request) *studioSession {
	cookie, err := r.Cookie(s.studioCookie())
	if err != nil || cookie.Value == "" {
		return nil
	}
	st, ended := s.studio.get(state.TokenHash(cookie.Value))
	if ended != nil {
		s.endStudio(w, ended)
	}
	if st != nil && r.Header.Get(backgroundHeader) != "1" {
		st.touch(s.studio.clock())
	}
	return st
}

// studioCall authenticates a Quarry route (SPEC.md §6.4, §1.2): its
// session, the site, and for a session opened from the panel the panel
// session it came from, the account's reach and, on a data route, its
// role. It fills c, or returns the refusal.
func (s *Server) studioCall(w http.ResponseWriter, r *http.Request, p policy, c *call) (int, string) {
	st := s.studioFromCookie(w, r)
	if st == nil {
		return http.StatusUnauthorized, msgStudioSignIn
	}
	site, err := s.Store.GetSite(st.siteID)
	if errors.Is(err, state.ErrNotFound) {
		s.endStudio(w, st)
		return http.StatusUnauthorized, msgStudioSignIn
	}
	if err != nil {
		s.logIfErr(err, "load the Quarry session's site")
		return http.StatusInternalServerError, "could not read the site"
	}
	c.Studio, c.Site = st, site
	if st.mode != studioPanel {
		return 0, ""
	}
	token := s.sessionToken(r)
	if token == "" || state.TokenHash(token) != st.panelKey {
		s.endStudio(w, st)
		return http.StatusUnauthorized, msgStudioPanel
	}
	user, err := s.sessionUser(r, token)
	if err != nil || user.ID != st.panelUser {
		s.endStudio(w, st)
		return http.StatusUnauthorized, msgStudioPanel
	}
	c.User = user
	if !c.CanAccess(site.ID) {
		s.endStudio(w, st)
		return http.StatusUnauthorized, msgStudioPanel
	}
	if s.secondFactorPending(user) {
		return http.StatusForbidden, "two-factor authentication is required for admin accounts: enrol in Security before continuing"
	}
	if p == policyStudioData && c.ReadOnly() {
		return http.StatusForbidden, msgStudioReadOnly
	}
	return 0, ""
}

// studioReadOnly reports whether the caller of a Quarry route may only
// read as far as the panel is concerned: an account opened it from the
// panel without the rights to write. A signed-in database user is never
// read-only here; its grants are what refuse its writes.
func (c *call) studioReadOnly() bool {
	return c.Studio != nil && c.Studio.mode == studioPanel && c.ReadOnly()
}

// login is the agent's handle of a signed-in user's session, "" for one
// opened from the panel.
func (c *call) login() string {
	if c.Studio != nil && c.Studio.mode == studioLogin {
		return c.Studio.login
	}
	return ""
}

// statementKey is who a running statement is counted against (SR-T5): an
// account, or a database user signed in to Quarry.
func (c *call) statementKey() string {
	if c.Studio != nil && c.Studio.mode == studioLogin {
		return "db:" + c.Studio.dbUser
	}
	return "u" + strconv.FormatInt(c.User.ID, 10)
}

// studioActor is a signed-in database user in the audit log.
func studioActor(user string) string { return user + " (database user)" }
