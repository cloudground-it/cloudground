//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

const agentSock = "/run/cloudground/agent.sock"

// agentPing calls the agent's Ping over its socket with the real token, as
// the given user (root when user is ""), optionally with a primary group.
// The token goes to curl on stdin, never in argv.
func (h *harness) agentPing(user, group string) result {
	tok, err := os.ReadFile("/etc/cloudground/agent.token")
	if err != nil {
		return result{Code: -1, Err: err.Error()}
	}
	curl := []string{"curl", "-sS", "--max-time", "20", "--unix-socket", agentSock, "-K", "-",
		"-X", "POST", "-H", "Content-Type: application/json", "-d", "{}", "-w", "\n%{http_code}", "http://agent/rpc/Ping"}
	args := curl
	if user != "" {
		pre := []string{"runuser", "-u", user}
		if group != "" {
			pre = append(pre, "-g", group)
		}
		args = append(append(pre, "--"), curl...)
	}
	cfg := fmt.Sprintf("header = \"Authorization: Bearer %s\"\n", strings.TrimSpace(string(tok)))
	return h.runIn(cfg, args[0], args[1:]...)
}

// R-2.5: the agent serves only root and the API's user, by peer credentials,
// whatever the socket's file mode lets through and even with the token.
// siteUser is a site's Unix user.
func (h *harness) agentPeerCredentials(t *testing.T, siteUser string) {
	// Controls: the request is well formed and the token right, so a refusal
	// below is the peer check and nothing else.
	r := h.agentPing("", "")
	h.assert(t, "control: root reaches the agent with the token", r.Code == 0 && strings.HasSuffix(strings.TrimSpace(r.Out), "200") && strings.Contains(r.Out, `"version"`),
		"root could not call the agent (exit %d): %s %s", r.Code, head([]byte(r.Out), 200), head([]byte(r.Err), 200))
	r = h.agentPing("cloudground", "")
	h.assert(t, "control: the API's user reaches the agent with the token", r.Code == 0 && strings.HasSuffix(strings.TrimSpace(r.Out), "200"),
		"the API's user could not call the agent (exit %d): %s %s", r.Code, head([]byte(r.Out), 200), head([]byte(r.Err), 200))

	// A site user in the socket's group passes the file mode: connect()
	// succeeds, and the agent must then close the connection unanswered.
	// curl reports that as 52, 55 or 56, and sometimes as 7 when the close
	// lands before its first write (seen on the box), so the exit code alone
	// cannot tell the peer check from the file mode: the journal check below,
	// which needs the agent to have accepted and refused this very uid, does.
	// The old agent answered with a JSON line (curl: exit 1), so this fails
	// on it.
	uid := strings.TrimSpace(h.run("id", "-u", siteUser).Out)
	since := strings.TrimSpace(h.run("date", "+%Y-%m-%d %H:%M:%S").Out)
	r = h.agentPing(siteUser, "cloudground")
	h.assert(t, "a site user in the socket's group is cut off by the agent", (r.Code == 7 || r.Code == 52 || r.Code == 55 || r.Code == 56) && !strings.Contains(r.Out, "version"),
		"a site user with the token got exit %d: %s %s", r.Code, head([]byte(r.Out), 200), head([]byte(r.Err), 200))
	logs := h.run("journalctl", "-u", "cloudground-agent", "--since", since, "-o", "cat", "--no-pager").Out
	h.assert(t, "the agent logged the refused peer's uid", strings.Contains(logs, "refused a connection") && strings.Contains(logs, `"uid":`+uid),
		"no refusal of uid %s in the agent's journal", uid)

	// Without the group, the socket's mode keeps a site user out before the
	// agent sees anything.
	r = h.agentPing(siteUser, "")
	h.assert(t, "a site user cannot open the agent socket", r.Code != 0 && !strings.Contains(r.Out, "version"),
		"a site user reached the agent (exit %d): %s", r.Code, head([]byte(r.Out), 200))
}

// R-2.2: an agent error seen by the browser carries no command line and no
// host path. Listing a directory that does not exist made the old agent
// answer "agent: lstat /srv/sites/<domain>/…: no such file or directory",
// which the API relayed as is.
func (h *harness) agentErrorsAreSafe(t *testing.T, siteID int64, domain string) {
	h.check(t, "control: the file manager lists the site", code(h.api.get(fmt.Sprintf("/api/sites/%d/files?path=", siteID))), "200")
	c, b := h.api.get(fmt.Sprintf("/api/sites/%d/files?path=e2e-nope1/e2e-nope2", siteID))
	body := string(b)
	h.assert(t, "control: listing a missing directory fails", c >= 400, "listing a missing directory answered %d: %s", c, head(b, 200))
	// R-3.1: a missing path is the request's problem, not a failed agent
	// (the old agent answered 502 "Could not list the files").
	h.assert(t, "R-3.1: listing a missing directory answers 404", c == 404, "listing a missing directory answered %d: %s", c, head(b, 200))
	leaks := []string{"/srv/", domain + "/", "lstat", "runuser", "exit status", "agent:"}
	var found []string
	for _, l := range leaks {
		if strings.Contains(body, l) {
			found = append(found, l)
		}
	}
	h.assert(t, "an agent error reaches the browser without paths or commands", len(found) == 0,
		"the browser got %v in: %s", found, head(b, 300))
}
