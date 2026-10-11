//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ---------------------------------------------- R-3.10: ops/system ------

type logTail struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (h *harness) logTail(path string) (int, logTail) {
	c, b := h.api.get(path)
	var l logTail
	_ = json.Unmarshal(b, &l)
	return c, l
}

// systemLogs: the Logs page reads the journal (now decoded from
// `journalctl -o json`) and a site's own files.
func (f *feat) systemLogs(t *testing.T) {
	h := f.h
	c, agent := h.logTail("/api/logs?source=agent&lines=20")
	h.check(t, "R-3.10: the agent's journal answers", fmt.Sprint(c), "200")
	h.check(t, "R-3.10: the agent's journal is labelled", agent.Path, "journal:cloudground-agent")
	first, _, _ := strings.Cut(agent.Content, "\n")
	stamp, _, _ := strings.Cut(first, " ")
	_, perr := time.Parse(time.RFC3339, stamp)
	h.assert(t, "R-3.10: agent journal lines start with an ISO time", perr == nil && agent.Content != "-- No entries --",
		"first line %q", first)
	n := strings.Count(agent.Content, "\n") + 1
	h.assert(t, "R-3.10: the journal tail honours lines", n <= 20, "%d lines for lines=20", n)
	c, php := h.logTail("/api/logs?source=php-error&lines=20")
	h.check(t, "R-3.10: the site PHP masters' journal answers", fmt.Sprint(c), "200")
	h.check(t, "R-3.10: the PHP journal is labelled", php.Path, "journal:php-fpm")

	marker := fmt.Sprintf("cg-e2e-log-%d", h.pid)
	h.get(f.fs, "/?"+marker)
	// nginx flushes the site's access log every second (flush=1s): read the
	// tail until it shows the request, within the 2 s this check always
	// allowed for the flush.
	var acc logTail
	for end := time.Now().Add(2 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		c, acc = h.logTail(f.api("/logs?source=access&lines=50"))
		if strings.Contains(acc.Content, marker) || time.Now().After(end) {
			break
		}
	}
	h.check(t, "R-3.10: the site access log answers", fmt.Sprint(c), "200")
	h.check(t, "R-3.10: the site access log is the site's file", acc.Path, "/var/log/cloudground/"+f.fs+"/access.log")
	h.assert(t, "R-3.10: the access log tail shows the last request", strings.Contains(acc.Content, marker),
		"no %s in the tail", marker)
}

// driftPlanted (R-3.10): the connector's directory belongs to the site, whose
// user can put a symlink or a FIFO where the drift check hashes. A symlink
// failed the whole check (ELOOP) for every site; a FIFO blocked it in open(2)
// until the RPC deadline. Both are now drift of that one file.
func (f *feat) driftPlanted(t *testing.T) {
	h := f.h
	conn := f.fp + "/wp-content/mu-plugins/cloudground-connector.php"
	orig, err := os.ReadFile(conn)
	fi, serr := os.Lstat(conn)
	if err != nil || serr != nil {
		t.Logf("fixture: %v %v", err, serr)
		return
	}
	st := fi.Sys().(*syscall.Stat_t)
	// The plants are the site user's, as an attacker's would be.
	uid, gid, _ := lookupIDs(f.fsu)
	restore := h.undo("restore the connector after the drift plants", func() {
		_ = os.Remove(conn)
		if err := os.WriteFile(conn, orig, fi.Mode().Perm()); err == nil {
			_ = os.Chmod(conn, fi.Mode().Perm())
			_ = os.Lchown(conn, int(st.Uid), int(st.Gid))
		}
		h.api.get("/api/system/drift") // resolves the events as restored
	})

	h.remove(conn)
	if err := os.Symlink("/etc/hostname", conn); err != nil {
		t.Logf("fixture: %v", err)
	}
	_ = os.Lchown(conn, uid, gid)
	c, b := h.api.get("/api/system/drift")
	h.check(t, "R-3.10: a symlinked connector does not fail the drift check", fmt.Sprint(c), "200")
	h.assert(t, "R-3.10: the symlinked connector is reported as drift", contains(b, conn), "drift = %s", head(b, 300))

	h.remove(conn)
	if err := syscall.Mkfifo(conn, 0o644); err != nil {
		t.Logf("fixture: %v", err)
	}
	_ = os.Lchown(conn, uid, gid)
	type answer struct {
		code int
		body []byte
	}
	done := make(chan answer, 1)
	go func() { c, b := h.api.get("/api/system/drift"); done <- answer{c, b} }()
	var a answer
	select {
	case a = <-done:
	case <-time.After(30 * time.Second):
		// The old agent sits in open(2): give it a writer and EOF, so the
		// call (and the agent's goroutine) can finish.
		if w, err := os.OpenFile(conn, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			w.Close()
		}
		a = <-done
		a.code = -1
	}
	h.check(t, "R-3.10: a FIFO at the connector does not block the drift check", fmt.Sprint(a.code), "200")
	h.assert(t, "R-3.10: the FIFO is reported as drift", contains(a.body, conn), "drift = %s", head(a.body, 300))
	restore()
}

