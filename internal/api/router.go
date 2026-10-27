package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// The router (SPEC.md §1). Every route is a row of Server.routeTable; the
// row says who may call it, what its {id} loads, how its body may be typed
// and how large it may be. The pipeline below enforces all of it before the
// handler runs, so a handler starts with a caller it may serve and the
// resource it asked for.

// policy is who may call a route (SPEC.md §1.3). The zero value is no
// policy at all, and a row with it does not get registered.
type policy uint8

const (
	policyUnset policy = iota
	// policyOpen: anyone, signed in or not.
	policyOpen
	// policyToken: the handler authenticates a per-site bearer token.
	policyToken
	// policySignedIn: every signed-in account.
	policySignedIn
	// policyGlobal: server-wide data, for every account but an operator.
	policyGlobal
	// policyManage: admins, and operators on their own sites.
	policyManage
	// policyAdmin: admins only.
	policyAdmin
	// policyStudio: a Quarry session (its own cookie, SPEC.md §6.4),
	// never a panel session or an API token.
	policyStudio
	// policyStudioData: a Quarry session that may read the rows: a
	// database user signed in with its password (the server's grants
	// decide), or one opened from the panel by an admin or an operator
	// (SR-T4: a read-only account sees tables and structure only).
	policyStudioData
	// policyMCP: the MCP server's OAuth and /mcp rows (SPEC.md §16): the
	// handler authenticates what it needs, and answers 404 while the MCP
	// server is off.
	policyMCP
)

// policies lists every valid policy, for the tests and the docs.
var policies = []policy{policyOpen, policyToken, policySignedIn, policyGlobal, policyManage, policyAdmin, policyStudio, policyStudioData, policyMCP}

// marker is the policy's name in docs/api.md.
func (p policy) marker() string {
	switch p {
	case policyOpen:
		return "open"
	case policyToken:
		return "site token"
	case policySignedIn:
		return "auth"
	case policyGlobal:
		return "global"
	case policyManage:
		return "manage"
	case policyAdmin:
		return "admin"
	case policyStudio:
		return "dbstudio"
	case policyStudioData:
		return "dbstudio data"
	case policyMCP:
		return "mcp"
	}
	return ""
}

// needsSession reports whether the router signs the caller in with a panel
// session or an API token.
func (p policy) needsSession() bool {
	return p == policySignedIn || p == policyGlobal || p == policyManage || p == policyAdmin
}

// studio reports whether the route takes a Quarry session.
func (p policy) studio() bool { return p == policyStudio || p == policyStudioData }

// admits reports whether an account with role may call the route, and the
// refusal's message when it may not. An operator refused an admin route is
// told it needs an administrator, by what (the row's AdminOnly) when the
// row names it: "this is a read-only account" is a read-only account's
// refusal, and an operator is not one.
func (p policy) admits(role, adminOnly string) (bool, string) {
	switch p {
	case policyGlobal:
		return role != roleOperator, "server-wide access required"
	case policyManage:
		return role == roleAdmin || role == roleOperator, "site management access required"
	case policyAdmin:
		if role == roleOperator {
			return false, operatorRefusal(adminOnly)
		}
		return role == roleAdmin, "this is a read-only account"
	}
	return true, ""
}

// operatorRefusal is an admin route's refusal of an operator.
func operatorRefusal(adminOnly string) string {
	if adminOnly == "" {
		adminOnly = "do this"
	}
	return "only administrators can " + adminOnly
}

// The three account roles.
const (
	roleAdmin    = "admin"
	roleOperator = "operator"
	roleReadonly = "readonly"
)

// loader is what a route's {id} names (SPEC.md §1.4).
type loader uint8

const (
	loadNone loader = iota
	loadSite
	loadBackup
	loadDeployment
	loadCron
	loaderEnd // not a loader: one past the last
)

