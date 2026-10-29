package install

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/install/managed"
)

var bg = context.Background()

// A fresh server needs every step; after the install, nothing; the files
// are as rendered, with their modes and owners.
func TestFreshInstallThenNothingToDo(t *testing.T) {
	for _, distro := range []string{"24.04", "26.04"} {
		t.Run(distro, func(t *testing.T) {
			m := newMachine(t, distro)
			m.pins()
			m.cmd.tools["ufw"] = false
			o := m.options(false)
			o.From = m.build("v1")

			before := needed(Plan(bg, Steps(m.env, o)))
			for _, id := range []string{"packages.base", "packages.main", "tools.restic", "tools.wp-cli", "tools.composer",
				"user.cloudground", "dirs", "panel.env", "token.agent", "cert.panel", "cert.panel-ip", "cert.fallback",
				"binaries", "sysctl", "nginx.tuning", "nginx.default-site", "nginx.vhost", "mariadb", "units", "services",
				"sftp.group", "sshd.sftp", "logrotate", "certbot.hook-dir", "certbot.hook"} {
				if !slices.Contains(before, id) {
					t.Errorf("fresh server: %s not needed", id)
				}
			}
			if got := slices.Contains(before, "packages.ppa"); got != (distro == "24.04") {
				t.Errorf("packages.ppa needed = %v on %s", got, distro)
			}

			m.mustApply(o)
			if after := needed(Plan(bg, Steps(m.env, o))); len(after) != 0 {
				t.Fatalf("after the install, still needed: %v", after)
			}
			// A second install changes nothing at all.
			calls := len(m.sd.calls)
			m.mustApply(o)
			if len(m.sd.calls) != calls {
				t.Errorf("a second install ran systemctl again: %v", m.sd.calls[calls:])
			}

			php := map[string]string{"24.04": "8.4", "26.04": "8.5"}[distro]
			if got := m.read("/etc/cloudground/panel.env"); got != "CLOUDGROUND_PHP_VERSION="+php+"\n" {
				t.Errorf("panel.env = %q", got)
			}
			for _, b := range Binaries {
				if got := m.read("/usr/local/bin/" + b); !strings.Contains(got, b+" v1") {
					t.Errorf("%s not installed from the build: %q", b, got)
				}
			}
			if m.exists("/etc/nginx/sites-enabled/default") {
				t.Error("the distribution's default site is still enabled")
			}
			for _, f := range managed.Files {
				fi, err := os.Stat(m.path(f.Path))
				if err != nil {
					t.Errorf("%s: %v", f.Path, err)
					continue
				}
				if fi.Mode().Perm() != f.Mode {
					t.Errorf("%s: mode %04o, want %04o", f.Path, fi.Mode().Perm(), f.Mode)
				}
			}
			for _, u := range []string{"enable --now mariadb", "daemon-reload", "enable --now cloudground-api.socket cloudground-agent cloudground-api nginx", "reload nginx"} {
				if !m.sd.did(u) {
					t.Errorf("systemctl %s was not run (calls %v)", u, m.sd.calls)
				}
			}
			if slices.ContainsFunc(m.sd.calls, func(c string) bool { return strings.HasPrefix(c, "restart") }) {
				t.Errorf("a fresh install restarted something: %v", m.sd.calls)
			}
			if m.cmd.ran("sysctl -p /etc/sysctl.d/60-cloudground.conf") != 1 || m.cmd.ran("sshd -t") != 1 || m.cmd.ran("logrotate --debug /etc/logrotate.d/cloudground") != 1 {
				t.Errorf("checks after writing not run once each: %v", m.cmd.calls)
			}
			for _, pkg := range []string{"redis-server", "php" + php + "-redis"} {
				if m.pkg.installed[pkg] {
					t.Errorf("%s installed on a new server (R-6.1)", pkg)
				}
			}
		})
	}
}

func (m *machine) exists(p string) bool {
	_, err := os.Lstat(m.path(p))
	return err == nil
}

