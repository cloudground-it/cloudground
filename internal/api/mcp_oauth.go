package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudground-it/cloudground/internal/accounts"
	"github.com/cloudground-it/cloudground/internal/state"
	"github.com/cloudground-it/cloudground/internal/throttle"
)

// The remote MCP server's authorisation (SPEC.md §16.1–§16.5): OAuth 2.1
// with PKCE, dynamic client registration and resource indicators, the
// consent being the panel's own sign-in. Off by default.

const (
	msgMCPOff          = "the MCP server is off"
	mcpRequestTTL      = 10 * time.Minute
	mcpMaxRequests     = 100
	mcpCodeTTL         = 2 * time.Minute
	mcpAccessTTL       = time.Hour
	mcpRefreshTTL      = 30 * 24 * time.Hour
	mcpAccessPrefix    = "cgm_"
	mcpRefreshPrefix   = "cgr_"
	mcpClientPrefix    = "cgc_"
	mcpMaxRedirects    = 5
	mcpMaxClientName   = 100
	mcpDefaultClient   = "MCP client"
	mcpConsentPath     = "/assistants/connect"
	mcpActionsPagePath = "/assistants"
)

// mcpAreas are what writing is granted for, per site (SPEC.md §16.2).
var mcpAreas = []string{"cache", "backups", "deploys", "wp_updates", "sites"}

// mcpScopes is what a grant allows: reading always, writing per area on
// the listed sites.
type mcpScopes struct {
	Write map[string][]int64 `json:"write,omitempty"`
}

// String is the grant's scope as OAuth names it.
func (sc mcpScopes) String() string {
	parts := []string{"read"}
	for _, a := range mcpAreas {
		if len(sc.Write[a]) > 0 {
			parts = append(parts, "write:"+a)
		}
	}
	return strings.Join(parts, " ")
}

// can reports whether the grant may write area on site.
func (sc mcpScopes) can(area string, site int64) bool {
	return slices.Contains(sc.Write[area], site)
}

func mcpScopesSupported() []string {
	out := []string{"read"}
	for _, a := range mcpAreas {
		out = append(out, "write:"+a)
	}
	return out
}

// mcpRequest is an authorisation request waiting for its consent.
type mcpRequest struct {
	id, clientID, clientName, redirectURI, state, challenge, scope, base string
	expires                                                              time.Time
}

// mcpCode is an authorisation code waiting for its token request.
type mcpCode struct {
	clientID, redirectURI, challenge, resource string
	userID                                     int64
	scopes                                     mcpScopes
	expires                                    time.Time
}

// mcpState is what the MCP server keeps in memory: requests and codes live
// minutes, and a restart only costs a new connection.
type mcpState struct {
	mu       sync.Mutex
	requests map[string]*mcpRequest
	codes    map[string]*mcpCode // by the code's SHA-256
	register *throttle.Window
	token    *throttle.Window
	calls    *throttle.Window
}

func newMCPState() *mcpState {
	return &mcpState{
		requests: map[string]*mcpRequest{}, codes: map[string]*mcpCode{},
		register: throttle.New(10, time.Hour), token: throttle.New(30, time.Minute), calls: throttle.New(120, time.Minute),
	}
}

// forget drops every request and code: the MCP server was turned off.
func (m *mcpState) forget() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests, m.codes = map[string]*mcpRequest{}, map[string]*mcpCode{}
}

func (m *mcpState) putRequest(req *mcpRequest, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, r := range m.requests {
		if !now.Before(r.expires) {
			delete(m.requests, id)
		}
	}
	if len(m.requests) >= mcpMaxRequests {
		oldest := ""
		for id, r := range m.requests {
			if oldest == "" || r.expires.Before(m.requests[oldest].expires) {
				oldest = id
			}
		}
		delete(m.requests, oldest)
	}
	req.expires = now.Add(mcpRequestTTL)
	m.requests[req.id] = req
}

func (m *mcpState) request(id string, now time.Time) *mcpRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.requests[id]
	if r == nil || !now.Before(r.expires) {
		return nil
	}
	return r
}