// noun is what the loader's refusals call the resource.
func (l loader) noun() string {
	switch l {
	case loadSite:
		return "site"
	case loadBackup:
		return "backup"
	case loadDeployment:
		return "deployment"
	case loadCron:
		return "cron job"
	}
	return ""
}

// bodyRule is the media types a state-changing request may carry.
type bodyRule uint8

const (
	// bodyJSON: application/json only.
	bodyJSON bodyRule = iota
	// bodyUpload: application/json or raw application/octet-stream. Like
	// JSON, a cross-site form cannot send it without a CORS preflight.
	bodyUpload
)

// defaultMaxBody bounds a request body when the row names no limit.
const defaultMaxBody = 4 << 20

// handlerFunc is a route's handler.
type handlerFunc func(w http.ResponseWriter, r *http.Request, c *call)

// plain adapts a handler that needs neither the caller nor a resource.
func plain(h http.HandlerFunc) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request, _ *call) { h(w, r) }
}

// route is one row of the route table (SPEC.md §1.1).
type route struct {
	Method, Path string
	Query        string
	Policy       policy
	Load         loader
	Body         bodyRule
	MaxBody      int64
	Enrol        bool
	// SessionOnly: a signed-in session only, never an API token (403): the
	// routes that manage accounts, sessions, second factors, users and
	// tokens, so a leaked token cannot make itself permanent (apitokens.go).
	SessionOnly    bool
	SameOriginRead bool
	Loopback       bool
	// Statements: the call runs a database statement while it waits; at
	// most maxStatementsPerUser at once per account and
	// maxStatementsPerSite per site, 429 beyond (statement_limit.go).
	Statements bool
	// AdminOnly, on an admin row, is what an operator is told it cannot
	// do: "only administrators can <AdminOnly>" (default "do this").
	AdminOnly    string
	Section, Doc string
	Handle       handlerFunc
}

// pattern is the row's ServeMux pattern.
func (rt route) pattern() string {
	if rt.Method == "" {
		return rt.Path
	}
	return rt.Method + " " + rt.Path
}

// valid reports what is wrong with a row, or nil.
func (rt route) valid() error {
	switch {
	case rt.Path == "":
		return errors.New("no path")
	case rt.Policy == policyUnset || rt.Policy.marker() == "":
		return fmt.Errorf("%s: no policy", rt.pattern())
	case rt.Handle == nil:
		return fmt.Errorf("%s: no handler", rt.pattern())
	case rt.Load >= loaderEnd:
		return fmt.Errorf("%s: unknown loader", rt.pattern())
	case rt.Load != loadNone && !strings.Contains(rt.Path, "{id}"):
		return fmt.Errorf("%s: a loader needs {id}", rt.pattern())
	case rt.Load != loadNone && !rt.Policy.needsSession():
		return fmt.Errorf("%s: a loader needs a signed-in caller", rt.pattern())
	case rt.Statements && rt.Load != loadSite && !rt.Policy.studio():
		return fmt.Errorf("%s: a statement limit needs the site", rt.pattern())
	case rt.SessionOnly && !rt.Policy.needsSession():
		return fmt.Errorf("%s: SessionOnly needs a signed-in caller", rt.pattern())
	}
	return nil
}

// call is what the router hands a handler: who is calling, and what the
// route's loader found.
type call struct {
	s *Server
	// User is the signed-in caller; zero on open and token routes.
	User state.User
	// Token is the API token the caller authenticated with; zero for a
	// session cookie.
	Token state.APIToken
	// Site is the site the {id} names, or the one the backup, release or
	// cron job belongs to.
	Site       state.Site
	Backup     state.Backup
	Deployment state.Deployment
	Cron       state.CronJob
	// Studio is the Quarry session of a studio route (its Site is the
	// session's site; User is the panel account of one opened from the
	// panel).
	Studio *studioSession
	// MCP is the assistant a call is made for (SPEC.md §16.7): the
	// router's pipeline runs the tool's route as its account, which then
	// counts as an API token does.
	MCP *mcpCaller
}

