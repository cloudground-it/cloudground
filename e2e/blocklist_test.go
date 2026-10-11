//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// The block list's client (#21): a network namespace joined to the host by a
// veth pair, so its address is a real remote one to sshd, nginx, the panel
// and netfilter, and not one of this machine's (which are never blocked).
const (
	blNS     = "cge2ebl"
	blHostIf = "cge2ebl0"
	blPeerIf = "cge2ebl1"
	blHost   = "198.18.77.1"
	blClient = "198.18.77.2"
)

// blocklistView is the part of GET /api/blocklist the section reads.
type blocklistView struct {
	Settings json.RawMessage `json:"settings"`
	Blocked  []struct {
		Address string `json:"address"`
		Source  string `json:"source"`
	} `json:"blocked"`
	Allowed []struct {
		Address string `json:"address"`
	} `json:"allowed"`
	Enforced bool `json:"enforced"`
	Watching struct {
		SSH       bool `json:"ssh"`
		WordPress bool `json:"wordpress"`
	} `json:"watching"`
}

func (h *harness) blocklistGet() blocklistView {
	_, b := h.api.get("/api/blocklist")
	var v blocklistView
	_ = json.Unmarshal(b, &v)
	return v
}

// blockedBy is the source of the block of the client's address, "" when
// there is none.
func (v blocklistView) blockedBy(addr string) string {
	for _, e := range v.Blocked {
		if e.Address == addr+"/32" {
			return e.Source
		}
	}
	return ""
}