// firewallGuards (R-3.10): the panel's own port is essential, and deleting a
// source-limited rule deletes that rule. `ufw show added` lists the rules
// whether or not ufw is active.
func (h *harness) firewallGuards(t *testing.T) {
	if !h.run("ufw", "version").OK() {
		t.Logf("(ufw is not installed: the firewall guards are not checked)")
		return
	}
	post := func(body string) int {
		c, _ := h.api.post("/api/firewall/rule", body)
		return c
	}
	// The old agent ran both of these: put 8443 back as the installer left it.
	repair := h.undo("reopen the panel port", func() {
		h.run("ufw", "delete", "deny", "8443/tcp")
		h.run("ufw", "allow", "8443/tcp")
	})
	denied := post(`{"action":"deny","port":8443,"proto":"tcp"}`)
	h.assert(t, "R-3.10: the firewall refuses to deny the panel port", denied >= 400, "deny 8443 answered %d", denied)
	deleted := post(`{"action":"delete","port":8443,"proto":"tcp"}`)
	h.assert(t, "R-3.10: the firewall refuses to delete the panel port's rule", deleted >= 400, "delete 8443 answered %d", deleted)
	if denied < 400 || deleted < 400 {
		repair()
	}
	// A source of every address is a port-wide deny in disguise: the old
	// guard took 0.0.0.0/0 for "one source" and ran it.
	repairAny := h.undo("remove the e2e deny from every address on the panel port", func() {
		h.run("ufw", "delete", "deny", "from", "0.0.0.0/0", "to", "any", "port", "8443", "proto", "tcp")
	})
	deniedAny := post(`{"action":"deny","port":8443,"proto":"tcp","from":"0.0.0.0/0"}`)
	h.assert(t, "R-9.2: the firewall refuses to deny the panel port from 0.0.0.0/0", deniedAny >= 400, "deny 8443 from 0.0.0.0/0 answered %d", deniedAny)
	if deniedAny < 400 {
		repairAny()
	}

	// W3: a deny for one source on an essential port only narrows access:
	// it is accepted, and deleting it again must be too (the old guard
	// refused every delete on 8443, so the deny could not be undone). The
	// port's own allow rule stays.
	const blocked = "203.0.113.78"
	unblock := h.undo("remove the e2e deny on the panel port", func() {
		h.run("ufw", "delete", "deny", "from", blocked, "to", "any", "port", "8443", "proto", "tcp")
	})
	h.check(t, "W3: a deny for one source on the panel port is accepted",
		fmt.Sprint(post(`{"action":"deny","port":8443,"proto":"tcp","from":"`+blocked+`"}`)), "200")
	h.check(t, "W3: that deny is deleted again",
		fmt.Sprint(post(`{"action":"delete","port":8443,"proto":"tcp","from":"`+blocked+`"}`)), "200")
	shown := h.run("ufw", "show", "added").Out
	h.assert(t, "W3: deleting the deny removed it", !strings.Contains(shown, blocked), "ufw show added:\n%s", shown)
	h.assert(t, "W3: the panel port's allow rule stayed", strings.Contains(shown, "ufw allow 8443/tcp"), "ufw show added:\n%s", shown)
	unblock()

	const port, src = "65011", "203.0.113.77"
	cleanup := h.undo("remove the firewall test rules", func() {
		h.run("ufw", "delete", "allow", port+"/tcp")
		h.run("ufw", "delete", "allow", "from", src, "to", "any", "port", port, "proto", "tcp")
	})
	defer cleanup()
	h.check(t, "R-3.10: a port is opened", fmt.Sprint(post(`{"action":"allow","port":`+port+`,"proto":"tcp"}`)), "200")
	h.check(t, "R-3.10: a port is opened to one source", fmt.Sprint(post(`{"action":"allow","port":`+port+`,"proto":"tcp","from":"`+src+`"}`)), "200")
	// W3: with ufw inactive, `ufw status` prints no rules though ufw keeps
	// them; the page showed none. The API lists the stored ones.
	_, fw := h.api.get("/api/firewall")
	h.assert(t, "W3: the firewall status lists the rules ufw holds, active or not", strings.Contains(string(fw), port),
		"GET /api/firewall: %s (ufw: %s)", head(fw, 400), h.run("ufw", "status").Trim())
	h.check(t, "R-3.10: the source-limited rule is deleted", fmt.Sprint(post(`{"action":"delete","port":`+port+`,"proto":"tcp","from":"`+src+`"}`)), "200")
	added := h.run("ufw", "show", "added").Out
	h.assert(t, "R-3.10: deleting the source rule keeps the port's own rule", strings.Contains(added, "ufw allow "+port+"/tcp"),
		"ufw show added:\n%s", added)
	h.assert(t, "R-3.10: deleting the source rule removes it", !strings.Contains(added, src), "ufw show added:\n%s", added)
}