// Actor is the caller's identity in the audit log.
func (c *call) Actor() string {
	if c.Studio != nil && c.Studio.mode == studioLogin {
		return studioActor(c.Studio.dbUser)
	}
	if c.User.Email == "" {
		return "unknown"
	}
	if c.MCP != nil {
		return fmt.Sprintf("%s (via MCP %q)", c.User.Email, c.MCP.client)
	}
	if c.ViaToken() {
		return fmt.Sprintf("%s (API token %q #%d)", c.User.Email, c.Token.Name, c.Token.ID)
	}
	return c.User.Email
}

// ViaToken reports whether the caller authenticated with an API token, or
// is an assistant (SPEC.md §16.7), which the routes treat the same way.
func (c *call) ViaToken() bool { return c.Token.ID != 0 || c.MCP != nil }

// CanAccess reports whether the caller may reach the site: an operator
// only its assigned sites and their staging copies, every other account
// every site.
func (c *call) CanAccess(siteID int64) bool {
	if c.User.Role != roleOperator {
		return c.User.Role != ""
	}
	ok, err := c.s.Store.UserCanAccessSite(c.User.ID, siteID)
	return err == nil && ok
}

// ReadOnly reports whether the caller may only read: neither an admin nor an
// operator.
func (c *call) ReadOnly() bool {
	return c.User.Role != roleAdmin && c.User.Role != roleOperator
}

// Routes builds the panel's handler from the route table.
func (s *Server) Routes() http.Handler {
	s.Init()
	mux := http.NewServeMux()
	var patterns []string
	for _, rt := range s.routeTable() {
		if err := rt.valid(); err != nil {
			panic("api: route table: " + err.Error())
		}
		mux.Handle(rt.pattern(), s.serve(rt))
		patterns = append(patterns, rt.pattern())
	}
	s.routePatterns = patterns
	return s.withCommonHeaders(mux)
}

// ConnectorRoutes is the plaintext loopback listener's surface: only the
// rows marked Loopback (the connector's purge and the liveness probe). A
// local process, a compromised site's PHP say, reaches nothing else.
func (s *Server) ConnectorRoutes() http.Handler {
	s.Init()
	mux := http.NewServeMux()
	for _, rt := range s.routeTable() {
		if rt.Loopback {
			mux.Handle(rt.pattern(), s.serve(rt))
		}
	}
	return s.withCommonHeaders(mux)
}

// mcpMux serves the route table to the MCP server's tools (SPEC.md §16.7),
// without the common headers: their calls come from inside the panel.
func (s *Server) mcpMux() http.Handler {
	s.mcpMuxOnce.Do(func() {
		mux := http.NewServeMux()
		for _, rt := range s.routeTable() {
			mux.Handle(rt.pattern(), s.serve(rt))
		}
		s.mcpHandler = mux
	})
	return s.mcpHandler
}

// MailRoutes is the sites' mail gate (SPEC.md §14.2), served on its own
// Unix socket: POST /send and nothing else. It is not a route of the
// panel's: no session, token or policy applies, the caller is the site the
// kernel says connected (mail_gate.go).
func (s *Server) MailRoutes() http.Handler {
	s.Init()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /send", s.handleGateSend)
	return mux
}

