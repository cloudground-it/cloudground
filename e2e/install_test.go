//go:build e2e

package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/install"
	"github.com/cloudground-it/cloudground/internal/install/managed"
	"github.com/cloudground-it/cloudground/internal/units"
)

// installer (R-6.1): the server the Go installer set up is what it says it
// set up. `cgctl doctor` runs every step's check against the box and finds no
// drift; the files it renders are on disk byte for byte; Redis, no longer
// installed and never used, is not running; wp-cli is the pinned build.
func (h *harness) installer(t *testing.T) {
	r := h.run(cgctlBinary, "doctor")
	h.assert(t, "R-6.1: cgctl doctor finds no drift after the install", r.OK() && strings.Contains(r.Out, "no drift"), "%s", r)

	r = h.run(cgctlBinary, "install", "--plan")
	h.assert(t, "R-6.1: a second install would change nothing", r.OK() && !strings.Contains("\n"+r.Out, "\n+ "), "%s", r)

	h.installedModes(t)

	quic := strings.Contains(h.run("nginx", "-V").Err, "http_v3_module")
	for _, f := range managed.Files {
		if f.Kind != managed.Rendered {
			continue
		}
		want, err := install.Render(f, install.Vars{QUIC: quic})
		if err != nil {
			h.fail(t, "R-6.1: render "+f.Path, "%v", err)
			continue
		}
		got, err := os.ReadFile(f.Path)
		h.assert(t, "R-6.1: "+f.Path+" is the installer's render", err == nil && string(got) == string(want), "read: %v", err)
	}
	conf, _ := os.ReadFile(managed.NginxConf)
	h.assert(t, "R-6.1: nginx.conf carries the worker tuning", strings.Contains(string(conf), "worker_connections 4096;") &&
		strings.Contains(string(conf), "worker_rlimit_nofile 65535;"), "nginx.conf: %s", head(conf, 400))

	// Every path the installed units need exists (a missing one fails the
	// unit at NAMESPACE, as /var/lib/letsencrypt did on a fresh 26.04), and
	// the agent and the API run without having crash-looped.
	for _, name := range units.Names {
		b, err := os.ReadFile("/etc/systemd/system/" + name)
		if err != nil {
			h.fail(t, "R-6.1: read "+name, "%v", err)
			continue
		}
		for _, p := range units.RequiredPaths(strings.Split(string(b), "\n")) {
			_, err := os.Stat(p)
			h.assert(t, "R-6.1: "+name+" path "+p+" exists", err == nil, "%v", err)
		}
	}
	for _, u := range []string{"cloudground-agent", "cloudground-api"} {
		// NRestarts counts Restart='s restarts since the last start by hand:
		// a crash-looping unit has hundreds (519 on the box that found it).
		r = h.run("systemctl", "show", u, "-p", "ActiveState", "-p", "NRestarts")
		props := map[string]string{}
		for _, l := range strings.Split(r.Out, "\n") {
			if k, v, ok := strings.Cut(strings.TrimSpace(l), "="); ok {
				props[k] = v
			}
		}
		n, err := strconv.Atoi(props["NRestarts"])
		h.assert(t, "R-6.1: "+u+" is active and not crash-looping", r.OK() && props["ActiveState"] == "active" && err == nil && n < 3, "%s", r)
	}

	r = h.run("systemctl", "is-active", "redis-server")
	h.assert(t, "R-6.1: Redis is not running", strings.TrimSpace(r.Out) != "active", "%s", r)

	got := "missing"
	if wp, err := os.ReadFile("/usr/local/bin/wp"); err == nil {
		sum := sha256.Sum256(wp)
		got = hex.EncodeToString(sum[:])
	}
	h.check(t, "R-6.1: wp-cli is the pinned build", got, install.WPCLISHA256)

	h.needrestartSkipsHelpers(t)

	// Re-converging applies what it rewrote: a drifted bootstrap vhost is
	// put back, and nginx is reloaded to serve it.
	vhost, err := os.ReadFile(managed.Vhost)
	if err != nil {
		h.fail(t, "R-6.1: read the bootstrap vhost", "%v", err)
		return
	}
	restore := h.undo("put the bootstrap vhost back", func() { _ = os.WriteFile(managed.Vhost, vhost, 0o644) })
	if err := os.WriteFile(managed.Vhost, append(append([]byte{}, vhost...), "# e2e drift\n"...), 0o644); err != nil {
		h.fail(t, "R-6.1: drift the bootstrap vhost", "%v", err)
		return
	}
	r = h.run(cgctlBinary, "doctor")
	h.assert(t, "R-6.1: doctor reports the drifted vhost", r.Code == 1 && strings.Contains(r.Out, "+ nginx.vhost"), "%s", r)
	since := time.Now().Add(-time.Second).Format("2006-01-02 15:04:05")
	r = h.run(cgctlBinary, "install")
	h.assert(t, "R-6.1: cgctl install re-converges", r.OK(), "%s", r)
	again, _ := os.ReadFile(managed.Vhost)
	h.assert(t, "R-6.1: the vhost is the render again", string(again) == string(vhost), "vhost: %s", head(again, 200))
	j := h.run("journalctl", "-u", "nginx", "--since", since, "-o", "cat")
	h.assert(t, "R-6.1: nginx was reloaded after the vhost was rewritten", strings.Contains(j.Out, "Reload"), "%s", j)
	r = h.run(cgctlBinary, "doctor")
	h.assert(t, "R-6.1: no drift after re-converging", r.OK(), "%s", r)
	restore()

	// H-3-2: the agent's token readable by every account is drift, and a
	// repair puts its mode back without rewriting it.
	const tokenFile = "/etc/cloudground/agent.token"
	tok, terr := os.ReadFile(tokenFile)
	if terr == nil && os.Chmod(tokenFile, 0o644) == nil {
		defer h.undo("put the agent token's mode back", func() { _ = os.Chmod(tokenFile, 0o640) })()
		r = h.run(cgctlBinary, "doctor")
		h.assert(t, "H-3-2: doctor reports a token readable by every account", r.Code == 1 && strings.Contains(r.Out, "+ token.agent"), "%s", r)
		r = h.run(cgctlBinary, "install", "--skip-packages")
		mode := os.FileMode(0)
		if fi, err := os.Stat(tokenFile); err == nil {
			mode = fi.Mode().Perm()
		}
		again, _ := os.ReadFile(tokenFile)
		h.assert(t, "H-3-2: cgctl install puts the token's mode back, its content kept",
			r.OK() && mode == 0o640 && string(again) == string(tok), "%s; mode %04o", r, mode)
		h.api.relogin()
	} else {
		h.fail(t, "H-3-2 control: loosen the agent token's mode", "%v", terr)
	}

	h.installerFailsOnADeadAgent(t)
}

