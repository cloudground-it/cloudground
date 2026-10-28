package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/accounts"
	"github.com/cloudground-it/cloudground/internal/state"
)

// mcpOAuth is a software MCP client for the tests: plain requests to the
// OAuth and /mcp rows, no cookies, redirects not followed.
type mcpOAuth struct {
	t    *testing.T
	ts   *httptest.Server
	hc   *http.Client
	base string
}

func newMCPOAuth(t *testing.T, ts *httptest.Server) *mcpOAuth {
	u, _ := url.Parse(ts.URL)
	return &mcpOAuth{t: t, ts: ts, base: "https://" + u.Host, hc: &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (m *mcpOAuth) do(method, path, contentType string, body io.Reader, hdr ...string) (*http.Response, []byte) {
	m.t.Helper()
	req, err := http.NewRequest(method, m.ts.URL+path, body)
	if err != nil {
		m.t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := m.hc.Do(req)
	if err != nil {
		m.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func (m *mcpOAuth) json(method, path string, v any, hdr ...string) (*http.Response, []byte) {
	b, _ := json.Marshal(v)
	return m.do(method, path, "application/json", strings.NewReader(string(b)), hdr...)
}

func (m *mcpOAuth) form(path string, v url.Values) (*http.Response, []byte) {
	return m.do("POST", path, "application/x-www-form-urlencoded", strings.NewReader(v.Encode()))
}

// rpc sends one JSON-RPC message to /mcp with the token.
func (m *mcpOAuth) rpc(token, method string, params any) (*http.Response, map[string]any) {
	m.t.Helper()
	resp, b := m.json("POST", "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params}, "Authorization", "Bearer "+token)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return resp, out
}

const mcpRedirect = "http://127.0.0.1:9/callback"

func pkcePair() (verifier, challenge string) {
	verifier = randomToken(48)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// register registers a client and returns its id.
func (m *mcpOAuth) register() string {
	m.t.Helper()
	resp, b := m.json("POST", "/oauth/register", map[string]any{"client_name": "Test assistant", "redirect_uris": []string{mcpRedirect}, "logo_uri": "https://x/logo.png"})
	var out struct {
		ClientID string `json:"client_id"`
	}
	if resp.StatusCode != http.StatusCreated || json.Unmarshal(b, &out) != nil || !strings.HasPrefix(out.ClientID, "cgc_") {
		m.t.Fatalf("register = %d %s", resp.StatusCode, b)
	}
	return out.ClientID
}

// authorize starts an authorisation and returns the consent request's id.
func (m *mcpOAuth) authorize(clientID, challenge string) string {
	m.t.Helper()
	q := url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {mcpRedirect},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "state": {"xyz"}, "resource": {m.base + "/mcp"}}
	resp, b := m.do("GET", "/oauth/authorize?"+q.Encode(), "", nil)
	loc := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusFound || !strings.HasPrefix(loc, mcpConsentPath+"?request=") {
		m.t.Fatalf("authorize = %d %s %s", resp.StatusCode, loc, b)
	}
	u, _ := url.Parse(loc)
	return u.Query().Get("request")
}

// mcpRecoveryCodes gives the account two-factor (a TOTP secret) and returns
// fresh recovery codes, one per proof.
func mcpRecoveryCodes(t *testing.T, s *Server, email string) []string {
	t.Helper()
	u, err := s.Store.GetUserByEmail(email)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Store.SetPendingTOTP(u.ID, "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.ActivatePendingTOTP(u.ID, "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"); err != nil {
		t.Fatal(err)
	}
	codes := make([]string, 10)
	hashes := make([]string, 10)
	for i := range codes {
		codes[i] = accounts.NewRecoveryCode()
		hashes[i] = accounts.RecoveryCodeHash(codes[i])
	}
	if err := s.Store.ReplaceRecoveryCodes(u.ID, hashes); err != nil {
		t.Fatal(err)
	}
	return codes
}

// tokensFor runs a whole authorisation for admin with write and returns the
// access and refresh tokens and the client.
func tokensFor(t *testing.T, m *mcpOAuth, admin *client, write map[string][]int64, recovery string) (access, refresh, clientID string) {
	t.Helper()
	clientID = m.register()
	verifier, challenge := pkcePair()
	id := m.authorize(clientID, challenge)
	resp, b := admin.do("POST", "/api/mcp/requests/"+id+"/approve", map[string]any{"write": write, "password": adminPassword, "recovery_code": recovery})
	var ap struct {
		Redirect string `json:"redirect"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(b, &ap) != nil {
		t.Fatalf("approve = %d %s", resp.StatusCode, b)
	}
	ru, _ := url.Parse(ap.Redirect)
	if ru.Query().Get("state") != "xyz" || ru.Query().Get("iss") != m.base {
		t.Fatalf("redirect %s", ap.Redirect)
	}
	resp, b = m.form("/oauth/token", url.Values{"grant_type": {"authorization_code"}, "code": {ru.Query().Get("code")},
		"client_id": {clientID}, "redirect_uri": {mcpRedirect}, "code_verifier": {verifier}, "resource": {m.base + "/mcp"}})
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(b, &tok) != nil || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("token = %d %s", resp.StatusCode, b)
	}
	return tok.AccessToken, tok.RefreshToken, clientID
}

func setMCP(t *testing.T, admin *client, on string) {
	t.Helper()
	if resp, b := admin.do("PUT", "/api/settings", map[string]string{"mcp": on}); resp.StatusCode != http.StatusOK {
		t.Fatalf("mcp=%q = %d %s", on, resp.StatusCode, b)
	}
}

func toolResult(t *testing.T, out map[string]any) (string, bool) {
	t.Helper()
	res, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", out)
	}
	content := res["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	isErr, _ := res["isError"].(bool)
	return text, isErr
}

func TestMCPIsOffByDefault(t *testing.T) {
	s, _, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	m := newMCPOAuth(t, ts)
	for _, rt := range [][2]string{{"GET", "/.well-known/oauth-authorization-server"}, {"GET", "/.well-known/oauth-protected-resource"},
		{"POST", "/oauth/register"}, {"GET", "/oauth/authorize"}, {"POST", "/oauth/token"}, {"POST", "/mcp"}, {"GET", "/mcp"}} {
		if resp, b := m.do(rt[0], rt[1], "application/json", strings.NewReader("{}")); resp.StatusCode != http.StatusNotFound || !strings.Contains(string(b), msgMCPOff) {
			t.Errorf("%s %s while off = %d %s", rt[0], rt[1], resp.StatusCode, b)
		}
	}
	if resp, _ := admin.do("GET", "/api/mcp/grants", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("grants while off = %d", resp.StatusCode)
	}
	if _, b := admin.do("GET", "/api/settings", nil); !strings.Contains(string(b), `"mcp":""`) {
		t.Errorf("settings: %s", b)
	}
}

func TestMCPRegistrationAndAuthorizationRefusals(t *testing.T) {
	s, _, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	setMCP(t, admin, "1")
	m := newMCPOAuth(t, ts)
	_, meta := m.do("GET", "/.well-known/oauth-authorization-server", "", nil)
	if !strings.Contains(string(meta), `"issuer":"`+m.base+`"`) || !strings.Contains(string(meta), `"S256"`) {
		t.Fatalf("metadata: %s", meta)
	}
	for _, bad := range []map[string]any{
		{"redirect_uris": []string{"http://evil.example/cb"}},
		{"redirect_uris": []string{"https://ok.example/cb#frag"}},
		{"redirect_uris": []string{}},
		{"redirect_uris": []string{"https://ok.example/cb"}, "token_endpoint_auth_method": "client_secret_basic"},
	} {
		if resp, b := m.json("POST", "/oauth/register", bad); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("register %v = %d %s", bad, resp.StatusCode, b)
		}
	}
	clientID := m.register()
	// An unknown redirect is never followed.
	if resp, _ := m.do("GET", "/oauth/authorize?client_id="+clientID+"&redirect_uri=https://evil.example/cb&response_type=code", "", nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("authorize to an unregistered redirect = %d", resp.StatusCode)
	}
	resp, _ := m.do("GET", "/oauth/authorize?"+url.Values{"client_id": {clientID}, "redirect_uri": {mcpRedirect}, "response_type": {"code"}, "state": {"s"}}.Encode(), "", nil)
	// Nor is a registered one, for a refusal: anyone may register a client.
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusBadRequest || loc != "" {
		t.Errorf("authorize without PKCE = %d %s", resp.StatusCode, loc)
	}
	_, challenge := pkcePair()
	id := m.authorize(clientID, challenge)
	if resp, b := admin.do("GET", "/api/mcp/requests/"+id, nil); resp.StatusCode != http.StatusOK || !strings.Contains(string(b), "Test assistant") {
		t.Fatalf("request = %d %s", resp.StatusCode, b)
	}
	// Without two-factor the panel grants nothing.
	if resp, b := admin.do("POST", "/api/mcp/requests/"+id+"/approve", map[string]any{"password": adminPassword}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("approve without two-factor = %d %s", resp.StatusCode, b)
	}
	codes := mcpRecoveryCodes(t, s, adminEmail)
	for _, tc := range []struct {
		body map[string]any
		want int
	}{
		{map[string]any{"password": adminPassword}, http.StatusForbidden},                                                                       // no factor
		{map[string]any{"password": adminPassword, "recovery_code": codes[0], "write": map[string][]int64{"root": {1}}}, http.StatusBadRequest}, // unknown area
		{map[string]any{"password": adminPassword, "recovery_code": codes[0], "write": map[string][]int64{"cache": {999}}}, http.StatusBadRequest},
	} {
		if resp, b := admin.do("POST", "/api/mcp/requests/"+id+"/approve", tc.body); resp.StatusCode != tc.want {
			t.Errorf("approve %v = %d %s, want %d", tc.body, resp.StatusCode, b, tc.want)
		}
	}
	// A wrong verifier spends the code.
	resp, b := admin.do("POST", "/api/mcp/requests/"+id+"/approve", map[string]any{"password": adminPassword, "recovery_code": codes[1]})
	var ap struct {
		Redirect string `json:"redirect"`
	}
	_ = json.Unmarshal(b, &ap)
	ru, _ := url.Parse(ap.Redirect)
	code := ru.Query().Get("code")
	if resp.StatusCode != http.StatusOK || code == "" {
		t.Fatalf("approve = %d %s", resp.StatusCode, b)
	}
	wrong, _ := pkcePair()
	tokenReq := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID}, "redirect_uri": {mcpRedirect}, "code_verifier": {wrong}}
	if resp, b := m.form("/oauth/token", tokenReq); resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(b), "invalid_grant") {
		t.Fatalf("a wrong verifier = %d %s", resp.StatusCode, b)
	}
	if resp, b := admin.do("GET", "/api/mcp/requests/"+id, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a spent request = %d %s", resp.StatusCode, b)
	}
}

func TestMCPToolsScopesAndPendingActions(t *testing.T) {
	s, _, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := createViaAPI(t, s, admin, staticSite("mcp.example.com"))
	other := createViaAPI(t, s, admin, staticSite("other.example.com"))
	setMCP(t, admin, "1")
	codes := mcpRecoveryCodes(t, s, adminEmail)
	m := newMCPOAuth(t, ts)

	resp, _ := m.json("POST", "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"})
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(resp.Header.Get("WWW-Authenticate"), m.base+"/.well-known/oauth-protected-resource") {
		t.Fatalf("/mcp without a token = %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}

	access, refresh, clientID := tokensFor(t, m, admin, map[string][]int64{"sites": {site.ID}, "cache": {site.ID}}, codes[0])
	if resp, out := m.rpc(access, "initialize", map[string]any{"protocolVersion": "2025-06-18"}); resp.StatusCode != 200 || out["result"].(map[string]any)["protocolVersion"] != "2025-06-18" {
		t.Fatalf("initialize = %d %v", resp.StatusCode, out)
	}
	_, list := m.rpc(access, "tools/list", nil)
	names := fmt.Sprint(list["result"])
	if !strings.Contains(names, "list_sites") || !strings.Contains(names, "delete_site") || strings.Contains(names, "create_backup") {
		t.Fatalf("tools/list: %s", names)
	}
	if text, isErr := toolResult(t, mustCall(m, access, "list_sites", nil)); isErr || !strings.Contains(text, "mcp.example.com") {
		t.Fatalf("list_sites: %v %s", isErr, text)
	}
	// A granted write with no body runs (its route needs the JSON type).
	if text, isErr := toolResult(t, mustCall(m, access, "purge_cache", map[string]any{"site_id": site.ID})); isErr {
		t.Fatalf("purge in the scope: %s", text)
	}
	// Writing outside the grant is refused before any route runs.
	if text, isErr := toolResult(t, mustCall(m, access, "purge_cache", map[string]any{"site_id": other.ID})); !isErr || !strings.Contains(text, "may not use cache") {
		t.Fatalf("purge outside the scope: %v %s", isErr, text)
	}
	if text, isErr := toolResult(t, mustCall(m, access, "create_backup", map[string]any{"site_id": site.ID})); !isErr || !strings.Contains(text, "may not use backups") {
		t.Fatalf("backup without the area: %v %s", isErr, text)
	}
	// A destructive tool does nothing until the person confirms it.
	text, isErr := toolResult(t, mustCall(m, access, "delete_site", map[string]any{"site_id": site.ID}))
	if isErr || !strings.Contains(text, `"status":"pending"`) {
		t.Fatalf("delete_site: %v %s", isErr, text)
	}
	if _, err := s.Store.GetSite(site.ID); err != nil {
		t.Fatal("the site went before anyone confirmed")
	}
	_, ab := admin.do("GET", "/api/mcp/actions", nil)
	var actions []struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
	}
	if json.Unmarshal(ab, &actions) != nil || len(actions) != 1 || actions[0].Status != "pending" {
		t.Fatalf("actions: %s", ab)
	}
	path := fmt.Sprintf("/api/mcp/actions/%d/confirm", actions[0].ID)
	if resp, b := admin.do("POST", path, map[string]any{"password": adminPassword}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("confirm without a factor = %d %s", resp.StatusCode, b)
	}
	if resp, b := admin.do("POST", path, map[string]any{"password": adminPassword, "recovery_code": codes[1]}); resp.StatusCode != http.StatusOK || !strings.Contains(string(b), `"status":"done"`) {
		t.Fatalf("confirm = %d %s", resp.StatusCode, b)
	}
	if resp, _ := admin.do("POST", path, map[string]any{"password": adminPassword, "recovery_code": codes[2]}); resp.StatusCode != http.StatusConflict {
		t.Fatalf("a second confirmation = %d", resp.StatusCode)
	}
	waitForTasks(t, s)
	if _, err := s.Store.GetSite(site.ID); err == nil {
		t.Fatal("the confirmed deletion did not run")
	}
	_, ad := admin.do("GET", "/api/audit", nil)
	if !strings.Contains(string(ad), `via MCP \"Test assistant\"`) || !strings.Contains(string(ad), "mcp.action.confirm") {
		t.Fatalf("audit: %.600s", ad)
	}

	// The tools never reach a session-only route.
	g, _ := s.Store.MCPGrantByAccess(tokenHash(access), time.Now())
	mc := &mcpCaller{grantID: g.ID, userID: g.UserID, client: g.ClientName, host: "x"}
	if st, b := s.mcpDo(context.Background(), mc, "GET", "/api/tokens", nil); st != http.StatusForbidden {
		t.Fatalf("an assistant on a session-only route = %d %s", st, b)
	}

	// Refresh rotation; the old refresh token again revokes the grant.
	resp, b := m.form("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {clientID}})
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(b, &tok) != nil {
		t.Fatalf("refresh = %d %s", resp.StatusCode, b)
	}
	if resp, _ := m.rpc(access, "ping", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatal("the rotated access token still works")
	}
	if resp, _ := m.rpc(tok.AccessToken, "ping", nil); resp.StatusCode != http.StatusOK {
		t.Fatal("the new access token does not work")
	}
	if resp, _ := m.form("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {clientID}}); resp.StatusCode != http.StatusBadRequest {
		t.Fatal("a reused refresh token was accepted")
	}
	if resp, _ := m.rpc(tok.AccessToken, "ping", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatal("the grant survived a reused refresh token")
	}

	// Turning the server off revokes everything; on again, the old token
	// stays refused.
	access2, _, _ := tokensFor(t, m, admin, nil, codes[3])
	setMCP(t, admin, "")
	if resp, _ := m.rpc(access2, "ping", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatal("/mcp answered while off")
	}
	setMCP(t, admin, "1")
	if resp, _ := m.rpc(access2, "ping", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatal("a token from before the switch went off still works")
	}

	// A password change ends the grants with the API tokens.
	access3, _, _ := tokensFor(t, m, admin, nil, codes[4])
	u, _ := s.Store.GetUserByEmail(adminEmail)
	if err := s.Store.SetUserPassword(u.ID, u.PasswordHash); err != nil {
		t.Fatal(err)
	}
	if resp, _ := m.rpc(access3, "ping", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatal("a grant outlived a password reset")
	}
	_ = state.ErrNotFound
}

func mustCall(m *mcpOAuth, token, tool string, args map[string]any) map[string]any {
	_, out := m.rpc(token, "tools/call", map[string]any{"name": tool, "arguments": args})
	return out
}