// serve is the per-route pipeline (SPEC.md §1.2, steps 3 to 10).
func (s *Server) serve(rt route) http.Handler {
	limit := rt.MaxBody
	if limit == 0 {
		limit = defaultMaxBody
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		withBodyDeadline(w, r, limit)
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		if status, msg := rt.crossSite(r); status != 0 {
			respondErr(w, status, msg)
			return
		}
		c := &call{s: s}
		if rt.Policy.needsSession() {
			user, tok, err := s.authenticate(r)
			// An assistant's call is made inside the panel, never over the
			// network: only the MCP server puts its caller in the context.
			if mc := mcpCallerOf(r); mc != nil {
				c.MCP = mc
				user, tok, err = s.mcpUser(mc)
			}
			if errors.Is(err, errBadToken) {
				respondErr(w, http.StatusUnauthorized, err.Error())
				return
			}
			if err != nil {
				respondErr(w, http.StatusUnauthorized, "authentication required")
				return
			}
			if rt.SessionOnly && (tok.ID != 0 || c.MCP != nil) {
				respondErr(w, http.StatusForbidden, msgSessionOnly)
				return
			}
			if !rt.Enrol && s.secondFactorPending(user) {
				respondErr(w, http.StatusForbidden, msgSecondFactorRequired)
				return
			}
			if ok, msg := rt.Policy.admits(user.Role, rt.AdminOnly); !ok {
				respondErr(w, http.StatusForbidden, msg)
				return
			}
			c.User, c.Token = user, tok
		}
		if rt.Policy.studio() {
			if status, msg := s.studioCall(w, r, rt.Policy, c); status != 0 {
				respondErr(w, status, msg)
				return
			}
		}
		if status, msg := s.load(rt.Load, r, c); status != 0 {
			respondErr(w, status, msg)
			return
		}
		if rt.Statements {
			release, ok := s.statements.acquire(c.statementKey(), c.Site.ID)
			if !ok {
				respondErr(w, http.StatusTooManyRequests, msgTooManyStatements)
				return
			}
			defer release()
		}
		rt.Handle(w, r, c)
	})
}

// crossSite applies the S-H4 rules (SPEC.md §1.5): the refusal's status and
// message, or 0.
func (rt route) crossSite(r *http.Request) (int, string) {
	if rt.Policy == policyToken || rt.Policy == policyMCP {
		return 0, ""
	}
	write := r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions
	if !write && !rt.SameOriginRead {
		return 0, ""
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return http.StatusForbidden, "cross-site request refused"
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if origin == "null" || err != nil || !strings.EqualFold(u.Host, r.Host) {
			return http.StatusForbidden, "cross-origin request refused"
		}
	}
	if !write || (r.Method == http.MethodDelete && r.ContentLength <= 0) {
		return 0, ""
	}
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt == "application/json" || (rt.Body == bodyUpload && mt == "application/octet-stream") {
		return 0, ""
	}
	return http.StatusUnsupportedMediaType, "requests must be sent as application/json"
}

// authenticate finds the account a request signs in as. A request carrying
// an API token (Authorization: Bearer cgt_...) is judged by it alone, never
// by a cookie it may also carry: a bad, expired or revoked token is
// errBadToken, and the token's owner must be an enabled admin
// (state.UserForAPIToken). Any other request is judged by its session
// cookie: a live session of an enabled account. tok is the token used, zero
// for a session.
func (s *Server) authenticate(r *http.Request) (user state.User, tok state.APIToken, err error) {
	if secret, has := bearerToken(r); has {
		user, tok, err = s.Store.UserForAPIToken(secret)
		if err != nil {
			return state.User{}, state.APIToken{}, errBadToken
		}
		return user, tok, nil
	}
	cookie, err := r.Cookie(s.sessionCookieName())
	if err != nil {
		return state.User{}, state.APIToken{}, err
	}
	user, err = s.sessionUser(r, cookie.Value)
	return user, state.APIToken{}, err
}

// backgroundHeader marks a request the UI sends on its own (a poll, a
// refetch after an event) rather than for something the person did.
const backgroundHeader = "X-CG-Background"

// sessionUser resolves a session token. A request marked as background
// (SPEC.md §2.2) is authenticated without counting as a use, so a tab left
// open still reaches the idle timeout; any other request records one.
func (s *Server) sessionUser(r *http.Request, token string) (state.User, error) {
	if r.Header.Get(backgroundHeader) == "1" {
		return s.Store.PeekSessionUser(token)
	}
	return s.Store.UserForSession(token)
}