// needrestartSkipsHelpers (#125): needrestart, run after a library upgrade,
// offers to restart the services still running a replaced binary; the agent's transient
// units must not be among them (restarted halfway, systemd would run the
// command again from the start). Two units run a copy of sleep that is then
// replaced on disk: a helper-named one (cg-helper-test-1) and a control with
// another name, which must be listed, or the check proves nothing.
func (h *harness) needrestartSkipsHelpers(t *testing.T) {
	if _, err := exec.LookPath("needrestart"); err != nil {
		h.step("needrestart is not installed: its check is skipped")
		return
	}
	_, err := os.Stat(managed.Needrestart)
	h.assert(t, "#125: the needrestart override is installed", err == nil, "%v", err)
	dir := "/run/cg-e2e-needrestart"
	h.assert(t, "#125: make the test binary", h.run("sh", "-c", "mkdir -p "+dir+" && cp /bin/sleep "+dir+"/sleep").OK(), "")
	defer h.undo("remove the needrestart test units", func() {
		_ = exec.Command("systemctl", "stop", "cg-helper-test-1.service", "cg-e2e-nr-control.service").Run()
		_ = os.RemoveAll(dir)
	})()
	for _, u := range []string{"cg-helper-test-1", "cg-e2e-nr-control"} {
		r := h.run("systemd-run", "--quiet", "--unit="+u, dir+"/sleep", "300")
		h.assert(t, "#125: start "+u, r.OK(), "%s", r)
	}
	// Replaced, not edited: the running units keep the old inode, as after a
	// package upgrade.
	h.assert(t, "#125: replace the test binary", h.run("sh", "-c", "cp /bin/sleep "+dir+"/new && mv "+dir+"/new "+dir+"/sleep").OK(), "")
	// List mode, not -b: the batch listing prints every outdated service
	// before it looks at the overrides, so it would show the helper too.
	r := h.run("env", "DEBIAN_FRONTEND=noninteractive", "needrestart", "-r", "l")
	out := r.Out + r.Err // the listing goes to stderr without a terminal
	restart := ""
	if _, after, ok := strings.Cut(out, "Services to be restarted:"); ok {
		restart, _, _ = strings.Cut(after, "\n\n")
	}
	h.assert(t, "#125: needrestart offers to restart the control unit (the check can see a unit)", strings.Contains(restart, "cg-e2e-nr-control"), "%s", r)
	h.assert(t, "#125: needrestart does not offer to restart a helper unit", !strings.Contains(restart, "cg-helper-test-1") &&
		strings.Contains(out, "cg-helper-test-1"), "%s", r)
}

