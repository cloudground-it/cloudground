//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

var fpmReloadRe = regexp.MustCompile(`^Reloading `)

// reloadDrain (R-5.2): what a PHP master reload does to a site's requests.
// Creating a site changes every other site's memory share; the site-count
// reconcile re-renders the pools that come out different and reloads their
// masters (R-0.7). php-fpm's reload waits process_control_timeout for the
// running requests, kills those still running, and serves nothing new
// meanwhile. That timeout was a fixed 10 s; it is now min(the request
// limit, 30 s) (phpfpm SPEC §5.2). Checked while one site's master reloads:
//
//   - a 20 s request survives it (on the old build it got a 502 at ~10 s);
//   - a fast uncached request sent during the reload waits for the drain,
//     at most the 30 s cap: it answers 200 within 35 s (the wait is logged);
//   - a 60 s request, longer than the cap, is ended at about 30 s and the
//     client gets a 502 or 504 (logged as what the cap costs).
//
// Whether this site's pool changes with the count depends on the box (RAM,
// CPUs, sites). When the reconcile left its master alone, the master is
// reloaded here the way the reconcile reloads a changed pool's master
// (systemctl reload: SIGUSR2); the log says which of the two happened.
func (h *harness) reloadDrain(t *testing.T) {
	long, other := h.dom("p52l"), h.dom("p52t")
	lid := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"php","php_version":%q}`, long, h.PHPV))
	if lid != 0 {
		defer func() { h.api.deleteSite(lid); h.api.teardownGone(lid) }()
	}
	if lid == 0 || h.api.waitActive(lid, 180*time.Second) != "active" {
		h.fail(t, "R-5.2: the long-request site provisions", "R-5.2: the long-request site did not provision")
		return
	}
	user := h.api.site(lid).SystemUser
	unit := "cloudground-php-" + user + ".service"
	pool, _ := os.ReadFile("/etc/cloudground-php/" + user + ".conf")
	h.assert(t, "R-5.2: a reload waits at most 30 s for running requests (process_control_timeout = 30s)",
		strings.Contains(string(pool), "process_control_timeout = 30s\n") && strings.Contains(string(pool), "request_terminate_timeout = 120s\n"),
		"the pool's reload drain is not capped at 30 s:\n%s", grepLines(string(pool), "_timeout"))
	// Settle the count change this site made, so the reconcile below is
	// caused by the other site alone.
	h.api.runMaintenance()

	docroot := "/srv/sites/" + long + "/current/"
	// The long request runs to a deadline in short steps. A plain sleep()
	// is not a long request: the reload's SIGQUIT to the workers interrupts
	// it (measured: a sleep(60) returned 57 at the reload, and the page
	// answered 200 after 2 s), so it ended at the reload with a 200 and
	// neither the drain nor the cap was tested.
	h.mustWriteAs(t, docroot+"cg-long.php", `<?php $end = microtime(true) + (int) ($_GET['s'] ?? 20); while (microtime(true) < $end) { usleep(100000); } echo 'cg-long-ok';`, user)
	h.mustWriteAs(t, docroot+"cg-fast.php", `<?php echo 'cg-fast-ok';`, user)
	defer h.remove(docroot+"cg-long.php", docroot+"cg-fast.php")

	oid := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"static"}`, other))
	if oid != 0 {
		defer func() { h.api.deleteSite(oid); h.api.teardownGone(oid) }()
	}
	h.check(t, "R-5.2: another site is created", h.api.waitActive(oid, 120*time.Second), "active")

	since := time.Now().Unix()
	inFlight := func(sec int) chan resp {
		ch := make(chan resp, 1)
		go func() {
			ch <- h.fetch(req{URL: fmt.Sprintf("https://%s/cg-long.php?s=%d&run=%d", long, sec, h.pid), Timeout: 200 * time.Second})
		}()
		return ch
	}
	// One after the other. On a small box the pool is "ondemand", and
	// php-fpm forks no second worker for a connection that arrives while the
	// worker it just forked has not yet taken its own (that worker still
	// counts as idle). Sent together, the 60 s request waited in the listen
	// queue behind the 20 s one (measured: queue 1, one worker, for 20 s),
	// the fast request behind both, and the drain was not tested.
	short := inFlight(20)
	time.Sleep(time.Second)
	overCap := inFlight(60)
	busy := 0
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
		if busy = h.poolWorkers(user); busy >= 2 {
			break
		}
	}
	h.assert(t, "R-5.2 control: both long requests are in workers before the reload", busy >= 2,
		"%d worker(s) of %s are running", busy, user)
	time.Sleep(time.Second)

	h.api.runMaintenance() // the site-count reconcile, now rather than at the next tick
	at := fmt.Sprintf("@%d", since)
	reloads := func() int {
		return CountLines(h.run("journalctl", "-u", unit, "--since", at, "-o", "cat").Out, fpmReloadRe)
	}
	how := "the site-count reconcile reloaded its master"
	if reloads() == 0 {
		how = "its pool came out unchanged, so the master was reloaded as the reconcile reloads a changed one"
		h.run("systemctl", "reload", unit)
	}
	t.Logf("R-5.2: %s", how)

	// The control is taken now, right after the reload: the fast request
	// below waits for the drain, that is for the 20 s request itself, so
	// once it answers the 20 s request is over either way. (Taken after it,
	// the control read a drained request as "completed before the reload".)
	var r resp
	gotShort := false
	select {
	case r = <-short:
		gotShort = true
		// Over already: either the reload ended it (the bug), or it
		// finished before any reload and nothing was tested.
		if r.Code == 200 {
			h.fail(t, "R-5.2 control: the master is reloaded while the requests run",
				"R-5.2: the 20 s request completed (%.0f s) before its master was reloaded: nothing was tested", r.Duration.Seconds())
			return
		}
	default:
		h.assert(t, "R-5.2 control: the master is reloaded while the requests run", reloads() > 0,
			"no reload of %s in the journal since the requests started", unit)
	}

	// A fast uncached page, sent while the master drains.
	fast := h.fetch(req{URL: fmt.Sprintf("https://%s/cg-fast.php?run=%d-%d", long, h.pid, time.Now().UnixNano()), Timeout: 120 * time.Second})
	t.Logf("R-5.2: an uncached request sent during the reload waited %.1f s (%s)", fast.Duration.Seconds(), fast.CodeStr())
	if !gotShort {
		r = <-short
	}
	h.assert(t, "R-5.2: a 20 s request survives a site creation on another site", r.Code == 200 && contains(r.Body, "cg-long-ok") && r.Duration >= 19*time.Second,
		"the request answered %s after %.0f s (%s; 502 near 10 s = the old process_control_timeout ended it at the reload; under 19 s = it did not run its time)",
		r.CodeStr(), r.Duration.Seconds(), how)
	h.assert(t, "R-5.2: an uncached request during a reload answers 200 within 35 s", fast.Code == 200 && contains(fast.Body, "cg-fast-ok") && fast.Duration <= 35*time.Second,
		"an uncached request sent during the reload answered %s after %.1f s", fast.CodeStr(), fast.Duration.Seconds())

	over := <-overCap
	t.Logf("R-5.2: a 60 s request running at the reload answered %s after %.1f s", over.CodeStr(), over.Duration.Seconds())
	h.assert(t, "R-5.2: a 60 s request running at a reload is ended at the 30 s cap (the client sees 502/504)",
		(over.Code == 502 || over.Code == 504) && over.Duration >= 25*time.Second && over.Duration < 58*time.Second,
		"a 60 s request running at the reload answered %s after %.1f s", over.CodeStr(), over.Duration.Seconds())
}

// poolWorkers is how many workers the site's PHP master runs ("php-fpm:
// pool <user>"); an ondemand pool forks none until a request needs one.
func (h *harness) poolWorkers(user string) int {
	return CountLines(h.run("ps", "-eo", "args=").Out, regexp.MustCompile(`^php-fpm: pool `+regexp.QuoteMeta(user)+`\s*$`))
}

// grepLines keeps the lines of s that contain sub.
func grepLines(s, sub string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}