// Directories, secrets and certificates: modes and owners of SPEC §3.
func TestUsersDirectoriesSecrets(t *testing.T) {
	m := newMachine(t, "24.04")
	o := m.options(false)
	o.SkipPackages = true
	m.mustApply(o)
	uid, gid, _ := m.acct.User("cloudground")
	cg := [2]int{uid, gid}
	for _, d := range []struct {
		path  string
		mode  os.FileMode
		owner [2]int
	}{
		{"/srv/sites", 0o755, [2]int{}},
		{"/var/lib/cloudground", 0o750, cg},
		{"/var/lib/cloudground-agent/work", 0o700, [2]int{}},
		{"/etc/cloudground", 0o750, [2]int{0, gid}},
		{"/etc/cloudground/certs", 0o750, [2]int{0, gid}},
		{"/run/cloudground/php", 0o755, [2]int{}},
		{"/etc/cloudground/agent.token", 0o640, [2]int{0, gid}},
		{"/etc/cloudground/panel.env", 0o640, [2]int{0, gid}},
		{"/etc/cloudground/certs/panel.pem", 0o600, [2]int{0, gid}},
		{"/etc/cloudground/certs/panel.key", 0o640, [2]int{0, gid}},
		{"/etc/cloudground/certs/panel-ip.pem", 0o600, [2]int{}},
		{"/etc/cloudground/certs/panel-ip.key", 0o640, [2]int{}},
		{"/etc/cloudground/certs/fallback.pem", 0o644, [2]int{}},
		{"/etc/cloudground/certs/fallback.key", 0o640, [2]int{0, 33}},
	} {
		fi, err := os.Stat(m.path(d.path))
		if err != nil {
			t.Errorf("%s: %v", d.path, err)
			continue
		}
		if fi.Mode().Perm() != d.mode {
			t.Errorf("%s: mode %04o, want %04o", d.path, fi.Mode().Perm(), d.mode)
		}
		if got := m.fs.owner(m.path(d.path)); got != d.owner {
			t.Errorf("%s: owner %v, want %v", d.path, got, d.owner)
		}
	}
	tok := m.read("/etc/cloudground/agent.token")
	if len(tok) != 48 || strings.Trim(tok, tokenAlphabet) != "" {
		t.Errorf("agent token %q: want 48 characters of [A-Za-z0-9], no newline", tok)
	}
	for name, cn := range map[string]string{"panel": "cloudground-panel", "panel-ip": "cloudground-panel", "fallback": "fallback.invalid"} {
		block, _ := pem.Decode([]byte(m.read("/etc/cloudground/certs/" + name + ".pem")))
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		if cert.Subject.CommonName != cn || cert.Issuer.CommonName != cn {
			t.Errorf("%s: subject %v issuer %v", name, cert.Subject, cert.Issuer)
		}
		if days := cert.NotAfter.Sub(time.Now()).Hours() / 24; days < 3649 || days > 3651 {
			t.Errorf("%s: valid for %.0f days, want 3650", name, days)
		}
		kb, _ := pem.Decode([]byte(m.read("/etc/cloudground/certs/" + name + ".key")))
		if kb.Type != "PRIVATE KEY" {
			t.Errorf("%s key is %s, want PKCS#8", name, kb.Type)
		}
		if _, err := x509.ParsePKCS8PrivateKey(kb.Bytes); err != nil {
			t.Error(err)
		}
	}

	// Secrets are never rewritten, even when someone changed them.
	m.write("/etc/cloudground/agent.token", "kept", 0o640)
	m.mustApply(o)
	if m.read("/etc/cloudground/agent.token") != "kept" {
		t.Error("an existing agent token was rewritten")
	}
}

