package install

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/install/managed"
	"github.com/cloudground-it/cloudground/internal/state"
)

// installed is a fake server after `cgctl install` and a few months of use:
// one site (id 1, a.test) with its user, tree, vhost, cache zone, PHP
// master unit and drop-in, cron file, database, a shell login and its mask;
// the panel's certificate lineage, its state with a backup repository, and
// MariaDB's tuning file.
func installed(t *testing.T) *machine {
	t.Helper()
	m := newMachine(t, "26.04")
	m.pins()
	os.Remove(m.path("/etc/nginx/sites-enabled/default"))
	m.write("/etc/nginx/sites-available/default", "server { listen 80 default_server; }\n", 0o644)
	if err := os.Symlink("/etc/nginx/sites-available/default", m.path("/etc/nginx/sites-enabled/default")); err != nil {
		t.Fatal(err)
	}
	o := m.options(false)
	o.From = m.build("v1")
	m.mustApply(o)

	m.acct.users["cg-site-1"] = [2]int{1001, 1001}
	m.acct.groups["cg-site-1"] = 1001
	m.acct.users["cg-shell-1"] = [2]int{1001, 1001}
	m.acct.users["cg-fn-3"] = [2]int{1003, 1003}
	m.acct.groups["cg-fn-3"] = 1003
	m.acct.groups["cg-shell"] = 990
	m.acct.groups["cg-sftp-keys"] = 991
	m.write("/etc/passwd", "root:x:0:0::/root:/bin/bash\ncloudground:x:901:901::/:/usr/sbin/nologin\n"+
		"cg-site-1:x:1001:1001::/srv/sites/a.test:/usr/sbin/nologin\ncg-shell-1:x:1001:1001::/srv/sites/a.test:/usr/local/bin/cg-site-shell\n"+
		"cg-fn-3:x:1003:1003::/nonexistent:/usr/sbin/nologin\n", 0o644)
	m.write("/var/lib/cloudground-agent/sites.json", `{"version":1,"sites":{"1":"a.test"}}`, 0o600)
	m.write("/var/lib/cloudground-agent/databases.json", `{"version":1,"databases":{"shop_db":1},"users":{"shop_user":{"site":1}}}`, 0o600)
	m.write("/var/lib/cloudground-signatures/cloudground.json", `{}`, 0o644)
	m.write("/etc/clamav/cloudground-freshclam.conf", "TestDatabases no\n", 0o644)
	m.write("/srv/sites/a.test/current/index.html", "the site's own page\n", 0o644)
	m.write("/etc/nginx/sites-enabled/a.test.conf", "# Managed by CloudGround — do not edit. Changes are detected as drift.\nserver {}\n", 0o644)
	m.write("/etc/nginx/sites-enabled/operator.conf", "server { server_name mine; }\n", 0o644)
	m.write("/etc/nginx/sites-enabled/cloudground-panel.conf", "# Managed by CloudGround: the panel\n", 0o644)
	m.write("/etc/nginx/conf.d/cloudground-cache-1.conf", "proxy_cache_path x;\n", 0o644)
	m.write("/etc/nginx/conf.d/cloudground-global.conf", "map x y {}\n", 0o644)
	m.write("/etc/systemd/system/cloudground-php-cg-site-1.service", "[Service]\n", 0o644)
	m.write("/etc/systemd/system/cloudground-php-cg-site-1.service.d/cloudground-slice.conf", "[Service]\nSlice=cg-site-1.slice\n", 0o644)
	m.write("/etc/systemd/system/cg-site-1.slice", "[Slice]\nMemoryMax=1024M\n", 0o644)
	m.write("/etc/systemd/system/user.slice", "[Slice]\n", 0o644) // not CloudGround's
	m.write("/etc/systemd/system/multi-user.target.wants/.keep", "", 0o644)
	os.Symlink("/etc/systemd/system/cloudground-php-cg-site-1.service", m.path("/etc/systemd/system/multi-user.target.wants/cloudground-php-cg-site-1.service"))
	os.Symlink("/etc/systemd/system/cloudground-agent.service", m.path("/etc/systemd/system/multi-user.target.wants/cloudground-agent.service"))
	os.Symlink("/dev/null", m.path("/etc/systemd/system/user@1001.service"))
	m.write("/etc/cron.d/cloudground-cg-site-1", "* * * * * cg-site-1 /usr/local/bin/cg-site-cron\n", 0o644)
	m.write("/etc/cloudground/certs/custom/a.test/privkey.pem", "bought key\n", 0o600)
	m.write("/etc/letsencrypt/renewal/cloudground-panel.conf", "x\n", 0o644)
	m.write("/etc/letsencrypt/renewal/a.test.conf", "x\n", 0o644)
	m.write("/etc/ssh/sshd_config.d/11-cloudground-shell.conf", "Match User cg-shell-*\n", 0o644)
	m.write("/etc/ssh/sshd_config.d/09-cloudground-sftp-keys.conf", "Match Group cg-sftp-keys\n", 0o644)
	m.write(mariadbTuning, "[mysqld]\nlocal_infile = 0\n", 0o644)
	m.write("/usr/local/bin/panel-agent.bak", "old\n", 0o755)
	m.write("/var/cache/cloudground/a.test/x", "cached\n", 0o644)
	m.write("/var/lib/cloudground-runtimes/node/22/bin/node", "binary\n", 0o755)
	m.write("/etc/cloudground-app/cg-site-1.env", "SECRET=1\n", 0o600)
	m.write("/var/log/cloudground/a.test/access.log", "GET /\n", 0o644)
	// A function (#143): its code, units, slice, nginx file, environment and lineage.
	m.write("/srv/functions/hello/releases/01JC0000000000000000000001/index.js", "exports.handler = () => 1;\n", 0o640)
	m.write("/etc/systemd/system/cloudground-fn-3.socket", "[Socket]\n", 0o644)
	m.write("/etc/systemd/system/cloudground-fn-3@.service", "[Service]\n", 0o644)
	m.write("/etc/systemd/system/cg-fn.slice", "[Slice]\n", 0o644)
	m.write("/etc/nginx/sites-enabled/cloudground-functions-internal.conf", "# Managed by CloudGround — do not edit. Changes are detected as drift.\n", 0o600)
	m.write("/etc/cloudground-fn/3.env", "A=1\n", 0o600)
	m.write("/etc/letsencrypt/renewal/cloudground-fn-3.conf", "x\n", 0o644)

	st, err := state.Open(m.path(stateDB))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdatePanelSettings(func(p *state.PanelSettings) error {
		p.BackupRepository, p.BackupS3AccessKey = "s3:https://s3.example.com/bucket/cg", "AKIDEXAMPLE"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSecret(state.SecretBackupPassword, "repo pass'word"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSecret(state.SecretBackupS3SecretKey, "s3-secret"); err != nil {
		t.Fatal(err)
	}
	st.Close()

	// userdel and groupdel take the account away, as on a server.
	m.cmd.handle = func(line string) (string, error, bool) {
		f := strings.Fields(line)
		switch {
		case len(f) >= 2 && f[0] == "userdel":
			delete(m.acct.users, f[len(f)-1])
			return "", nil, true
		case len(f) >= 4 && f[0] == "certbot" && f[1] == "delete":
			os.Remove(m.path("/etc/letsencrypt/renewal/" + f[3] + ".conf"))
			return "", nil, true
		case len(f) == 2 && f[0] == "groupdel":
			delete(m.acct.groups, f[1])
			return "", nil, true
		}
		return m.answer(line)
	}
	return m
}

func (m *machine) uninstall(o UninstallOptions) error {
	m.t.Helper()
	inv := m.env.TakeInventory(bg)
	return RunUninstall(bg, m.env, UninstallActions(m.env, inv, o), &bytes.Buffer{})
}

func (m *machine) gone(p string) bool {
	_, err := os.Lstat(m.path(p))
	return errors.Is(err, os.ErrNotExist)
}

// Every file of CloudGround's that the default tier removes (SPEC §9.2).
var removedByDefault = []string{
	"/usr/local/bin/panel-api", "/usr/local/bin/panel-agent", "/usr/local/bin/cgctl", "/usr/local/bin/panel-agent.bak",
	managed.AgentUnit, managed.APIUnit, managed.SocketUnit,
	"/etc/systemd/system/cloudground-php-cg-site-1.service", "/etc/systemd/system/cloudground-php-cg-site-1.service.d",
	"/etc/systemd/system/cg-site-1.slice",
	"/etc/systemd/system/multi-user.target.wants/cloudground-php-cg-site-1.service",
	"/etc/systemd/system/multi-user.target.wants/cloudground-agent.service",
	"/etc/systemd/system/user@1001.service",
	managed.Sysctl, managed.Vhost, managed.SSHD, "/etc/ssh/sshd_config.d/11-cloudground-shell.conf",
	"/etc/ssh/sshd_config.d/09-cloudground-sftp-keys.conf",
	managed.Logrotate, managed.Needrestart, managed.RenewHook, nginxPre,
	"/etc/nginx/sites-enabled/a.test.conf", "/etc/nginx/sites-enabled/cloudground-panel.conf",
	"/etc/nginx/conf.d/cloudground-cache-1.conf", "/etc/nginx/conf.d/cloudground-global.conf",
	"/etc/cron.d/cloudground-cg-site-1",
	"/etc/systemd/system/cloudground-fn-3.socket", "/etc/systemd/system/cloudground-fn-3@.service", "/etc/systemd/system/cg-fn.slice",
	"/etc/nginx/sites-enabled/cloudground-functions-internal.conf", "/etc/cloudground-fn",
	"/etc/cloudground", "/etc/cloudground-php", "/var/lib/cloudground", "/var/lib/cloudground-agent",
	"/var/lib/cloudground-signatures", "/var/lib/cloudground-runtimes", "/etc/cloudground-app", "/etc/clamav/cloudground-freshclam.conf",
	"/var/cache/cloudground", "/var/log/cloudground", "/run/cloudground", "/var/www/cloudground-acme",
}

// The default tier takes CloudGround away and keeps the site: its files,
// its user (locked), its databases, MariaDB's tuning, the backups; the
// state and the backup credentials are copied out first; nginx.conf is the
// distribution's again, and its default site is back.
func TestUninstallKeepsTheSites(t *testing.T) {
	m := installed(t)
	stock, _ := os.ReadFile(filepath.Join("testdata/golden/26.04/nginx.conf.stock"))
	if err := m.uninstall(UninstallOptions{KeptDir: DefaultKeptDir}); err != nil {
		t.Fatal(err)
	}
	for _, p := range removedByDefault {
		if !m.gone(p) {
			t.Errorf("%s is still there", p)
		}
	}
	for _, p := range []string{"/srv/sites/a.test/current/index.html", "/srv/functions/hello/releases/01JC0000000000000000000001/index.js", mariadbTuning, "/etc/nginx/sites-enabled/operator.conf", "/etc/systemd/system/user.slice"} {
		if m.gone(p) {
			t.Errorf("%s was removed", p)
		}
	}
	if got := m.read(managed.NginxConf); got != string(stock) {
		t.Errorf("nginx.conf is not the distribution's again:\n%s", got)
	}
	if l, err := os.Readlink(m.path("/etc/nginx/sites-enabled/default")); err != nil || l != "/etc/nginx/sites-available/default" {
		t.Errorf("the default site was not put back: %q %v", l, err)
	}
	if _, _, ok := m.acct.User("cg-site-1"); !ok {
		t.Error("the site's user was removed")
	}
	if m.cmd.ran("usermod --lock --expiredate 1 --shell /usr/sbin/nologin cg-site-1") != 1 {
		t.Error("the site's user was not locked")
	}
	if m.cmd.ran("usermod --lock --expiredate 1 --shell /usr/sbin/nologin cg-fn-3") != 1 {
		t.Error("the function's user was not locked")
	}
	if m.cmd.ran("certbot delete --cert-name cloudground-fn-3") != 1 {
		t.Error("the function's certificate was not deleted")
	}
	for _, u := range []string{"cloudground", "cg-shell-1"} {
		if _, _, ok := m.acct.User(u); ok {
			t.Errorf("user %s is still there", u)
		}
	}
	for _, g := range []string{"cloudground", "cg-sftp-off", "cg-sftp-keys", "cg-shell"} {
		if _, ok := m.acct.Group(g); ok {
			t.Errorf("group %s is still there", g)
		}
	}
	if m.cmd.ran("mariadb -e DROP") != 0 || m.cmd.ran("certbot delete --cert-name a.test") != 0 {
		t.Error("the default tier dropped a database or a site's certificate")
	}
	if m.cmd.ran("certbot delete --cert-name cloudground-panel") != 1 {
		t.Error("the panel's certificate was not deleted")
	}
	for _, call := range []string{"disable --now cloudground-agent.service", "disable --now cloudground-php-cg-site-1.service", "stop cg-site.slice",
		"stop cg-fn.slice", "disable --now cloudground-fn-3.socket", "daemon-reload"} {
		if !m.sd.did(call) {
			t.Errorf("systemctl %s did not run", call)
		}
	}

	// What was kept for the operator.
	env := m.read(DefaultKeptDir + "/backup.env")
	for _, want := range []string{"RESTIC_REPOSITORY='s3:https://s3.example.com/bucket/cg'", `RESTIC_PASSWORD='repo pass'\''word'`,
		"AWS_ACCESS_KEY_ID='AKIDEXAMPLE'", "AWS_SECRET_ACCESS_KEY='s3-secret'"} {
		if !strings.Contains(env, want) {
			t.Errorf("backup.env lacks %s:\n%s", want, env)
		}
	}
	for _, p := range []string{"/state/state.db", "/state/secret.key", "/backup.env", "/custom-certs/a.test/privkey.pem"} {
		fi, err := os.Stat(m.path(DefaultKeptDir + p))
		if err != nil {
			t.Errorf("kept %s: %v", p, err)
		} else if fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("kept %s is mode %v", p, fi.Mode().Perm())
		}
	}
	if repos, creds := m.env.KeptSummary(DefaultKeptDir); !creds || !slices.Contains(repos, "s3:https://s3.example.com/bucket/cg") {
		t.Errorf("kept summary: %v %v", repos, creds)
	}

	// Run again: nothing of CloudGround's is left to remove.
	if a := UninstallActions(m.env, m.env.TakeInventory(bg), UninstallOptions{}); a != nil {
		var ids []string
		for _, x := range a {
			ids = append(ids, x.ID+": "+x.What)
		}
		t.Errorf("a second uninstall still has work: %v", ids)
	}
}

// --remove-sites takes the site too: its user and group, its tree, its
// databases and accounts (the registry's and the panel's names), its
// certificate, MariaDB's tuning. The backups' credentials are still kept.
func TestUninstallRemoveSites(t *testing.T) {
	m := installed(t)
	m.cmd.tools["mariadb"] = true
	inner := m.cmd.handle
	m.cmd.handle = func(line string) (string, error, bool) {
		switch {
		case strings.Contains(line, "information_schema.schemata"):
			return "mysql\nsite_1\nshop_db\ncgv_0123456789abcdef\nunrelated\n", nil, true
		case strings.Contains(line, "mysql.user"):
			return "root\nsite_1\nshop_user\ncgq_0123456789abcdef\n", nil, true
		}
		return inner(line)
	}
	if err := m.uninstall(UninstallOptions{RemoveSites: true, KeptDir: DefaultKeptDir}); err != nil {
		t.Fatal(err)
	}
	for _, p := range append(removedByDefault, "/srv/sites", "/srv/functions", mariadbTuning) {
		if !m.gone(p) {
			t.Errorf("%s is still there", p)
		}
	}
	if _, _, ok := m.acct.User("cg-site-1"); ok {
		t.Error("the site's user is still there")
	}
	if _, _, ok := m.acct.User("cg-fn-3"); ok {
		t.Error("the function's user is still there")
	}
	for _, stmt := range []string{"DROP DATABASE IF EXISTS `site_1`", "DROP DATABASE IF EXISTS `shop_db`", "DROP DATABASE IF EXISTS `cgv_0123456789abcdef`",
		"DROP USER IF EXISTS 'shop_user'@'localhost'", "DROP USER IF EXISTS 'site_1'@'localhost'", "DROP USER IF EXISTS 'cgq_0123456789abcdef'@'localhost'"} {
		if m.cmd.ran("mariadb -e "+stmt) != 1 {
			t.Errorf("not run: %s", stmt)
		}
	}
	if m.cmd.ran("mariadb -e DROP DATABASE IF EXISTS `unrelated`") != 0 || m.cmd.ran("mariadb -e DROP DATABASE IF EXISTS `mysql`") != 0 {
		t.Error("a database CloudGround did not make was dropped")
	}
	if m.cmd.ran("certbot delete --cert-name a.test --non-interactive") != 1 {
		t.Error("the site's certificate was not deleted")
	}
	if !strings.Contains(m.read(DefaultKeptDir+"/backup.env"), "RESTIC_PASSWORD=") {
		t.Error("the backup credentials were not kept")
	}
}

// The state is copied out before anything is removed: a copy that fails
// stops the run with the panel's state still in place.
func TestUninstallStopsWhenTheStateCannotBeKept(t *testing.T) {
	m := installed(t)
	os.Rename(m.path(stateDB), m.path(stateDB+".real"))
	os.Symlink(m.path(stateDB+".real"), m.path(stateDB)) // the API's user could plant this
	err := m.uninstall(UninstallOptions{KeptDir: DefaultKeptDir})
	if err == nil || !strings.Contains(err.Error(), "keep.state") {
		t.Fatalf("err = %v", err)
	}
	for _, p := range []string{"/var/lib/cloudground/state.db.real", "/usr/local/bin/panel-agent", managed.AgentUnit} {
		if m.gone(p) {
			t.Errorf("%s was removed although the state could not be kept", p)
		}
	}
}

// --purge-packages purges what the installer recorded installing, of the
// data plane only, and MariaDB only when the sites go too.
func TestUninstallPurgesOnlyWhatTheInstallerInstalled(t *testing.T) {
	m := installed(t)
	rec, err := m.env.ReadInstallRecord()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"nginx", "mariadb-server", "php8.5-fpm", "curl", "cron"} {
		if !slices.Contains(rec.Packages, p) {
			t.Fatalf("the install did not record %s: %v", p, rec.Packages)
		}
	}
	if !slices.Contains(rec.Tools, "/usr/local/bin/restic") || rec.NginxDefaultSite != "/etc/nginx/sites-available/default" {
		t.Fatalf("record %+v", rec)
	}
	m.pkg.installed = map[string]bool{}
	rec.Packages = []string{"nginx", "mariadb-server", "php8.5-fpm", "curl", "certbot"}
	if err := m.env.record(func(r *InstallRecord) { r.Packages = rec.Packages }); err != nil {
		t.Fatal(err)
	}
	// apt-get's orphans: one before the purge (the operator's), two more after.
	purged, prev := false, m.cmd.handle
	m.cmd.handle = func(line string) (string, error, bool) {
		switch {
		case strings.HasPrefix(line, "apt-get purge -y -qq nginx "):
			purged = true
		case line == "apt-get -s autoremove" && !purged:
			return "Remv pv [1.10]\n", nil, true
		case line == "apt-get -s autoremove":
			return "Remv pv [1.10]\nRemv nginx-common [1.28] [x:amd64 ]\nPurg php8.5-common [8.5]\nRemv libgd3:i386 [2.3]\nRemv php8.5-cli [8.5]\n", nil, true
		case line == "apt-mark showhold":
			return "php8.5-cli\n", nil, true
		}
		return prev(line)
	}
	m.write("/var/lib/letsencrypt/.keep", "", 0o644)
	os.Remove(m.path("/var/lib/letsencrypt/.keep")) // the empty directory certbot's sandbox needed
	if err := m.uninstall(UninstallOptions{PurgePackages: true, KeptDir: DefaultKeptDir}); err != nil {
		t.Fatal(err)
	}
	if m.cmd.ran("apt-get purge -y -qq nginx-common php8.5-common libgd3:i386") != 1 || m.cmd.ran("apt-get purge -y -qq pv") != 0 {
		t.Errorf("dependency purge: %v", m.cmd.calls)
	}
	if m.cmd.ran("apt-get purge -y -qq nginx php8.5-fpm") != 1 || m.cmd.ran("apt-get purge -y -qq nginx mariadb") != 0 {
		t.Errorf("purge calls: %v", m.cmd.calls)
	}
	if !m.gone("/var/lib/letsencrypt") {
		t.Error("certbot's empty directory is still there")
	}
	if !m.gone("/usr/local/bin/restic") {
		t.Error("the restic the installer downloaded is still there")
	}
}

// The debian.cnf mariadb-server's postinst wrote goes with MariaDB, alone in
// /etc/mysql; next to the operator's own configuration it stays.
func TestUninstallTakesDebianCnfWithMariaDB(t *testing.T) {
	for _, own := range []bool{false, true} {
		m := installed(t)
		if err := m.env.record(func(r *InstallRecord) { r.Packages = []string{"mariadb-server"} }); err != nil {
			t.Fatal(err)
		}
		m.cmd.tools["mariadb"] = true
		prev, dropped := m.cmd.handle, false
		m.cmd.handle = func(line string) (string, error, bool) {
			switch {
			case strings.Contains(line, "DROP DATABASE"):
				dropped = true
			case strings.Contains(line, "information_schema.schemata") && dropped:
				return "mysql\ninformation_schema\n", nil, true
			case strings.Contains(line, "information_schema.schemata"):
				return "mysql\nsite_1\nshop_db\n", nil, true
			case strings.Contains(line, "mysql.user"):
				return "root\nsite_1\nshop_user\n", nil, true
			}
			return prev(line)
		}
		m.write("/etc/mysql/debian.cnf", "[client]\n", 0o600)
		// Ubuntu's profile: the purge deletes the file, the kernel keeps it.
		m.write("/etc/apparmor.d/mariadbd", "abi <abi/4.0>,\nprofile mariadbd /usr/{,s}bin/mariadbd flags=(complain) {\n}\n", 0o644)
		m.write(apparmorFS+"/profiles", "nginx (enforce)\nmariadbd (complain)\n", 0o444)
		inner := m.cmd.handle
		m.cmd.handle = func(line string) (string, error, bool) {
			if line == "dpkg-query -L mariadb-server" {
				return "/.\n/etc/apparmor.d\n/etc/apparmor.d/mariadbd\n/usr/sbin/mariadbd\n", nil, true
			}
			if strings.HasPrefix(line, "apt-get purge") {
				os.Remove(m.path("/etc/apparmor.d/mariadbd"))
			}
			return inner(line)
		}
		if own {
			m.write("/etc/mysql/my.cnf", "[mysqld]\n", 0o644)
		}
		if err := m.uninstall(UninstallOptions{RemoveSites: true, PurgePackages: true, KeptDir: DefaultKeptDir}); err != nil {
			t.Fatal(err)
		}
		if m.cmd.ran("apt-get purge -y -qq mariadb-server") != 1 {
			t.Fatalf("purge calls: %v", m.cmd.calls)
		}
		if own && (m.gone("/etc/mysql/debian.cnf") || m.gone("/etc/mysql/my.cnf")) {
			t.Error("/etc/mysql went with the operator's my.cnf in it")
		}
		if !own && !m.gone("/etc/mysql") {
			t.Error("/etc/mysql with only debian.cnf in it is still there")
		}
		if b, err := os.ReadFile(m.path(apparmorFS + "/.remove")); err != nil || string(b) != "mariadbd" {
			t.Errorf("the mariadbd profile was not unloaded: %q %v", b, err)
		}
	}
}

// A purge that failed leaves the package, its file and so its profile.
func TestUninstallKeepsTheProfileOfAFailedPurge(t *testing.T) {
	m := installed(t)
	if err := m.env.record(func(r *InstallRecord) { r.Packages = []string{"nginx"} }); err != nil {
		t.Fatal(err)
	}
	m.write("/etc/apparmor.d/nginx", "profile \"nginx web\" /usr/sbin/nginx {\n}\n", 0o644)
	m.write(apparmorFS+"/profiles", "nginx web (enforce)\n", 0o444)
	inner := m.cmd.handle
	m.cmd.handle = func(line string) (string, error, bool) {
		switch {
		case line == "dpkg-query -L nginx":
			return "/etc/apparmor.d/nginx\n", nil, true
		case strings.HasPrefix(line, "apt-get purge"):
			return "E: Could not get lock /var/lib/dpkg/lock-frontend", errors.New("exit status 100"), true
		}
		return inner(line)
	}
	if err := m.uninstall(UninstallOptions{PurgePackages: true, KeptDir: DefaultKeptDir}); err == nil {
		t.Fatal("the failed purge was not reported")
	}
	if !m.gone(apparmorFS + "/.remove") {
		t.Error("the profile of a package still installed was unloaded")
	}
	if got := profileNames("profile \"nginx web\" /usr/sbin/nginx {"); len(got) != 1 || got[0] != "nginx web" {
		t.Errorf("quoted name read as %q", got)
	}
}

// MariaDB kept for a database CloudGround did not make keeps its debian.cnf.
func TestUninstallKeepsDebianCnfWithAKeptMariaDB(t *testing.T) {
	m := installed(t)
	if err := m.env.record(func(r *InstallRecord) { r.Packages = []string{"mariadb-server", "nginx"} }); err != nil {
		t.Fatal(err)
	}
	m.cmd.tools["mariadb"] = true
	prev := m.cmd.handle
	m.cmd.handle = func(line string) (string, error, bool) {
		switch {
		case strings.Contains(line, "information_schema.schemata"):
			return "mysql\noperators_db\n", nil, true
		case strings.Contains(line, "mysql.user"):
			return "root\n", nil, true
		}
		return prev(line)
	}
	m.write("/etc/mysql/debian.cnf", "[client]\n", 0o600)
	if err := m.uninstall(UninstallOptions{RemoveSites: true, PurgePackages: true, KeptDir: DefaultKeptDir}); err != nil {
		t.Fatal(err)
	}
	if m.cmd.ran("apt-get purge -y -qq nginx") != 1 || m.cmd.ran("apt-get purge -y -qq nginx mariadb") != 0 {
		t.Fatalf("purge calls: %v", m.cmd.calls)
	}
	if m.gone("/etc/mysql/debian.cnf") {
		t.Error("debian.cnf went with MariaDB kept")
	}
}

// A package that was on the server before the install is never recorded,
// so never purged.
func TestInstallRecordsOnlyWhatWasMissing(t *testing.T) {
	m := newMachine(t, "26.04")
	m.pins()
	m.pkg.installed["nginx"] = true
	m.write("/usr/local/bin/wp", "the operator's own wp\n", 0o755)
	o := m.options(false)
	o.From = m.build("v1")
	m.mustApply(o)
	rec, err := m.env.ReadInstallRecord()
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(rec.Packages, "nginx") || !slices.Contains(rec.Packages, "mariadb-server") {
		t.Errorf("packages recorded: %v", rec.Packages)
	}
	if slices.Contains(rec.Tools, "/usr/local/bin/wp") || !slices.Contains(rec.Tools, "/usr/local/bin/composer") {
		t.Errorf("tools recorded: %v", rec.Tools)
	}
	if rec.NginxDefaultSite != "" {
		t.Errorf("a regular default site was recorded as a link: %q", rec.NginxDefaultSite)
	}
}

// An nginx.conf the operator edited after the install keeps the edit: only
// the installer's lines go and worker_connections gets its old value.
func TestUntuneKeepsTheOperatorsEdits(t *testing.T) {
	m := installed(t)
	cur := m.read(managed.NginxConf)
	m.write(managed.NginxConf, cur+"# the operator's line\n", 0o644)
	if err := m.env.untuneNginx(); err != nil {
		t.Fatal(err)
	}
	got := m.read(managed.NginxConf)
	if strings.Contains(got, TunedMarker) || strings.Contains(got, "worker_rlimit_nofile") || strings.Contains(got, "worker_connections 4096;") {
		t.Errorf("the tuning is still there:\n%s", got)
	}
	if !strings.Contains(got, "# the operator's line") || !strings.Contains(got, "worker_connections 768;") {
		t.Errorf("the operator's edit or the original value is missing:\n%s", got)
	}
	if !m.gone(nginxPre) {
		t.Error("the saved original is still there")
	}
}

// --plan lists and changes nothing.
func TestUninstallPlanChangesNothing(t *testing.T) {
	m := installed(t)
	inv := m.env.TakeInventory(bg)
	var b bytes.Buffer
	PrintUninstallPlan(&b, UninstallActions(m.env, inv, UninstallOptions{RemoveSites: true}))
	for _, want := range []string{"keep.state", "site.user", "site.trees", "binaries", "nginx.conf"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("plan lacks %s:\n%s", want, b.String())
		}
	}
	if m.gone("/usr/local/bin/panel-agent") || m.gone("/srv/sites/a.test") {
		t.Error("--plan removed something")
	}
	_ = context.Background
}