// msgSecondFactorRequired refuses an account the 2FA requirement holds
// back (SPEC.md §1.2 step 7).
const msgSecondFactorRequired = "two-factor authentication is required for this account: set it up in Security before continuing"

// secondFactorPending reports whether the 2FA requirement holds this account
// back (S-C9, #156): the panel requires two-factor of its role (admins, or
// every account) and it has neither TOTP nor a passkey. An admin is root on
// the machine in all but name, so its password alone must not be the whole
// boundary. A setting that cannot be read counts as covering everyone.
func (s *Server) secondFactorPending(user state.User) bool {
	if user.HasSecondFactor() {
		return false
	}
	ps, err := s.Store.PanelSettings()
	if err != nil {
		s.logIfErr(err, "PanelSettings")
		return true
	}
	return ps.RequiresTwoFactor(user.Role)
}

// load runs the route's loader (SPEC.md §1.4): the refusal's status and
// message, or 0 with the resource in c.
func (s *Server) load(l loader, r *http.Request, c *call) (int, string) {
	if l == loadNone {
		return 0, ""
	}
	id, err := pathID(r)
	if err != nil {
		return http.StatusBadRequest, err.Error()
	}
	siteID := id
	switch l {
	case loadBackup:
		c.Backup, err = s.Store.GetBackup(id)
		siteID = c.Backup.SiteID
	case loadDeployment:
		c.Deployment, err = s.Store.GetDeployment(id)
		siteID = c.Deployment.SiteID
	case loadCron:
		c.Cron, err = s.Store.GetCronJob(id)
		siteID = c.Cron.SiteID
	}
	if err == nil {
		c.Site, err = s.Store.GetSite(siteID)
	}
	if err == nil && !c.CanAccess(c.Site.ID) {
		err = state.ErrNotFound
	}
	if errors.Is(err, state.ErrNotFound) {
		return http.StatusNotFound, l.noun() + " not found"
	}
	if err != nil {
		s.logIfErr(err, "load "+l.noun())
		return http.StatusInternalServerError, "could not read the " + l.noun()
	}
	return 0, ""
}

// decode reads the request's JSON body into into and refuses a field into
// does not have. The router has already bounded the body at the row's
// MaxBody, so a larger one fails here as malformed.
func decode[T any](r *http.Request, into *T) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(into)
}

// pathID parses the route's {id}.
func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("invalid id")
	}
	return id, nil
}

// withCommonHeaders sets the headers every response carries, matched or not
// (SPEC.md §1.2, step 1).
func (s *Server) withCommonHeaders(next http.Handler) http.Handler {
	csp := s.contentSecurityPolicy()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		// A page the panel opens in a new tab (wp-admin, the database
		// manager) gets no handle on the panel's window.
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
		h.Set("Content-Security-Policy", csp)
		next.ServeHTTP(w, r)
	})
}

// errorReply maps an error to the status and the message a browser may see
// (SPEC.md §1.7). def is the status for an error with no mapping of its own
// (502 when 0), fallback the message for one that carries none safe to show.
func errorReply(err error, def int, fallback string) (int, string) {
	if def == 0 {
		def = http.StatusBadGateway
	}
	var tooLarge *http.MaxBytesError
	switch {
	case agentrpc.AsError(err) != nil:
		return agentrpc.HTTPStatus(err, def), agentrpc.Message(err, fallback)
	case errors.Is(err, state.ErrNotFound):
		return http.StatusNotFound, fallback
	case errors.As(err, &tooLarge):
		return http.StatusRequestEntityTooLarge, "request body too large"
	}
	return def, fallback
}

// respondAgentErr answers a failed agent call through errorReply, with the
// message after prefix when one is given.
func respondAgentErr(w http.ResponseWriter, def int, prefix string, err error) {
	status, msg := errorReply(err, def, "The agent could not complete the operation")
	if prefix != "" {
		msg = prefix + ": " + msg
	}
	respondErr(w, status, msg)
}