// panel.env keeps the other lines and carries exactly the version line.
func TestSetEnvLine(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", "CLOUDGROUND_PHP_VERSION=8.4\n"},
		{"A=1\n", "A=1\nCLOUDGROUND_PHP_VERSION=8.4\n"},
		{"A=1", "A=1\nCLOUDGROUND_PHP_VERSION=8.4\n"},
		{"CLOUDGROUND_PHP_VERSION=8.3\nB=2\n", "CLOUDGROUND_PHP_VERSION=8.4\nB=2\n"},
		{"A=1\nCLOUDGROUND_PHP_VERSION=8.3", "A=1\nCLOUDGROUND_PHP_VERSION=8.4"},
		{"CLOUDGROUND_PHP_VERSION=8.3\nCLOUDGROUND_PHP_VERSION=8.2\n", "CLOUDGROUND_PHP_VERSION=8.4\nCLOUDGROUND_PHP_VERSION=8.4\n"},
		{"# CLOUDGROUND_PHP_VERSION=8.3\n", "# CLOUDGROUND_PHP_VERSION=8.3\nCLOUDGROUND_PHP_VERSION=8.4\n"},
	} {
		if got := string(SetEnvLine([]byte(tc.in), "CLOUDGROUND_PHP_VERSION", "8.4")); got != tc.want {
			t.Errorf("SetEnvLine(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// doctor finds what changed after the install: content, mode, a missing
// secret, a service stopped, a default site back.
func TestDoctorFindsDrift(t *testing.T) {
	m := newMachine(t, "26.04")
	o := m.options(false)
	o.SkipPackages = true
	m.mustApply(o)
	d := o
	d.Doctor = true
	if n := needed(Plan(bg, Steps(m.env, d))); len(n) != 0 {
		t.Fatalf("no drift expected, got %v", n)
	}
	m.write(managed.Sysctl, "net.core.somaxconn = 128\n", 0o644)
	if err := os.Chmod(m.path(managed.Vhost), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(m.path(AgentToken)); err != nil {
		t.Fatal(err)
	}
	m.write("/etc/nginx/sites-enabled/default", "server {}\n", 0o644)
	m.sd.unit("cloudground-agent").active = false
	m.write(managed.PanelEnv, "CLOUDGROUND_PHP_VERSION=8.4\n", 0o640)
	got := needed(Plan(bg, Steps(m.env, d)))
	want := []string{"panel.env", "token.agent", "sysctl", "nginx.default-site", "nginx.vhost", "services"}
	if !slices.Equal(got, want) {
		t.Fatalf("drift = %v, want %v", got, want)
	}
	for _, s := range Steps(m.env, d) {
		if s.ID == "binaries" {
			t.Error("doctor has a binaries step")
		}
	}
	m.mustApply(o)
	if n := needed(Plan(bg, Steps(m.env, d))); len(n) != 0 {
		t.Fatalf("install did not put back %v", n)
	}
}

// The nginx.conf edit is made once, backed up, and undone when nginx
// rejects it.
func TestNginxTuning(t *testing.T) {
	m := newMachine(t, "24.04")
	o := m.options(false)
	o.SkipPackages = true
	stock := m.read(managed.NginxConf)
	m.mustApply(o)
	if got := m.read(managed.NginxConf + ".cloudground-pre"); got != stock {
		t.Error("the backup is not the distribution's file")
	}
	tuned := m.read(managed.NginxConf)
	m.mustApply(o)
	if m.read(managed.NginxConf) != tuned {
		t.Error("a second run edited nginx.conf again")
	}

	m = newMachine(t, "24.04")
	m.nginxOK = func() bool { return !strings.Contains(m.read(managed.NginxConf), TunedMarker) }
	st := stepByID(t, Steps(m.env, o), "nginx.tuning")
	if err := st.Apply(bg); err != nil {
		t.Fatalf("a rejected edit must be restored, not fail the install: %v", err)
	}
	if m.read(managed.NginxConf) != stock {
		t.Error("the rejected edit was not undone")
	}
	m.nginxOK = func() bool { return false }
	if err := st.Apply(bg); err == nil || !strings.Contains(err.Error(), "backup did not restore") {
		t.Errorf("nginx broken either way: err = %v", err)
	}
}

func stepByID(t *testing.T, steps []Step, id string) Step {
	t.Helper()
	for _, s := range steps {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("no step %s", id)
	return Step{}
}

// An upgrade restarts the panel when its binaries or units changed, and
// only then.
func TestUpgradeRestarts(t *testing.T) {
	m := newMachine(t, "24.04")
	o := m.options(false)
	o.SkipPackages = true
	o.From = m.build("v1")
	m.mustApply(o)

	p, _ := m.env.RunPreflight(bg, false)
	if !p.Upgrade {
		t.Fatal("token present and agent active: not seen as an upgrade")
	}
	if len(p.Problems) != 0 {
		t.Fatalf("an upgrade must not complain about its own ports: %v", p.Problems)
	}
	o.Upgrade = true

	m.sd.calls = nil
	m.mustApply(o)
	if len(m.sd.calls) != 0 {
		t.Errorf("an upgrade with nothing new ran %v", m.sd.calls)
	}

	o.From = m.build("v2")
	if n := needed(Plan(bg, Steps(m.env, o))); !slices.Equal(n, []string{"binaries", "services"}) {
		t.Errorf("plan of an upgrade = %v, want binaries and services", n)
	}
	m.mustApply(o)
	for _, c := range []string{"stop cloudground-api", "restart cloudground-api.socket", "restart cloudground-agent cloudground-api"} {
		if !m.sd.did(c) {
			t.Errorf("upgrade: systemctl %s not run (%v)", c, m.sd.calls)
		}
	}
	if m.sd.did("reload nginx") {
		t.Errorf("new binaries alone reloaded nginx: %v", m.sd.calls)
	}
}

// Redis: not installed any more; on an upgraded server it stays installed
// and is kept off. The distribution's PHP-FPM is kept off too.
func TestRedisAndDistroFPMStayOff(t *testing.T) {
	if slices.ContainsFunc(MainPackages("8.4"), func(p string) bool { return strings.Contains(p, "redis") }) {
		t.Fatal("the package list still installs Redis")
	}
	m := newMachine(t, "24.04")
	o := m.options(false)
	o.SkipPackages = true
	m.sd.units["redis-server"] = &unitState{exists: true, enabled: true, active: true}
	m.sd.units["php8.4-fpm"] = &unitState{exists: true, enabled: true}
	n := needed(Plan(bg, Steps(m.env, o)))
	if !slices.Contains(n, "redis.off") || !slices.Contains(n, "php.distro-fpm-off") {
		t.Fatalf("plan = %v", n)
	}
	m.mustApply(o)
	if !m.sd.did("disable --now redis-server.service") || !m.sd.did("disable --now php8.4-fpm.service") {
		t.Errorf("calls %v", m.sd.calls)
	}
	if !m.sd.units["redis-server"].exists {
		t.Error("redis-server was removed from an upgraded server")
	}

	// A server that never had Redis has nothing to do.
	m = newMachine(t, "24.04")
	if needs, why := stepByID(t, Steps(m.env, o), "redis.off").Check(bg); needs {
		t.Errorf("redis.off needed without Redis: %s", why)
	}
}

// A pinned download that does not hash to its pin is refused, and nothing
// is installed.
func TestPinnedDownloadRefusesAnotherFile(t *testing.T) {
	m := newMachine(t, "24.04")
	m.pins()
	m.net.files[pinWPCLI.URL] = []byte("<?php // tampered\n")
	o := m.options(false)
	err := stepByID(t, Steps(m.env, o), "tools.wp-cli").Apply(bg)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v", err)
	}
	if m.exists("/usr/local/bin/wp") {
		t.Error("a tampered wp-cli was installed")
	}
	// The real pins are the ones of SPEC §3.
	if WPCLISHA256 != "ce34ddd838f7351d6759068d09793f26755463b4a4610a5a5c0a97b68220d85c" ||
		ComposerSHA256 != "7a2d379d5b8ffdaa028580ef26494c36d2feef4b178d3dd1473a4dbc5e17c8d6" ||
		ResticSHA256 != "f415415624dcc452f2a02b8c33641791a8c6d6d3b65bbb3543fcf9a25151585c" || ResticVersion != "0.19.1" {
		t.Error("a pin changed: update SPEC.md §3 with where the new hash was checked")
	}
}

// ufw: the five rules when it is active, nothing when it is not.
func TestFirewall(t *testing.T) {
	m := newMachine(t, "24.04")
	o := m.options(false)
	o.SkipPackages = true
	m.cmd.tools["ufw"] = true
	allowed := map[string]bool{"22/tcp": true}
	m.cmd.handle = func(line string) (string, error, bool) {
		switch {
		case line == "ufw status":
			out := "Status: active\n\nTo                         Action      From\n--                         ------      ----\n"
			for r := range allowed {
				out += r + "                     ALLOW       Anywhere\n" + r + " (v6)                ALLOW       Anywhere (v6)\n"
			}
			return out, nil, true
		case strings.HasPrefix(line, "ufw allow "):
			allowed[strings.TrimPrefix(line, "ufw allow ")] = true
			return "Rule added", nil, true
		}
		return m.answer(line)
	}
	st := stepByID(t, Steps(m.env, o), "firewall")
	needs, why := st.Check(bg)
	if !needs || why != "not allowed: 80/tcp 443/tcp 8443/tcp 443/udp" {
		t.Fatalf("check = %v %q", needs, why)
	}
	if err := st.Apply(bg); err != nil {
		t.Fatal(err)
	}
	if needs, why := st.Check(bg); needs {
		t.Errorf("after apply: %s", why)
	}
	m.cmd.tools["ufw"] = false
	if needs, _ := st.Check(bg); needs {
		t.Error("no ufw: nothing to do")
	}
}

// With ufw active, SSH is opened on the ports sshd really listens on, not
// on 22 when sshd is elsewhere; 22 when sshd's configuration cannot be read.
func TestFirewallActiveUsesSSHDPorts(t *testing.T) {
	m := newMachine(t, "24.04")
	o := m.options(false)
	o.SkipPackages = true
	m.cmd.tools["ufw"] = true
	sshd := "port 2222\nport 2200\nlistenaddress 0.0.0.0:2222\n"
	var sshdErr error
	var added []string
	m.cmd.handle = func(line string) (string, error, bool) {
		switch {
		case line == "ufw status":
			return "Status: active\n", nil, true
		case line == "sshd -T":
			return sshd, sshdErr, true
		case strings.HasPrefix(line, "ufw allow "):
			added = append(added, strings.TrimPrefix(line, "ufw allow "))
			return "Rule added", nil, true
		}
		return m.answer(line)
	}
	st := stepByID(t, Steps(m.env, o), "firewall")
	if needs, why := st.Check(bg); !needs || why != "not allowed: 2222/tcp 2200/tcp 80/tcp 443/tcp 8443/tcp 443/udp" {
		t.Fatalf("check = %v %q", needs, why)
	}
	if err := st.Apply(bg); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(added, " "), "2222/tcp 2200/tcp 80/tcp 443/tcp 8443/tcp 443/udp"; got != want {
		t.Errorf("allowed %q, want %q", got, want)
	}
	added, sshdErr = nil, errors.New("exit status 255")
	if err := st.Apply(bg); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(added, " "), "22/tcp 80/tcp 443/tcp 8443/tcp 443/udp"; got != want {
		t.Errorf("sshd -T failing: allowed %q, want %q", got, want)
	}
}

// Preflight reports every problem together.
func TestPreflight(t *testing.T) {
	m := newMachine(t, "24.04")
	m.write("/etc/os-release", "PRETTY_NAME=\"Debian GNU/Linux 12\"\nID=debian\nVERSION_ID=\"12\"\n", 0o644)
	m.write("/proc/meminfo", "MemTotal:  500000 kB\n", 0o444)
	m.env.Arch = "aarch64"
	m.env.FreeMB = func(string) (int64, error) { return 2000, nil }
	m.listening[80] = true
	p, err := m.env.RunPreflight(bg, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"Ubuntu 24.04 or 26.04 LTS required (found Debian GNU/Linux 12)",
		"amd64 required (found aarch64)",
		"at least 1 GB of RAM required (found 488 MB)",
		"at least 10 GB free disk required (found 2000 MB)",
		"port 80 is already in use (unknown) — CloudGround must own the web ports",
	}
	if !slices.Equal(p.Problems, want) {
		t.Errorf("problems:\n%q\nwant\n%q", p.Problems, want)
	}
	m.env.EUID = 1000
	if _, err := m.env.RunPreflight(bg, false); err != ErrNotRoot {
		t.Errorf("not root: err = %v", err)
	}
	// In a container only the distribution counts.
	p, err = m.env.RunPreflight(bg, true)
	if err != nil || len(p.Problems) != 1 {
		t.Errorf("container preflight = %v, %v", p.Problems, err)
	}
}

// In a container the systemd, kernel and firewall work is skipped and the
// files are still written (SPEC §1).
func TestContainerRun(t *testing.T) {
	m := newMachine(t, "26.04")
	m.sd.running = false
	o := m.options(true)
	o.SkipPackages, o.Container = true, true
	o.From = m.build("v1")
	m.mustApply(o)
	if len(m.sd.calls) != 0 {
		t.Errorf("systemctl run in a container: %v", m.sd.calls)
	}
	if m.cmd.ran("sysctl") != 0 {
		t.Error("sysctl -p run in a container")
	}
	for _, f := range managed.Files {
		if !m.exists(f.Path) {
			t.Errorf("%s not written", f.Path)
		}
	}
	res := Plan(bg, Steps(m.env, o))
	if n := needed(res); len(n) != 0 {
		t.Fatalf("second plan in a container: %v", n)
	}
	for _, r := range res {
		if r.ID == "services" && r.Why != whyNoSystemd {
			t.Errorf("services: %q", r.Why)
		}
	}
}

// A failing nginx -t after the vhost is written stops the install there.
func TestVhostRejectedStopsTheInstall(t *testing.T) {
	m := newMachine(t, "24.04")
	o := m.options(false)
	o.SkipPackages = true
	m.nginxOK = func() bool { return !m.exists(managed.Vhost) }
	err := Apply(bg, m.env, Steps(m.env, o), nil)
	if err == nil || !strings.HasPrefix(err.Error(), "nginx.vhost: nginx -t") {
		t.Fatalf("err = %v", err)
	}
	if m.sd.did("daemon-reload") {
		t.Error("the install went on after nginx rejected the vhost")
	}
}

// The panel must be listening on 443 at the end, or the install fails.
func TestServicesWaitFor443(t *testing.T) {
	m := newMachine(t, "24.04")
	o := m.options(false)
	o.SkipPackages = true
	slept := 0
	m.env.Sleep = func(time.Duration) { slept++ }
	err := Apply(bg, m.env, Steps(m.env, o), nil)
	if err == nil || !strings.Contains(err.Error(), "not listening on 443") || slept != 10 {
		t.Fatalf("err = %v after %d waits", err, slept)
	}
}

// Every managed file is rendered by a step, from a template that exists.
func TestManagedListIsTheStepsList(t *testing.T) {
	m := newMachine(t, "24.04")
	o := m.options(false)
	o.SkipPackages = true
	m.mustApply(o)
	for _, f := range managed.Files {
		if f.Kind == managed.Rendered {
			want, err := Render(f, Vars{})
			if err != nil {
				t.Fatal(err)
			}
			if got := m.read(f.Path); got != string(want) {
				t.Errorf("%s differs from its template", f.Path)
			}
		}
	}
	// And nothing else is written under /etc but the managed files, the
	// nginx.conf backup and the secrets.
	allowed := map[string]bool{managed.NginxConf + ".cloudground-pre": true, AgentToken: true, "/etc/os-release": true}
	for _, c := range certs {
		allowed[filepath.Join(certDir, c.name+".pem")] = true
		allowed[filepath.Join(certDir, c.name+".key")] = true
	}
	_ = filepath.Walk(m.path("/etc"), func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		rel := strings.TrimPrefix(p, m.root)
		if !managed.Has(rel) && !allowed[rel] && !strings.HasPrefix(rel, "/etc/apt/") {
			t.Errorf("%s written but not in internal/install/managed", rel)
		}
		return nil
	})
}

// Re-converging applies what it rewrote: a changed vhost or nginx.conf is
// followed by a reload of nginx, a changed panel.env or agent token by a
// restart of the agent and the API — and nothing else.
func TestReconvergeAppliesWhatItRewrote(t *testing.T) {
	m := newMachine(t, "24.04")
	o := m.options(false)
	o.SkipPackages = true
	m.mustApply(o)

	m.sd.calls = nil
	m.write(managed.Vhost, "server {}\n", 0o644)
	n := needed(Plan(bg, Steps(m.env, o)))
	if !slices.Equal(n, []string{"nginx.vhost", "services"}) {
		t.Fatalf("plan after a vhost change = %v", n)
	}
	m.mustApply(o)
	if !slices.Equal(m.sd.calls, []string{"enable --now cloudground-api.socket cloudground-agent cloudground-api nginx", "reload nginx"}) {
		t.Errorf("after a vhost change: %v", m.sd.calls)
	}

	m.sd.calls = nil
	m.write(managed.PanelEnv, "CLOUDGROUND_PHP_VERSION=8.3\n", 0o640)
	m.mustApply(o)
	if !m.sd.did("restart cloudground-agent cloudground-api") || m.sd.did("reload nginx") {
		t.Errorf("after a panel.env change: %v", m.sd.calls)
	}

	// A panel that was not running starts on the new files: no restart.
	m.sd.calls = nil
	m.sd.unit("cloudground-agent").active = false
	m.sd.unit("cloudground-api").active = false
	m.write(managed.PanelEnv, "CLOUDGROUND_PHP_VERSION=8.3\n", 0o640)
	m.mustApply(o)
	if slices.ContainsFunc(m.sd.calls, func(c string) bool { return strings.HasPrefix(c, "restart") }) {
		t.Errorf("a stopped panel was restarted: %v", m.sd.calls)
	}
}

// The kernel settings are loaded when the file changes and when the running
// kernel's values differ from it; doctor sees the live values.
func TestSysctlLiveValues(t *testing.T) {
	m := newMachine(t, "24.04")
	o := m.options(false)
	o.SkipPackages = true
	live := map[string]string{"net/core/somaxconn": "4096", "net/ipv4/ip_local_port_range": "32768\t60999"}
	for k, v := range live {
		m.write("/proc/sys/"+k, v+"\n", 0o644)
	}
	handle := m.cmd.handle
	m.cmd.handle = func(line string) (string, error, bool) {
		if line == "sysctl -p "+managed.Sysctl {
			m.write("/proc/sys/net/core/somaxconn", "65535\n", 0o644)
			m.write("/proc/sys/net/ipv4/ip_local_port_range", "1024\t65535\n", 0o644)
			return "", nil, true
		}
		return handle(line)
	}
	m.mustApply(o)
	if needs, why := stepByID(t, Steps(m.env, o), "sysctl").Check(bg); needs {
		t.Fatalf("after the install: %s", why)
	}
	// Something set a value back at run time; the file is untouched.
	m.write("/proc/sys/net/core/somaxconn", "4096\n", 0o644)
	needs, why := stepByID(t, Steps(m.env, Options{OS: o.OS, SkipPackages: true, Doctor: true}), "sysctl").Check(bg)
	if !needs || !strings.Contains(why, "net.core.somaxconn is 4096, want 65535") {
		t.Fatalf("doctor: %v %q", needs, why)
	}
	before := m.cmd.ran("sysctl -p")
	m.mustApply(o)
	if m.cmd.ran("sysctl -p") != before+1 || m.read("/proc/sys/net/core/somaxconn") != "65535\n" {
		t.Error("live values not loaded again")
	}
}

// A directory the installer manages is never chmod-ed through a symlink.
func TestDirsRefuseASymlink(t *testing.T) {
	m := newMachine(t, "24.04")
	o := m.options(false)
	o.SkipPackages = true
	target := m.path("/elsewhere")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(m.path("/var/lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, m.path("/var/lib/cloudground-agent")); err != nil {
		t.Fatal(err)
	}
	err := Apply(bg, m.env, Steps(m.env, o), nil)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err = %v", err)
	}
	if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o755 {
		t.Errorf("the link's target was chmod-ed to %04o", fi.Mode().Perm())
	}
}

// Turning off the distribution's FPM or Redis is best effort, as before.
func TestDisableFailureIsAWarning(t *testing.T) {
	m := newMachine(t, "24.04")
	o := m.options(false)
	m.sd.units["redis-server"] = &unitState{exists: true, enabled: true, active: true}
	sd := &failingSd{fakeSd: m.sd}
	m.env.Sd = sd
	if err := stepByID(t, Steps(m.env, o), "redis.off").Apply(bg); err != nil {
		t.Errorf("a failed disable stopped the install: %v", err)
	}
}

type failingSd struct{ *fakeSd }

func (s *failingSd) Do(_ context.Context, args ...string) error {
	return errors.New("unit busy")
}

// A package on hold is installed; only missing ones are installed, nothing
// is upgraded (that is unattended-upgrades' job).
func TestPackagesOnHoldAndMissingOnly(t *testing.T) {
	c := &fakeCmd{handle: func(line string) (string, error, bool) {
		switch line {
		case "dpkg-query -W -f=${Status} nginx":
			return "hold ok installed", nil, true
		case "dpkg-query -W -f=${Status} git":
			return "install ok installed", nil, true
		case "dpkg-query -W -f=${Status} unzip":
			return "deinstall ok config-files", nil, true
		}
		return "", errors.New("no packages found"), true
	}}
	a := &apt{c: c}
	missing, err := a.Missing(bg, []string{"nginx", "git", "unzip", "certbot"})
	if err != nil || !slices.Equal(missing, []string{"unzip", "certbot"}) {
		t.Fatalf("missing = %v, %v", missing, err)
	}
	m := newMachine(t, "24.04")
	o := m.options(false)
	for _, p := range MainPackages("8.4")[1:] {
		m.pkg.installed[p] = true
	}
	for _, p := range BasePackages {
		m.pkg.installed[p] = true
	}
	m.pkg.installed["nginx"] = false
	if err := stepByID(t, Steps(m.env, o), "packages.main").Apply(bg); err != nil {
		t.Fatal(err)
	}
	if len(m.pkg.installs) != 1 || !slices.Equal(m.pkg.installs[0], []string{"nginx"}) {
		t.Errorf("installs = %v, want only the missing nginx", m.pkg.installs)
	}
}

// The setup link is a credential: its journal read never reaches the log.
func TestSetupURLIsNotLogged(t *testing.T) {
	var log strings.Builder
	c := &osCommands{log: &log}
	out, err := c.Quiet(bg, "sh", "-c", "echo s3cr3t-link | rev")
	if err != nil || strings.TrimSpace(out) != "knil-t3rc3s" {
		t.Fatalf("%q %v", out, err)
	}
	if strings.Contains(log.String(), "knil-t3rc3s") || !strings.Contains(log.String(), "output not logged") {
		t.Errorf("log: %q", log.String())
	}
	m := newMachine(t, "24.04")
	m.net.files["https://api.ipify.org"] = []byte("203.0.113.7\n")
	m.cmd.handle = func(line string) (string, error, bool) {
		if strings.HasPrefix(line, "journalctl") {
			return `{"msg":"setup pending","url":"https://<server-ip>:8443/setup/tok"}`, nil, true
		}
		return "", nil, false
	}
	if got := SetupURL(bg, m.env); got != "https://203.0.113.7:8443/setup/tok" {
		t.Errorf("setup URL = %q", got)
	}
}

// An upgrade of a set-up panel finds no setup link in the journal: the end
// of the run must not print the first-install text.
func TestFinishedOnAnUpgrade(t *testing.T) {
	var out strings.Builder
	(&UI{Out: &out}).Finished("https://203.0.113.7:8443", "/var/log/cloudground-install.log", 0, nil)
	if strings.Contains(out.String(), "works once") || !strings.Contains(out.String(), "already set up") {
		t.Fatalf("upgrade end text: %q", out.String())
	}
	out.Reset()
	(&UI{Out: &out}).Finished("https://203.0.113.7:8443/setup/tok", "/var/log/cloudground-install.log", 0, nil)
	if !strings.Contains(out.String(), "works once") {
		t.Fatalf("first install end text: %q", out.String())
	}
}

// H-3-2: a token file with a looser mode is put back to 0640 on the next
// run, its content kept; one that is not a regular file is not followed.
func TestAgentTokenModeIsCorrected(t *testing.T) {
	m := newMachine(t, "26.04")
	o := m.options(false)
	o.SkipPackages = true
	m.mustApply(o)
	before := m.read(AgentToken)
	if err := os.Chmod(m.path(AgentToken), 0o644); err != nil {
		t.Fatal(err)
	}
	d := o
	d.Doctor = true
	if got := needed(Plan(bg, Steps(m.env, d))); !slices.Contains(got, "token.agent") {
		t.Fatalf("a 0644 token is not drift: %v", got)
	}
	m.mustApply(o)
	fi, err := os.Stat(m.path(AgentToken))
	if err != nil || fi.Mode().Perm() != 0o640 {
		t.Fatalf("token mode after the run: %v %v", fi.Mode(), err)
	}
	if m.read(AgentToken) != before {
		t.Fatal("the token was rewritten")
	}
	if err := os.Remove(m.path(AgentToken)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/shadow", m.path(AgentToken)); err != nil {
		t.Fatal(err)
	}
	var tokenStep Step
	for _, s := range Steps(m.env, o) {
		if s.ID == "token.agent" {
			tokenStep = s
		}
	}
	if err := tokenStep.Apply(bg); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("a symlinked token: %v", err)
	}
}