// A server uninstalled, installed again and uninstalled again: what the
// first uninstall kept (the only way into an older backup repository) is
// set aside, not overwritten (SPEC §9.3).
func TestUninstallKeepsAnEarlierKeptState(t *testing.T) {
	m := installed(t)
	m.write(DefaultKeptDir+"/state/state.db", "the earlier panel's state\n", 0o600)
	m.write(DefaultKeptDir+"/backup.env", "RESTIC_PASSWORD='the earlier one'\n", 0o600)
	m.write(DefaultKeptDir+"/"+KeptReadme, "earlier\n", 0o600)
	if err := m.uninstall(UninstallOptions{KeptDir: DefaultKeptDir}); err != nil {
		t.Fatal(err)
	}
	earlier, _ := filepath.Glob(m.path(DefaultKeptDir + "/earlier-*"))
	if len(earlier) != 1 {
		t.Fatalf("earlier kept directories: %v", earlier)
	}
	for name, want := range map[string]string{"state/state.db": "the earlier panel's state\n", "backup.env": "RESTIC_PASSWORD='the earlier one'\n", KeptReadme: "earlier\n"} {
		if b, err := os.ReadFile(filepath.Join(earlier[0], name)); err != nil || string(b) != want {
			t.Errorf("earlier %s = %q, %v", name, b, err)
		}
	}
	if fi, err := os.Stat(earlier[0]); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("earlier directory: %v %v", fi, err)
	}
	if env := m.read(DefaultKeptDir + "/backup.env"); !strings.Contains(env, "repo pass") {
		t.Errorf("this run's backup.env:\n%s", env)
	}
}