func (m *mcpState) takeRequest(id string, now time.Time) *mcpRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.requests[id]
	delete(m.requests, id)
	if r == nil || !now.Before(r.expires) {
		return nil
	}
	return r
}

func (m *mcpState) putCode(hash string, c *mcpCode) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.codes[hash] = c
}

// takeCode spends a code, right or wrong.
func (m *mcpState) takeCode(hash string, now time.Time) *mcpCode {
	m.mu.Lock()
	defer m.mu.Unlock()
	for h, c := range m.codes {
		if !now.Before(c.expires) {
			delete(m.codes, h)
		}
	}
	c := m.codes[hash]
	delete(m.codes, hash)
	return c
}

// mcpOn reports whether the MCP server is on; a setting that cannot be read
// counts as off.
func (s *Server) mcpOn() bool {
	ps, err := s.Store.PanelSettings()
	if err != nil {
		s.logIfErr(err, "PanelSettings")
		return false
	}
	return ps.MCPEnabled
}

// mcpOff answers 404 when the MCP server is off, and reports whether it
// did.
func (s *Server) mcpOff(w http.ResponseWriter) bool {
	if s.mcpOn() {
		return false
	}
	respondErr(w, http.StatusNotFound, msgMCPOff)
	return true
}

// mcpBase is the base URL a request reached the panel at (SPEC.md §16.2).
func mcpBase(r *http.Request) string { return "https://" + r.Host }

func tokenHash(token string) string { return state.TokenHash(token) }

func oauthErr(w http.ResponseWriter, status int, code, desc string) {
	w.Header().Set("Cache-Control", "no-store")
	respond(w, status, map[string]string{"error": code, "error_description": desc})
}

func (s *Server) handleMCPResourceMetadata(w http.ResponseWriter, r *http.Request) {
	if s.mcpOff(w) {
		return
	}
	base := mcpBase(r)
	respond(w, http.StatusOK, map[string]any{
		"resource": base + "/mcp", "authorization_servers": []string{base},
		"scopes_supported": mcpScopesSupported(), "bearer_methods_supported": []string{"header"},
		"resource_name": "CloudGround",
	})
}

func (s *Server) handleMCPServerMetadata(w http.ResponseWriter, r *http.Request) {
	if s.mcpOff(w) {
		return
	}
	base := mcpBase(r)
	respond(w, http.StatusOK, map[string]any{
		"issuer":                                base,
		"authorization_endpoint":                base + "/oauth/authorize",
		"token_endpoint":                        base + "/oauth/token",
		"registration_endpoint":                 base + "/oauth/register",
		"revocation_endpoint":                   base + "/oauth/revoke",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"scopes_supported":                      mcpScopesSupported(),
	})
}

// validRedirect admits an absolute https URL without a fragment, or http on
// a loopback host (a desktop client's local listener).
func validRedirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" || u.Host == "" || u.User != nil {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		h := u.Hostname()
		if h == "localhost" {
			return true
		}
		ip := net.ParseIP(h)
		return ip != nil && ip.IsLoopback()
	}
	return false
}