// installerFailsOnADeadAgent: an agent that cannot start (here a drop-in
// naming a path that does not exist, the NAMESPACE failure of a fresh 26.04)
// makes `cgctl install` fail, naming the unit and showing its journal,
// instead of printing "CloudGround is running"; doctor reports it; once the
// cause is gone the agent comes back.
func (h *harness) installerFailsOnADeadAgent(t *testing.T) {
	const dropDir = "/etc/systemd/system/cloudground-agent.service.d"
	drop := dropDir + "/zz-cg-e2e-missing-path.conf"
	missing := "/var/lib/cg-e2e-missing-path"
	_ = os.RemoveAll(missing)
	back := h.undo("bring the agent back", func() {
		_ = os.Remove(drop)
		h.run("systemctl", "daemon-reload")
		h.run("systemctl", "reset-failed", "cloudground-agent")
		h.run("systemctl", "restart", "cloudground-agent")
		for i := 0; i < 30; i++ {
			if c, _, err := h.agentCall("Ping", "{}"); err == nil && c == 200 {
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
	})
	defer back()
	if err := os.MkdirAll(dropDir, 0o755); err != nil {
		h.fail(t, "R-6.1: make the agent's drop-in directory", "%v", err)
		return
	}
	if err := os.WriteFile(drop, []byte("[Service]\nReadWritePaths="+missing+"\n"), 0o644); err != nil {
		h.fail(t, "R-6.1: write the drop-in", "%v", err)
		return
	}
	h.run("systemctl", "daemon-reload")
	h.run("systemctl", "restart", "--no-block", "cloudground-agent")

	r := h.run(cgctlBinary, "doctor")
	h.assert(t, "R-6.1: doctor reports an agent that cannot start", r.Code == 1 && strings.Contains(r.Out, "+ services"), "%s", r)
	r = h.run(cgctlBinary, "install")
	out := r.Out + r.Err
	h.assert(t, "R-6.1: cgctl install fails when the agent cannot start", !r.OK() && !strings.Contains(out, "CloudGround is running"), "%s", r)
	h.assert(t, "R-6.1: the failure names the agent and shows its journal", strings.Contains(out, "cloudground-agent is not active") &&
		strings.Contains(out, "journalctl -u cloudground-agent") && strings.Contains(out, missing), "%s", r)

	back()
	r = h.run("systemctl", "is-active", "cloudground-agent")
	h.assert(t, "R-6.1: the agent is back once the cause is gone", r.Trim() == "active", "%s", r)
	r = h.run(cgctlBinary, "doctor")
	h.assert(t, "R-6.1: no drift once the agent is back", r.OK(), "%s", r)
}