// nginx served only CloudGround's sites: the uninstall stops and disables it,
// else its stock site holds port 80 and a reinstall refuses (#212, SPEC §9.2).
func TestUninstallStopsNginx(t *testing.T) {
	m := installed(t)
	m.cmd.tools["nginx"] = true
	inv := m.env.TakeInventory(bg)
	var plan bytes.Buffer
	PrintUninstallPlan(&plan, UninstallActions(m.env, inv, UninstallOptions{}))
	if !strings.Contains(plan.String(), "nginx.stop") {
		t.Errorf("the plan lacks nginx.stop:\n%s", plan.String())
	}
	if m.sd.did("disable --now nginx") {
		t.Error("--plan stopped nginx")
	}
	if err := m.uninstall(UninstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if !m.sd.did("disable --now nginx") {
		t.Error("nginx was not stopped and disabled")
	}
	if !m.env.StoppedNginx() {
		t.Error("the summary would not mention nginx")
	}
	// Run again: stopping nginx alone is not work to do.
	if a := UninstallActions(m.env, m.env.TakeInventory(bg), UninstallOptions{}); a != nil {
		t.Errorf("a second uninstall still has work: %d actions", len(a))
	}
}

// Without nginx on the machine there is nothing to stop.
func TestUninstallLeavesNoNginxAlone(t *testing.T) {
	m := installed(t)
	if err := m.uninstall(UninstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if m.sd.did("disable --now nginx") || m.env.StoppedNginx() {
		t.Error("nginx is not installed and was still stopped")
	}
}