// blocklist (#21): failed sign-ins at SSH, WordPress and the panel from a
// remote address block it on every port once they reach the threshold;
// netfilter drops it while the server itself still answers; unblocking lets
// it back in; an allowed address is never blocked; a block that would hold
// the server is refused; a look-alike sshd line from outside the SSH unit and
// a datagram from outside nginx count for nothing.
func (h *harness) blocklistSection(t *testing.T) {
	f, ok := h.featureSite(t)
	if !ok {
		return
	}
	if !h.run("nft", "--version").OK() {
		h.fail(t, "#21: nft is installed", "nft --version failed: the installer installs nftables")
		return
	}
	// The client namespace.
	h.run("ip", "netns", "del", blNS)
	h.run("ip", "link", "del", blHostIf)
	teardown := h.undo("remove the block list's client namespace", func() {
		h.run("ip", "netns", "del", blNS)
		h.run("ip", "link", "del", blHostIf)
	})
	defer teardown()
	for _, c := range [][]string{
		{"ip", "netns", "add", blNS},
		{"ip", "link", "add", blHostIf, "type", "veth", "peer", "name", blPeerIf},
		{"ip", "link", "set", blPeerIf, "netns", blNS},
		{"ip", "addr", "add", blHost + "/30", "dev", blHostIf},
		{"ip", "link", "set", blHostIf, "up"},
		{"ip", "netns", "exec", blNS, "ip", "addr", "add", blClient + "/30", "dev", blPeerIf},
		{"ip", "netns", "exec", blNS, "ip", "link", "set", blPeerIf, "up"},
		{"ip", "netns", "exec", blNS, "ip", "link", "set", "lo", "up"},
		{"ip", "netns", "exec", blNS, "ip", "route", "add", "default", "via", blHost},
	} {
		if r := h.run(c[0], c[1:]...); !r.OK() {
			h.fail(t, "#21: the client namespace is set up", "%s: %s", strings.Join(c, " "), r)
			return
		}
	}
	inNS := func(args ...string) result { return h.run("ip", append([]string{"netns", "exec", blNS}, args...)...) }
	// reaches reports whether the client gets an HTTP answer from the panel.
	reaches := func() bool {
		return inNS("curl", "-sk", "-m", "5", "-o", "/dev/null", "https://"+h.IP+":8443/").OK()
	}
	h.assert(t, "#21: control: the client reaches the panel", reaches(), "curl from %s to %s:8443 failed", blClient, h.IP)

	// A low threshold for the section, the settings put back after it.
	_, before := h.api.get("/api/blocklist")
	var saved blocklistView
	_ = json.Unmarshal(before, &saved)
	restore := h.undo("put the block list's settings back", func() {
		h.api.put("/api/blocklist/settings", saved.Settings)
		for _, path := range []string{"/api/blocklist/blocks", "/api/blocklist/allowed"} {
			h.api.do("DELETE", path, map[string]string{"address": blClient})
		}
	})
	defer restore()
	c, _ := h.api.put("/api/blocklist/settings", `{"enabled":true,"max_failures":3,"window_minutes":10,"ban_minutes":5}`)
	h.check(t, "#21: the settings take a threshold of 3", fmt.Sprint(c), "200")

	// waitBlock polls the list until the client is blocked by source.
	waitBlock := func(source string) bool {
		deadline := time.Now().Add(45 * time.Second)
		for time.Now().Before(deadline) {
			if h.blocklistGet().blockedBy(blClient) == source {
				return true
			}
			time.Sleep(2 * time.Second)
		}
		return false
	}
	unblock := func(what string) {
		c, _ := h.api.do("DELETE", "/api/blocklist/blocks", map[string]string{"address": blClient})
		h.check(t, "#21: "+what+": the block is removed", fmt.Sprint(c), "200")
		ok := false
		for range 10 {
			if ok = reaches(); ok {
				break
			}
			time.Sleep(time.Second)
		}
		h.assert(t, "#21: "+what+": unblocked, the client reaches the panel again", ok, "the client still cannot reach %s:8443", h.IP)
	}
	v := h.blocklistGet()
	h.assert(t, "#21: the agent reads sshd's journal and the vhosts' failure log", v.Watching.SSH && v.Watching.WordPress, "watching = %+v", v.Watching)

	// SSH: three unknown users.
	for range 3 {
		inNS("ssh", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
			"-o", "ConnectTimeout=5", "-p", h.sshPort(), "cg-e2e-nobody@"+h.IP, "true")
	}
	h.assert(t, "#21: three failed SSH logins block the address", waitBlock("ssh"), "blocked: %+v", h.blocklistGet().Blocked)
	set := h.run("nft", "list", "set", "inet", "cloudground", "block4").Out
	h.assert(t, "#21: netfilter holds the block", strings.Contains(set, blClient), "nft list set inet cloudground block4:\n%s", set)
	h.assert(t, "#21: the blocked client no longer reaches the panel", !reaches(), "the blocked client still reached %s:8443", h.IP)
	banner := inNS("timeout", "5", "bash", "-c", "exec 3<>/dev/tcp/"+h.IP+"/"+h.sshPort()+" && head -c 4 <&3").Out
	h.assert(t, "#21: the blocked client no longer reaches SSH", !strings.HasPrefix(banner, "SSH-"), "sshd still answered the blocked client: %q", banner)
	h.check(t, "#21: the server itself still reaches the panel",
		fmt.Sprint(h.run("curl", "-sk", "-m", "5", "-o", "/dev/null", "https://"+h.IP+":8443/").OK()), "true")
	unblock("SSH")

	// WordPress: three wrong passwords at wp-login.php. The connector's mark
	// never reaches the client.
	resolve := f.fs + ":443:" + h.IP
	var marked bool
	for range 3 {
		r := inNS("curl", "-sk", "-m", "15", "-D", "-", "-o", "/dev/null", "--resolve", resolve,
			"-d", "log=cg-e2e-nobody&pwd=wrong-password", "https://"+f.fs+"/wp-login.php")
		marked = marked || strings.Contains(strings.ToLower(r.Out), "x-cloudground-auth")
	}
	h.assert(t, "#21: the failed sign-in's mark is hidden from the client", !marked, "a wp-login.php answer carried X-CloudGround-Auth")
	h.assert(t, "#21: three failed WordPress sign-ins block the address", waitBlock("wordpress"), "blocked: %+v", h.blocklistGet().Blocked)
	unblock("WordPress")

	// The panel: three wrong passwords at its sign-in.
	for range 3 {
		inNS("curl", "-sk", "-m", "30", "-o", "/dev/null", "-H", "Content-Type: application/json",
			"-d", `{"email":"cg-e2e-nobody@example.com","password":"wrong-password-e2e"}`, "https://"+h.IP+":8443/api/login")
	}
	h.assert(t, "#21: three failed panel sign-ins block the address", waitBlock("panel"), "blocked: %+v", h.blocklistGet().Blocked)
	unblock("panel")

	// The allowlist: failures from an allowed address block nothing.
	c, _ = h.api.post("/api/blocklist/allowed", map[string]string{"address": blClient, "note": "e2e"})
	h.check(t, "#21: the client is allowed", fmt.Sprint(c), "200")
	for range 4 {
		inNS("ssh", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
			"-o", "ConnectTimeout=5", "-p", h.sshPort(), "cg-e2e-nobody@"+h.IP, "true")
	}
	time.Sleep(25 * time.Second) // two of the watcher's rounds
	h.check(t, "#21: an allowed address is not blocked", h.blocklistGet().blockedBy(blClient), "")
	h.assert(t, "#21: an allowed address still reaches the panel", reaches(), "the allowed client cannot reach the panel")
	c, _ = h.api.do("DELETE", "/api/blocklist/allowed", map[string]string{"address": blClient})
	h.check(t, "#21: the client is taken off the allowlist", fmt.Sprint(c), "200")

	// By hand: the client is blocked and let in again; a network holding the
	// server's own address is refused.
	c, _ = h.api.post("/api/blocklist/blocks", map[string]any{"address": blClient, "note": "e2e", "minutes": 5})
	h.check(t, "#21: a block by hand is accepted", fmt.Sprint(c), "200")
	h.assert(t, "#21: a block by hand cuts the client off", !reaches(), "the client blocked by hand still reached the panel")
	unblock("by hand")
	c, _ = h.api.post("/api/blocklist/blocks", map[string]any{"address": blHost + "/30"})
	h.check(t, "#21: a block holding the server's own address is refused", fmt.Sprint(c), "400")
	c, _ = h.api.post("/api/blocklist/blocks", map[string]any{"address": "0.0.0.0/0"})
	h.check(t, "#21: a block of every address is refused", fmt.Sprint(c), "400")

	// What cannot feed the list: an sshd look-alike written to the journal
	// by root outside the SSH unit, and a datagram from a user other than
	// nginx's (the socket refuses it).
	for range 5 {
		h.run("logger", "-t", "sshd", "Failed password for root from "+blClient+" port 22 ssh2")
	}
	sock := "/run/cloudground/auth-log.sock"
	owner, mode := statOwnerMode(sock)
	h.check(t, "#21: the failure log socket is root's, written by nginx's group only", owner+" "+mode, "root 620")
	sent := h.as("nobody", "logger", "--socket-errors=on", "-u", sock, "-t", "cloudground", blClient+" failed=50")
	h.assert(t, "#21: another user cannot write to the failure log socket", !sent.OK(), "logger as nobody: %s", sent)
	time.Sleep(25 * time.Second)
	h.check(t, "#21: look-alike failures block nothing", h.blocklistGet().blockedBy(blClient), "")
	v = h.blocklistGet()
	h.assert(t, "#21: the list is enforced", v.Enforced, "enforced = false")
	h.assert(t, "#21: nothing of the section is left blocked", !slices.ContainsFunc(v.Blocked, func(e struct {
		Address string `json:"address"`
		Source  string `json:"source"`
	}) bool {
		return strings.HasPrefix(e.Address, "198.18.77.")
	}), "blocked: %+v", v.Blocked)
}