func (s *Server) handleMCPRegister(w http.ResponseWriter, r *http.Request) {
	if s.mcpOff(w) {
		return
	}
	o := clientOrigin(r)
	if !s.mcp.register.Allow(o.Network) {
		oauthErr(w, http.StatusTooManyRequests, "slow_down", "too many registrations from this network: try again later")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		oauthErr(w, http.StatusBadRequest, "invalid_client_metadata", "the body could not be read")
		return
	}
	// Clients send metadata this panel has no use for (logo_uri,
	// software_id…): unknown fields are ignored, as RFC 7591 says.
	var in struct {
		ClientName              string   `json:"client_name"`
		RedirectURIs            []string `json:"redirect_uris"`
		TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
		GrantTypes              []string `json:"grant_types"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		oauthErr(w, http.StatusBadRequest, "invalid_client_metadata", "malformed client metadata")
		return
	}
	if in.TokenEndpointAuthMethod != "" && in.TokenEndpointAuthMethod != "none" {
		oauthErr(w, http.StatusBadRequest, "invalid_client_metadata", "only public clients (token_endpoint_auth_method none, with PKCE) are accepted")
		return
	}
	for _, g := range in.GrantTypes {
		if g != "authorization_code" && g != "refresh_token" {
			oauthErr(w, http.StatusBadRequest, "invalid_client_metadata", "grant_types may be authorization_code and refresh_token only")
			return
		}
	}
	name := strings.TrimSpace(in.ClientName)
	if len(name) > mcpMaxClientName || strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || (r >= 0x7f && r < 0xa0) }) {
		oauthErr(w, http.StatusBadRequest, "invalid_client_metadata", "client_name: at most 100 characters, no control characters")
		return
	}
	if name == "" {
		name = mcpDefaultClient
	}
	if len(in.RedirectURIs) == 0 || len(in.RedirectURIs) > mcpMaxRedirects {
		oauthErr(w, http.StatusBadRequest, "invalid_redirect_uri", "give 1 to 5 redirect_uris")
		return
	}
	for _, u := range in.RedirectURIs {
		if !validRedirect(u) {
			oauthErr(w, http.StatusBadRequest, "invalid_redirect_uri", "a redirect_uri must be https, or http on localhost: "+u)
			return
		}
	}
	grants := in.GrantTypes
	if len(grants) == 0 {
		grants = []string{"authorization_code", "refresh_token"}
	}
	c, err := s.Store.AddMCPClient(state.MCPClient{ID: mcpClientPrefix + randomToken(16), Name: name, RedirectURIs: in.RedirectURIs})
	if errors.Is(err, state.ErrTooManyMCPClients) {
		oauthErr(w, http.StatusTooManyRequests, "slow_down", err.Error())
		return
	}
	if err != nil {
		internalError(w, "AddMCPClient", err, "")
		return
	}
	s.audit("MCP client registration", "mcp.client.register", c.Name, o.Address)
	respond(w, http.StatusCreated, map[string]any{
		"client_id": c.ID, "client_id_issued_at": c.CreatedAt.Unix(), "client_name": c.Name,
		"redirect_uris": c.RedirectURIs, "grant_types": grants, "response_types": []string{"code"},
		"token_endpoint_auth_method": "none",
	})
}

// redirectWith is a redirect_uri with query parameters added.
func redirectWith(redirectURI string, params url.Values) string {
	u, err := url.Parse(redirectURI)
	if err != nil {
		return redirectURI
	}
	q := u.Query()
	for k, vs := range params {
		for _, v := range vs {
			if v != "" {
				q.Set(k, v)
			}
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func (s *Server) handleMCPAuthorize(w http.ResponseWriter, r *http.Request) {
	if s.mcpOff(w) {
		return
	}
	q := r.URL.Query()
	base := mcpBase(r)
	client, err := s.Store.GetMCPClient(q.Get("client_id"))
	redirectURI := q.Get("redirect_uri")
	if err != nil || !slices.Contains(client.RedirectURIs, redirectURI) {
		// Never redirect to an address the client did not register.
		oauthErr(w, http.StatusBadRequest, "invalid_request", "unknown client_id, or a redirect_uri it did not register")
		return
	}
	fail := func(code, desc string) {
		http.Redirect(w, r, redirectWith(redirectURI, url.Values{
			"error": {code}, "error_description": {desc}, "state": {q.Get("state")}, "iss": {base},
		}), http.StatusFound)
	}
	switch {
	case q.Get("response_type") != "code":
		fail("unsupported_response_type", "response_type must be code")
		return
	case q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256":
		fail("invalid_request", "PKCE is required: code_challenge with code_challenge_method S256")
		return
	case q.Get("resource") != "" && strings.TrimSuffix(q.Get("resource"), "/") != base+"/mcp":
		fail("invalid_target", "resource must be "+base+"/mcp")
		return
	}
	req := &mcpRequest{
		id: randomToken(24), clientID: client.ID, clientName: client.Name, redirectURI: redirectURI,
		state: q.Get("state"), challenge: q.Get("code_challenge"), scope: q.Get("scope"), base: base,
	}
	s.mcp.putRequest(req, time.Now())
	http.Redirect(w, r, mcpConsentPath+"?request="+url.QueryEscape(req.id), http.StatusFound)
}

// mcpSites are the sites the caller may grant writing on: an admin's all,
// an operator's own, a read-only account's none.
func (s *Server) mcpSites(c *call) ([]map[string]any, error) {
	if c.ReadOnly() {
		return []map[string]any{}, nil
	}
	sites, err := s.Store.ListSites()
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, st := range sites {
		if c.CanAccess(st.ID) {
			out = append(out, map[string]any{"id": st.ID, "domain": st.Domain})
		}
	}
	return out, nil
}

const msgMCPRequestGone = "this connection request has expired: start again from the assistant"

func (s *Server) handleMCPRequest(w http.ResponseWriter, r *http.Request, c *call) {
	if s.mcpOff(w) {
		return
	}
	req := s.mcp.request(r.PathValue("id"), time.Now())
	if req == nil {
		respondErr(w, http.StatusNotFound, msgMCPRequestGone)
		return
	}
	sites, err := s.mcpSites(c)
	if err != nil {
		internalError(w, "ListSites", err, "")
		return
	}
	host := req.redirectURI
	if u, err := url.Parse(req.redirectURI); err == nil {
		host = u.Host
	}
	respond(w, http.StatusOK, map[string]any{
		"client_name": req.clientName, "redirect_host": host, "requested_scope": req.scope,
		"areas": mcpAreas, "sites": sites, "two_factor": c.User.HasSecondFactor(),
	})
}

func (s *Server) handleMCPApprove(w http.ResponseWriter, r *http.Request, c *call) {
	if s.mcpOff(w) {
		return
	}
	var in struct {
		Write map[string][]int64 `json:"write"`
		accounts.Proof
	}
	if !decodeOr400(w, r, &in) {
		return
	}
	id := r.PathValue("id")
	now := time.Now()
	if s.mcp.request(id, now) == nil {
		respondErr(w, http.StatusNotFound, msgMCPRequestGone)
		return
	}
	if !c.User.HasSecondFactor() {
		respondErr(w, http.StatusForbidden, "turn on two-step verification before connecting an assistant")
		return
	}
	scopes := mcpScopes{Write: map[string][]int64{}}
	for area, sites := range in.Write {
		if !slices.Contains(mcpAreas, area) {
			respondErr(w, http.StatusBadRequest, "unknown area: use cache, backups, deploys, wp_updates or sites")
			return
		}
		for _, site := range sites {
			if c.ReadOnly() || site <= 0 || !c.CanAccess(site) {
				respondErr(w, http.StatusBadRequest, "invalid site")
				return
			}
			if _, err := s.Store.GetSite(site); err != nil {
				respondErr(w, http.StatusBadRequest, "invalid site")
				return
			}
			if !slices.Contains(scopes.Write[area], site) {
				scopes.Write[area] = append(scopes.Write[area], site)
			}
		}
		sort.Slice(scopes.Write[area], func(i, j int) bool { return scopes.Write[area][i] < scopes.Write[area][j] })
		if len(scopes.Write[area]) == 0 {
			delete(scopes.Write, area)
		}
	}
	if err := s.accounts.Confirm(r.Context(), clientOrigin(r), c.User, s.relyingParty(r), c.Actor(), "connect an assistant", in.Proof); err != nil {
		respondAccountErr(w, err)
		return
	}
	req := s.mcp.takeRequest(id, now)
	if req == nil {
		respondErr(w, http.StatusNotFound, msgMCPRequestGone)
		return
	}
	code := randomToken(32)
	s.mcp.putCode(tokenHash(code), &mcpCode{
		clientID: req.clientID, redirectURI: req.redirectURI, challenge: req.challenge, resource: req.base + "/mcp",
		userID: c.User.ID, scopes: scopes, expires: now.Add(mcpCodeTTL),
	})
	s.audit(c.Actor(), "mcp.grant", req.clientName, scopes.String())
	respond(w, http.StatusOK, map[string]string{"redirect": redirectWith(req.redirectURI, url.Values{
		"code": {code}, "state": {req.state}, "iss": {req.base},
	})})
}

func (s *Server) handleMCPDeny(w http.ResponseWriter, r *http.Request, c *call) {
	if s.mcpOff(w) {
		return
	}
	req := s.mcp.takeRequest(r.PathValue("id"), time.Now())
	if req == nil {
		respondErr(w, http.StatusNotFound, msgMCPRequestGone)
		return
	}
	respond(w, http.StatusOK, map[string]string{"redirect": redirectWith(req.redirectURI, url.Values{
		"error": {"access_denied"}, "state": {req.state}, "iss": {req.base},
	})})
}

// pkceOK checks a code verifier against an S256 challenge (RFC 7636).
func pkceOK(verifier, challenge string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	for _, r := range verifier {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || strings.ContainsRune("-._~", r)) {
			return false
		}
	}
	sum := sha256.Sum256([]byte(verifier))
	return subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(challenge)) == 1
}

// newMCPTokens mints an access and a refresh token, and their hashes.
func newMCPTokens(now time.Time) (access, refresh string, t state.MCPTokens) {
	access, refresh = mcpAccessPrefix+randomToken(32), mcpRefreshPrefix+randomToken(32)
	return access, refresh, state.MCPTokens{
		AccessHash: tokenHash(access), RefreshHash: tokenHash(refresh),
		AccessExpires: now.Add(mcpAccessTTL), RefreshExpires: now.Add(mcpRefreshTTL),
	}
}

func (s *Server) handleMCPToken(w http.ResponseWriter, r *http.Request) {
	if s.mcpOff(w) {
		return
	}
	if !s.mcp.token.Allow(clientOrigin(r).Network) {
		oauthErr(w, http.StatusTooManyRequests, "slow_down", "too many token requests from this network: try again in a minute")
		return
	}
	if err := r.ParseForm(); err != nil {
		oauthErr(w, http.StatusBadRequest, "invalid_request", "the body must be form-encoded")
		return
	}
	f := r.PostForm
	now := time.Now()
	issue := func(scope string, access, refresh string) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		respond(w, http.StatusOK, map[string]any{
			"access_token": access, "token_type": "Bearer", "expires_in": int(mcpAccessTTL / time.Second),
			"refresh_token": refresh, "scope": scope,
		})
	}
	switch f.Get("grant_type") {
	case "authorization_code":
		if f.Get("code") == "" || f.Get("client_id") == "" || f.Get("code_verifier") == "" {
			oauthErr(w, http.StatusBadRequest, "invalid_request", "code, client_id, redirect_uri and code_verifier are required")
			return
		}
		code := s.mcp.takeCode(tokenHash(f.Get("code")), now)
		if code == nil || code.clientID != f.Get("client_id") || code.redirectURI != f.Get("redirect_uri") ||
			!pkceOK(f.Get("code_verifier"), code.challenge) ||
			(f.Get("resource") != "" && strings.TrimSuffix(f.Get("resource"), "/") != code.resource) {
			oauthErr(w, http.StatusBadRequest, "invalid_grant", "the code is unknown, expired, used, or does not match")
			return
		}
		scopes, err := json.Marshal(code.scopes)
		if err != nil {
			internalError(w, "scopes", err, "")
			return
		}
		access, refresh, t := newMCPTokens(now)
		if _, err := s.Store.CreateMCPGrant(code.userID, code.clientID, string(scopes), t); err != nil {
			internalError(w, "CreateMCPGrant", err, "")
			return
		}
		issue(code.scopes.String(), access, refresh)
	case "refresh_token":
		if f.Get("refresh_token") == "" || f.Get("client_id") == "" {
			oauthErr(w, http.StatusBadRequest, "invalid_request", "refresh_token and client_id are required")
			return
		}
		access, refresh, t := newMCPTokens(now)
		g, err := s.Store.RotateMCPGrant(tokenHash(f.Get("refresh_token")), f.Get("client_id"), now, t)
		switch {
		case errors.Is(err, state.ErrMCPRefreshReused):
			s.audit(g.Email, "mcp.grant.revoked", g.ClientName, "refresh token reused")
			oauthErr(w, http.StatusBadRequest, "invalid_grant", "the refresh token was already used: the grant is revoked")
			return
		case errors.Is(err, state.ErrNotFound):
			oauthErr(w, http.StatusBadRequest, "invalid_grant", "the refresh token is unknown or expired")
			return
		case err != nil:
			internalError(w, "RotateMCPGrant", err, "")
			return
		}
		var sc mcpScopes
		_ = json.Unmarshal([]byte(g.Scopes), &sc)
		issue(sc.String(), access, refresh)
	default:
		oauthErr(w, http.StatusBadRequest, "unsupported_grant_type", "grant_type must be authorization_code or refresh_token")
	}
}

func (s *Server) handleMCPRevoke(w http.ResponseWriter, r *http.Request) {
	if s.mcpOff(w) {
		return
	}
	if err := r.ParseForm(); err == nil && r.PostForm.Get("token") != "" {
		g, found, err := s.Store.RevokeMCPGrantByToken(tokenHash(r.PostForm.Get("token")))
		s.logIfErr(err, "RevokeMCPGrantByToken")
		if found {
			s.audit(g.Email, "mcp.grant.revoked", g.ClientName, "revoked by the client")
		}
	}
	w.WriteHeader(http.StatusOK)
}

// mcpGrantView is a grant as the panel lists it.
type mcpGrantView struct {
	ID         int64      `json:"id"`
	ClientName string     `json:"client_name"`
	Email      string     `json:"email"`
	Scope      string     `json:"scope"`
	Write      any        `json:"write"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  time.Time  `json:"expires_at"`
}

func (s *Server) handleMCPGrants(w http.ResponseWriter, r *http.Request, c *call) {
	if s.mcpOff(w) {
		return
	}
	owner := c.User.ID
	if c.User.Role == roleAdmin {
		owner = 0
	}
	list, err := s.Store.ListMCPGrants(owner)
	if err != nil {
		internalError(w, "ListMCPGrants", err, "")
		return
	}
	out := make([]mcpGrantView, 0, len(list))
	for _, g := range list {
		var sc mcpScopes
		_ = json.Unmarshal([]byte(g.Scopes), &sc)
		v := mcpGrantView{ID: g.ID, ClientName: g.ClientName, Email: g.Email, Scope: sc.String(), Write: sc.Write, CreatedAt: g.CreatedAt, ExpiresAt: g.RefreshExpires}
		if !g.LastUsedAt.IsZero() {
			used := g.LastUsedAt
			v.LastUsedAt = &used
		}
		out = append(out, v)
	}
	respond(w, http.StatusOK, out)
}

func (s *Server) handleMCPRevokeGrant(w http.ResponseWriter, r *http.Request, c *call) {
	if s.mcpOff(w) {
		return
	}
	id, err := pathID(r)
	if err != nil {
		respondErr(w, http.StatusBadRequest, err.Error())
		return
	}
	g, err := s.Store.GetMCPGrant(id)
	if err != nil || (g.UserID != c.User.ID && c.User.Role != roleAdmin) {
		respondErr(w, http.StatusNotFound, "grant not found")
		return
	}
	if _, err := s.Store.DeleteMCPGrant(id); err != nil {
		internalError(w, "DeleteMCPGrant", err, "")
		return
	}
	s.audit(c.Actor(), "mcp.grant.revoked", g.ClientName, g.Email)
	respond(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// mcpSwitched follows a change of the setting mcp (SPEC.md §16.1): off
// revokes everything the MCP server issued.
func (s *Server) mcpSwitched(actor string, was, now bool) error {
	switch {
	case was && !now:
		if err := s.Store.ClearMCP(); err != nil {
			return err
		}
		s.mcp.forget()
		s.audit(actor, "mcp.disabled", "panel", "every client, grant and pending action revoked")
	case !was && now:
		s.audit(actor, "mcp.enabled", "panel", "")
	}
	return nil
}
