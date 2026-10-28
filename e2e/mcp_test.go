//go:build e2e

package e2e

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/accounts"
)

// mcpClient is a software MCP client reaching the panel as a remote one
// does: through nginx on 8443, at the box's sslip.io name, with no cookie
// (the dial goes to the box's address, so no DNS is needed; the certificate is the
// panel's self-signed one for its IP).
type mcpClient struct {
	h    *harness
	hc   *http.Client
	base string
}

func newMCPClient(h *harness) *mcpClient {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, net.JoinHostPort(h.IP, "8443"))
		},
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // the panel's self-signed IP certificate
		DisableKeepAlives: true,
	}
	return &mcpClient{h: h, base: "https://" + h.IP + ".sslip.io:8443", hc: &http.Client{
		Transport: tr, Timeout: time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (m *mcpClient) do(method, path, contentType, body string, hdr ...string) (*http.Response, []byte) {
	m.h.step(method + " " + path + " (MCP client)")
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	target := path
	if !strings.HasPrefix(path, "https://") {
		target = m.base + path
	}
	req, err := http.NewRequest(method, target, rd)
	if err != nil {
		return &http.Response{}, nil
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := m.hc.Do(req)
	if err != nil {
		m.h.root.Logf("MCP client %s %s: %v", method, path, err)
		return &http.Response{Header: http.Header{}}, nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func (m *mcpClient) rpc(token, method string, params any) (int, map[string]any) {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	resp, out := m.do("POST", "/mcp", "application/json", string(b), "Authorization", "Bearer "+token)
	var v map[string]any
	_ = json.Unmarshal(out, &v)
	return resp.StatusCode, v
}

// toolText is a tools/call result's text and whether it is an error.
func toolText(v map[string]any) (string, bool) {
	res, _ := v["result"].(map[string]any)
	content, _ := res["content"].([]any)
	if len(content) == 0 {
		return fmt.Sprint(v), true
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	isErr, _ := res["isError"].(bool)
	return text, isErr
}

const mcpE2ERedirect = "http://127.0.0.1:9/callback"

// mcpConnect runs a client's whole connection: discovery from /mcp's 401,
// registration, authorisation, the consent by owner (with a recovery code)
// and the token exchange. It returns the access and refresh tokens and
// the client id; "" when a step failed (and was reported).
func (h *harness) mcpConnect(t *testing.T, m *mcpClient, owner *apiClient, pw, recovery string, write map[string][]int64) (access, refresh, clientID string) {
	resp, _ := m.do("POST", "/mcp", "application/json", `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	www := resp.Header.Get("WWW-Authenticate")
	h.assert(t, "#70: /mcp without a token is 401 and names the resource metadata",
		resp.StatusCode == 401 && strings.Contains(www, m.base+"/.well-known/oauth-protected-resource"), "HTTP %d %q", resp.StatusCode, www)
	_, rm := m.do("GET", "/.well-known/oauth-protected-resource", "", "")
	var prm struct {
		AuthorizationServers []string `json:"authorization_servers"`
		Resource             string   `json:"resource"`
	}
	_ = json.Unmarshal(rm, &prm)
	h.check(t, "#70: the resource metadata names the panel as its authorisation server", strings.Join(prm.AuthorizationServers, ","), m.base)
	_, am := m.do("GET", "/.well-known/oauth-authorization-server", "", "")
	var asm struct {
		Registration string `json:"registration_endpoint"`
		Token        string `json:"token_endpoint"`
	}
	_ = json.Unmarshal(am, &asm)
	if asm.Registration == "" {
		h.fail(t, "#70: the authorisation server's metadata answers", "%s", head(am, 300))
		return
	}
	resp, rb := m.do("POST", asm.Registration, "application/json", `{"client_name":"e2e assistant","redirect_uris":["`+mcpE2ERedirect+`"]}`)
	clientID = jsonField(rb, "client_id")
	h.assert(t, "#70: a client registers itself", resp.StatusCode == 201 && strings.HasPrefix(clientID, "cgc_"), "HTTP %d %s", resp.StatusCode, head(rb, 300))
	if clientID == "" {
		return
	}
	verifier := strings.Repeat("v", 20) + fmt.Sprint(time.Now().UnixNano()) + strings.Repeat("x", 20)
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {mcpE2ERedirect}, "state": {"e2e"},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}, "resource": {prm.Resource}}
	resp, _ = m.do("GET", "/oauth/authorize?"+q.Encode(), "", "")
	loc, _ := url.Parse(resp.Header.Get("Location"))
	reqID := ""
	if loc != nil {
		reqID = loc.Query().Get("request")
	}
	h.assert(t, "#70: authorisation sends the person to the panel's consent page", resp.StatusCode == 302 && loc != nil && loc.Path == "/assistants/connect" && reqID != "",
		"HTTP %d %q", resp.StatusCode, resp.Header.Get("Location"))
	if reqID == "" {
		return
	}
	st, ab := owner.post("/api/mcp/requests/"+reqID+"/approve", map[string]any{"write": write, "password": pw, "recovery_code": recovery})
	redirect := jsonField(ab, "redirect")
	ru, _ := url.Parse(redirect)
	if st != 200 || ru == nil || ru.Query().Get("code") == "" {
		h.fail(t, "#70: the consent with a second factor gives a code", "HTTP %d %s", st, head(ab, 300))
		return
	}
	h.check(t, "#70: the code comes back with the client's state and the issuer", ru.Query().Get("state")+" "+ru.Query().Get("iss"), "e2e "+m.base)
	form := url.Values{"grant_type": {"authorization_code"}, "code": {ru.Query().Get("code")}, "client_id": {clientID},
		"redirect_uri": {mcpE2ERedirect}, "code_verifier": {verifier}, "resource": {prm.Resource}}
	resp, tb := m.do("POST", asm.Token, "application/x-www-form-urlencoded", form.Encode())
	access, refresh = jsonField(tb, "access_token"), jsonField(tb, "refresh_token")
	h.assert(t, "#70: the code and its PKCE verifier give tokens", resp.StatusCode == 200 && strings.HasPrefix(access, "cgm_") && refresh != "",
		"HTTP %d %s", resp.StatusCode, head(tb, 300))
	return access, refresh, clientID
}

// mcpServer (#70): the remote MCP server, off by default, reached as a
// remote client does (nginx on 8443 at the box's sslip.io name). Off, its
// addresses are 404; on, a client discovers, registers and is authorised
// with the panel's consent (an account with two-factor, a recovery code as
// the proof), lists sites, is refused a write outside its grant, and its
// delete_site waits, the site untouched, until the person confirms it in
// the panel; a reused refresh token revokes the grant, a grant revoked in
// the panel stops working, and turning the server off revokes the rest.
func (h *harness) mcpServer(t *testing.T) {
	_, sb := h.api.get("/api/settings")
	if jsonField(sb, "mcp") == "1" {
		t.Logf("(the MCP server is already on: the section needs it off to begin with)")
		return
	}
	m := newMCPClient(h)
	for _, path := range []string{"/.well-known/oauth-authorization-server", "/mcp"} {
		method := "GET"
		if path == "/mcp" {
			method = "POST"
		}
		resp, b := m.do(method, path, "application/json", "{}")
		h.assert(t, "#70: off by default, "+path+" is 404", resp.StatusCode == 404 && contains(b, "MCP server is off"), "HTTP %d %s", resp.StatusCode, head(b, 200))
	}
	resp, _ := m.do("POST", "/oauth/register", "application/json", `{"redirect_uris":["https://x.example/cb"]}`)
	h.check(t, "#70: off, no client can register", fmt.Sprint(resp.StatusCode), "404")

	setMCP := func(v string) int {
		st, _ := h.api.put("/api/settings", map[string]string{"mcp": v})
		return st
	}
	if setMCP("1") != 200 {
		h.fail(t, "control: the MCP server is turned on", "")
		return
	}
	defer h.undo("turn the MCP server off", func() { setMCP("") })()

	// The section's own admin, with two-factor: its recovery codes are the
	// proofs of the consents and of the confirmation.
	email, pw := fmt.Sprintf("e2e-mcp-%d@example.com", h.pid), fmt.Sprintf("e2e-mcp-password-%d", h.pid)
	_, ub := h.api.post("/api/users", map[string]any{"email": email, "password": pw, "role": "admin"})
	uid := jsonField(ub, "id")
	if uid == "" {
		h.fail(t, "control: the MCP account is created", "%s", head(ub, 200))
		return
	}
	defer h.undo("delete the MCP account", func() { h.api.relogin(); h.api.del("/api/users/" + uid) })()
	owner := newAPIClient(h, email, pw)
	if !owner.login() {
		h.fail(t, "control: the MCP account signs in", "")
		return
	}
	_, eb := owner.post("/api/account/2fa/begin", map[string]string{"password": pw})
	secret := jsonField(eb, "secret")
	c, _ := accounts.TOTPCode(secret, uint64(time.Now().Unix()/30))
	_, cb := owner.post("/api/account/2fa/confirm", map[string]string{"code": c})
	var conf struct {
		RecoveryCodes []string `json:"recovery_codes"`
	}
	_ = json.Unmarshal(cb, &conf)
	if len(conf.RecoveryCodes) < 5 {
		h.fail(t, "control: the MCP account turns on two-factor and gets recovery codes", "%s", head(cb, 300))
		return
	}
	codes := conf.RecoveryCodes

	// A site of the section's own, for the destructive tool.
	siteID := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"static"}`, h.dom("mcp")))
	if h.api.waitActive(siteID, 4*time.Minute) != "active" {
		h.fail(t, "control: the MCP section's site provisions", "site %d", siteID)
		return
	}

	access, refresh, clientID := h.mcpConnect(t, m, owner, pw, codes[0], map[string][]int64{"sites": {siteID}, "cache": {siteID}})
	if access == "" {
		return
	}
	// A refusal stays on the panel, even for a client's registered address:
	// anyone may register one, so following it would be an open redirect.
	resp, _ = m.do("GET", "/oauth/authorize?"+url.Values{"response_type": {"token"}, "client_id": {clientID}, "redirect_uri": {mcpE2ERedirect}}.Encode(), "", "")
	h.assert(t, "#36: a refused authorisation is answered, not redirected", resp.StatusCode == 400 && resp.Header.Get("Location") == "",
		"HTTP %d %q", resp.StatusCode, resp.Header.Get("Location"))
	st, init := m.rpc(access, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "e2e"}})
	h.assert(t, "#70: initialize answers with the protocol version", st == 200 && strings.Contains(fmt.Sprint(init["result"]), "protocolVersion:2025-06-18"), "HTTP %d %v", st, init)
	st, out := m.rpc(access, "tools/call", map[string]any{"name": "list_sites", "arguments": map[string]any{}})
	text, isErr := toolText(out)
	h.assert(t, "#70: list_sites lists the site", st == 200 && !isErr && strings.Contains(text, h.dom("mcp")), "HTTP %d %s", st, head([]byte(text), 300))
	_, out = m.rpc(access, "tools/call", map[string]any{"name": "create_backup", "arguments": map[string]any{"site_id": siteID}})
	text, isErr = toolText(out)
	h.assert(t, "#70: a write outside the grant is refused", isErr && strings.Contains(text, "may not use backups"), "%s", text)
	_, out = m.rpc(access, "tools/call", map[string]any{"name": "purge_cache", "arguments": map[string]any{"site_id": siteID}})
	text, isErr = toolText(out)
	h.assert(t, "#36: a granted write with no body runs", !isErr, "%s", text)

	_, out = m.rpc(access, "tools/call", map[string]any{"name": "delete_site", "arguments": map[string]any{"site_id": siteID}})
	text, isErr = toolText(out)
	h.assert(t, "#70: delete_site only records a pending action", !isErr && strings.Contains(text, `"status":"pending"`), "%s", text)
	time.Sleep(3 * time.Second)
	h.check(t, "#70: the site is untouched until the person confirms", code(h.api.get(fmt.Sprintf("/api/sites/%d", siteID))), "200")
	_, al := owner.get("/api/mcp/actions")
	var actions []struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
	}
	_ = json.Unmarshal(al, &actions)
	if len(actions) == 0 || actions[0].Status != "pending" {
		h.fail(t, "#70: the action waits in the panel", "%s", head(al, 300))
	} else {
		path := fmt.Sprintf("/api/mcp/actions/%d/confirm", actions[0].ID)
		h.check(t, "#70: confirming without a second factor is refused", code(owner.post(path, map[string]any{"password": pw})), "403")
		st, cb := owner.post(path, map[string]any{"password": pw, "recovery_code": codes[1]})
		h.assert(t, "#70: the person confirms it with a second factor", st == 200 && contains(cb, `"status":"done"`), "HTTP %d %s", st, head(cb, 300))
		gone := false
		for deadline := time.Now().Add(4 * time.Minute); time.Now().Before(deadline); time.Sleep(pollEvery) {
			if code(h.api.get(fmt.Sprintf("/api/sites/%d", siteID))) == "404" {
				gone = true
				break
			}
		}
		h.assert(t, "#70: once confirmed, the site is deleted", gone, "site %d still there", siteID)
		_, audit := h.api.get("/api/audit")
		h.assert(t, "#70: the deletion is audited as the assistant's", contains(audit, `via MCP \"e2e assistant\"`) && contains(audit, "mcp.action.confirm"), "audit has no MCP actor")
	}

	// A refresh token used twice revokes the grant.
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {clientID}}
	resp, rb := m.do("POST", "/oauth/token", "application/x-www-form-urlencoded", form.Encode())
	access2 := jsonField(rb, "access_token")
	h.assert(t, "#70: a refresh token gives new tokens", resp.StatusCode == 200 && access2 != "", "HTTP %d %s", resp.StatusCode, head(rb, 200))
	resp, _ = m.do("POST", "/oauth/token", "application/x-www-form-urlencoded", form.Encode())
	h.check(t, "#70: a refresh token used twice is refused", fmt.Sprint(resp.StatusCode), "400")
	st, _ = m.rpc(access2, "ping", nil)
	h.check(t, "#70: and its grant is revoked", fmt.Sprint(st), "401")

	// A grant revoked in the panel stops working.
	access3, _, _ := h.mcpConnect(t, m, owner, pw, codes[2], nil)
	_, gl := owner.get("/api/mcp/grants")
	var grants []struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(gl, &grants)
	if access3 != "" && len(grants) > 0 {
		h.check(t, "#70: control: the new grant works", fmt.Sprint(firstStatus(m.rpc(access3, "ping", nil))), "200")
		h.check(t, "#70: a grant is revoked from the panel", code(owner.del(fmt.Sprintf("/api/mcp/grants/%d", grants[0].ID))), "200")
		h.check(t, "#70: a revoked grant's token is refused", fmt.Sprint(firstStatus(m.rpc(access3, "ping", nil))), "401")
	}

	// Off again: 404; on again, a token from before is refused.
	access4, _, _ := h.mcpConnect(t, m, owner, pw, codes[3], nil)
	if setMCP("") != 200 {
		h.fail(t, "control: the MCP server is turned off", "")
		return
	}
	h.check(t, "#70: off again, /mcp is 404", fmt.Sprint(firstStatus(m.rpc(access4, "ping", nil))), "404")
	if setMCP("1") == 200 {
		h.check(t, "#70: turning it off revoked the token", fmt.Sprint(firstStatus(m.rpc(access4, "ping", nil))), "401")
	}
	setMCP("")
}

func firstStatus(st int, _ map[string]any) int { return st }
