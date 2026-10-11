package sites

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cloudground-it/cloudground/connector"
	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/cron"
	"github.com/cloudground-it/cloudground/internal/agent/ops/sftp"
	"github.com/cloudground-it/cloudground/internal/agent/ops/wp"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/phpfpm"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// ---------------------------------------------------------------- create --

func TestCreateStaticSite(t *testing.T) {
	e := newEnv(t)
	s := e.site(3, "docs.example.com", state.SiteStatic)
	res := e.create(t, s)

	u, ok := e.accounts.User(s.SystemUser)
	if !ok || !u.Spec.System || u.Spec.Home != s.RootPath || u.Spec.Shell != "/usr/sbin/nologin" || u.Spec.CreateHome {
		t.Fatalf("site user %+v %v", u.Spec, ok)
	}
	if in, _ := e.accounts.IsMember(context.Background(), s.SystemUser, sftp.DenyGroup); !in {
		t.Error("a new site has SFTP on")
	}
	if in, _ := e.accounts.IsMember(context.Background(), "www-data", s.SystemUser); !in {
		t.Error("nginx is not in the site's group")
	}
	for _, p := range []string{"releases/initial/index.html", "shared/uploads", "logs", "tmp/sessions"} {
		if !exists(filepath.Join(s.RootPath, p)) {
			t.Errorf("missing %s", p)
		}
	}
	if l, err := os.Readlink(filepath.Join(s.RootPath, "current")); err != nil || l != "releases/initial" {
		t.Errorf("current -> %q %v", l, err)
	}
	if fi, _ := os.Stat(s.RootPath); fi.Mode().Perm() != 0o750 {
		t.Errorf("root mode %v", fi.Mode().Perm())
	}
	vhost, err := os.ReadFile(e.vhostPath(s))
	if err != nil || !strings.Contains(string(vhost), "server_name docs.example.com;") {
		t.Fatalf("vhost: %v", err)
	}
	if tests, reloads := e.nginx.Counts(); tests != 1 || reloads != 1 {
		t.Errorf("nginx tests %d reloads %d, want 1 and 1", tests, reloads)
	}
	if !hasFile(res.Files, e.vhostPath(s)) || res.SystemUser != s.SystemUser || res.RootPath != s.RootPath {
		t.Errorf("result %+v", res)
	}
	if exists(e.unitPath(s)) || exists(e.poolPath(s)) {
		t.Error("a static site got a PHP master")
	}
	if len(e.exec.Calls()) != 0 {
		t.Errorf("a static site ran commands: %v", e.exec.Calls())
	}
}

func hasFile(files []rpc.ManagedFile, path string) bool {
	for _, f := range files {
		if f.Path == path && f.SHA256 != "" {
			return true
		}
	}
	return false
}

// A php or laravel site serves a page that proves PHP runs, under the
// public root for laravel, until code is deployed.
func TestCreatePHPSitesServeAPlaceholder(t *testing.T) {
	e := newEnv(t)
	php := e.site(4, "app.example.com", state.SitePHP)
	e.create(t, php)
	laravel := e.site(5, "shop.example.com", state.SiteLaravel)
	laravel.Config.PublicRoot = "public"
	e.create(t, laravel)
	for _, p := range []string{filepath.Join(php.RootPath, "releases/initial/index.php"), filepath.Join(laravel.RootPath, "releases/initial/public/index.php")} {
		b, err := os.ReadFile(p)
		if err != nil || !strings.Contains(string(b), "<?= htmlspecialchars(PHP_VERSION") {
			t.Errorf("%s: %v\n%s", p, err, b)
		}
	}
}

// A PHP site's master is rendered, put in the site's slice, tested as the
// site user and started fresh.
func TestCreatePHPSiteStartsItsMasterInItsSlice(t *testing.T) {
	e := newEnv(t)
	s := e.site(4, "app.example.com", state.SitePHP)
	e.create(t, s)
	unit := phpfpm.UnitName(s.SystemUser)
	if got := e.systemd.SliceOf(unit); got != "cg-site-4.slice" {
		t.Fatalf("the master's slice is %q", got)
	}
	if u, _ := e.systemd.Unit(unit); u.ActiveState != "active" || u.UnitFileState != "enabled" {
		t.Fatalf("master %+v", u)
	}
	if !e.systemd.Did("restart", unit) {
		t.Error("a new master was not started fresh")
	}
	test := filepath.Join(e.Paths.PHPBinDir, "php-fpm8.4") + " -t --fpm-config " + e.poolPath(s)
	var ran bool
	for _, c := range e.exec.Calls() {
		line := c.Line()
		if c.Name != "systemd-run" || !strings.HasSuffix(line, "-- "+test) {
			continue
		}
		ran = true
		// The site's user, slice and sandbox, through systemd: never
		// runuser in the agent's own mount namespace.
		for _, want := range append([]string{"--uid=cg-site-4", "--gid=cg-site-4", "--slice=cg-site-4.slice", "--pipe", "--wait"}, phpfpm.Sandbox(s)...) {
			if !strings.Contains(line, want) {
				t.Errorf("the configuration test lacks %q: %s", want, line)
			}
		}
	}
	if !ran {
		t.Errorf("no sandboxed configuration test; ran %v", e.exec.Calls())
	}
	if fi, _ := os.Stat(e.poolPath(s)); fi.Mode().Perm() != 0o640 {
		t.Errorf("pool mode %v", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Join(e.Paths.LogRoot, s.Domain, "php-error.log")); fi == nil {
		t.Error("no PHP error log")
	}
}

func TestCreateWordPress(t *testing.T) {
	e := newEnv(t)
	s := e.site(6, "blog.example.com", state.SiteWooCommerce)
	// wp-cli writes wp-config.php when it creates the configuration.
	e.wpcli.onRun = func(inv host.Invocation) {
		if strings.Join(inv.Args, " ") == "config create --dbname=wp_6 --dbuser=wp_6 --prompt=dbpass --dbhost=localhost:3306 --skip-check" {
			_ = os.WriteFile(filepath.Join(inv.Path, "wp-config.php"), []byte("<?php // config"), 0o640)
		}
	}
	p := rpc.CreateSiteParams{Site: s, DBPassword: strings.Repeat("d", 20), AdminUser: "admin",
		AdminPassword: "admin-secret", AdminEmail: "ops@example.com", ConnectorToken: "token-secret"}
	if _, err := e.Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	for _, c := range e.wpcli.calls {
		if c.User != s.SystemUser || c.Slice != "cg-site-6.slice" {
			t.Fatalf("wp-cli ran as %s in %q", c.User, c.Slice)
		}
		for _, secret := range []string{"admin-secret", "token-secret", p.DBPassword} {
			if strings.Contains(strings.Join(c.Args, " "), secret) {
				t.Fatalf("a secret in argv: %v", c.Args)
			}
		}
	}
	want := []string{"core download", "config create", "config set CLOUDGROUND_SITE_TOKEN", "config set CLOUDGROUND_ENDPOINT",
		"config set DISABLE_WP_CRON", "core install", "rewrite structure", "plugin install woocommerce --activate",
		"rewrite flush", "config set CLOUDGROUND_DISABLE_CART_FRAGMENTS true --raw"}
	got := e.wpcli.lines()
	for i, w := range want {
		if i >= len(got) || !strings.HasPrefix(got[i], w) {
			t.Fatalf("wp-cli runs %q, want them to start %q", got, want)
		}
	}
	rel := filepath.Join(s.RootPath, "releases/initial")
	if l, _ := os.Readlink(filepath.Join(rel, "wp-config.php")); l != wp.ConfigTarget {
		t.Errorf("wp-config.php -> %q", l)
	}
	if fi, err := os.Stat(filepath.Join(s.RootPath, "shared/wp-config.php")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("shared config: %v", err)
	}
	if l, _ := os.Readlink(filepath.Join(rel, "wp-content/uploads")); l != wp.UploadsTarget {
		t.Errorf("uploads -> %q", l)
	}
	if b, _ := os.ReadFile(filepath.Join(rel, wp.ConnectorPlugin)); string(b) != connector.PHP {
		t.Error("no connector")
	}
	if !exists(filepath.Join(rel, wp.DropinLink)) {
		t.Error("no object cache")
	}
	if !e.dbs.has("wp_6") {
		t.Error("no database")
	}
	// WordPress's events are scheduled for the site (wp-cron is off): a
	// line of its cron.d file, run sandboxed by the runner.
	tab, _ := os.ReadFile(filepath.Join(e.Cron.(*cron.Ops).CronDir, "cloudground-cg-site-6"))
	if !strings.Contains(string(tab), "root ") || !strings.HasSuffix(strings.TrimSpace(string(tab)), " cg-site-6 wp") {
		t.Errorf("cron file %q", tab)
	}
}

// Anything the site would create that already exists is a refusal before
// any change: the undo of a failure would remove it by name.
func TestCreateRefusesWhatAlreadyExists(t *testing.T) {
	ctx := context.Background()
	for name, setup := range map[string]func(e *env, s state.Site){
		"user":     func(e *env, s state.Site) { _ = e.accounts.CreateUser(ctx, host.UserSpec{Name: s.SystemUser}) },
		"root":     func(e *env, s state.Site) { _ = os.MkdirAll(s.RootPath, 0o750) },
		"database": func(e *env, s state.Site) { e.dbs.dbs["wp_7"] = "someone" },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			s := e.site(7, "taken.example.com", state.SiteWordPress)
			setup(e, s)
			before := len(e.accounts.Ops())
			_, err := e.Create(ctx, rpc.CreateSiteParams{Site: s, DBPassword: strings.Repeat("p", 20)})
			if e := agentrpc.AsError(err); e == nil || e.Code != agentrpc.CodeConflict {
				t.Fatalf("err %v", err)
			}
			if len(e.accounts.Ops()) != before || len(e.wpcli.calls) != 0 {
				t.Fatal("a refused creation changed something")
			}
			if name == "root" && !exists(s.RootPath) || name == "database" && !e.dbs.has("wp_7") {
				t.Fatal("the existing resource was removed")
			}
		})
	}
}

// A PHP version the machine does not have is refused, naming those it has;
// a machine where none can be found is not blocked.
func TestCreateRefusesAnUninstalledPHP(t *testing.T) {
	e := newEnv(t)
	s := e.site(8, "app.example.com", state.SitePHP)
	if _, err := e.Create(context.Background(), rpc.CreateSiteParams{Site: s}); err != nil {
		t.Fatalf("no PHP found at all must not block: %v", err)
	}
	e = newEnv(t)
	s = e.site(8, "app.example.com", state.SitePHP)
	e.installPHP(t, "8.5")
	// A directory named like a binary is not an installation.
	if err := os.Mkdir(filepath.Join(e.Paths.PHPBinDir, "php-fpm8.4"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := e.Create(context.Background(), rpc.CreateSiteParams{Site: s})
	if err == nil || !strings.Contains(err.Error(), "available: 8.5") {
		t.Fatalf("err %v", err)
	}
	if _, ok := e.accounts.User(s.SystemUser); ok {
		t.Fatal("a user was created")
	}
	s.PHPVersion = "8.5"
	e.create(t, s)
}

// A failure undoes the steps that were done, in reverse, and only them.
func TestCreateFailureUndoesOnlyItsOwnSteps(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fault func(e *env)
	}{
		{"database", func(e *env) { e.dbs.createErr = errors.New("database server down") }},
		{"wordpress", func(e *env) { e.wpcli.fail = "core install" }},
		{"nginx", func(e *env) { e.nginx.TestErr = &host.ConfigError{Output: "bad"} }},
		{"php", func(e *env) { e.exec.Respond = phpTestFails }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			other := e.site(1, "other.example.com", state.SiteWordPress)
			e.create(t, other)
			tc.fault(e)
			s := e.site(2, "new.example.com", state.SiteWordPress)
			if _, err := e.Create(context.Background(), rpc.CreateSiteParams{Site: s, DBPassword: strings.Repeat("p", 20)}); err == nil {
				t.Fatal("the creation succeeded")
			}
			if left := e.leftovers(s); len(left) != 0 {
				t.Fatalf("left behind: %v", left)
			}
			if !exists(other.RootPath) || !exists(e.vhostPath(other)) || !e.dbs.has("wp_1") {
				t.Fatal("the undo touched another site")
			}
			if _, ok := e.accounts.User(other.SystemUser); !ok {
				t.Fatal("the undo removed another site's user")
			}
		})
	}
}

func phpTestFails(c host.FakeCall) host.FakeReply {
	if c.Name == "systemd-run" && strings.Contains(c.Line(), "--fpm-config") && strings.Contains(c.Line(), "--uid=cg-site-2") {
		return host.FakeReply{Code: 78, Stderr: "ERROR: bad pool"}
	}
	return host.FakeReply{}
}

// ---------------------------------------------------------------- delete --

func TestDeleteStopsTheSliceAndRemovesEverything(t *testing.T) {
	e := newEnv(t)
	s := e.site(9, "gone.example.com", state.SiteWordPress)
	s.Config.StagingAuthHash = "$2a$10$" + strings.Repeat("a", 53)
	e.create(t, s)
	if !exists(filepath.Join(e.Paths.NginxAuthDir, s.Domain+".htpasswd")) {
		t.Fatal("control: no password file")
	}
	if _, err := e.Delete(context.Background(), rpc.SiteRef{Site: s}); err != nil {
		t.Fatal(err)
	}
	if !e.systemd.Did("stop", "cg-site-9.slice") {
		t.Fatal("the site's slice was not stopped")
	}
	if left := e.leftovers(s); len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}
	if exists(filepath.Join(e.Paths.NginxAuthDir, s.Domain+".htpasswd")) {
		t.Fatal("the staging password file outlived the site")
	}
	if len(e.certs.removed) != 1 || e.certs.removed[0] != s.Domain {
		t.Fatalf("certificates removed for %v", e.certs.removed)
	}
	if len(e.killed) != 1 || e.killed[0] != s.SystemUser {
		t.Fatalf("the user's processes were ended for %v, want once for %s", e.killed, s.SystemUser)
	}
	// Again: nothing is left, and that is not an error.
	if _, err := e.Delete(context.Background(), rpc.SiteRef{Site: s}); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}

// userdel refusing a user with processes (a cron job crond started, an
// SFTP session: outside the slice) is the one case that kills them, once.
func TestDeleteEndsTheUsersProcessesBeforeUserdel(t *testing.T) {
	// R-3.15: userdel's own busy check is blind from the agent's sandbox,
	// so the processes must be gone before it runs, not after it refuses.
	e := newEnv(t)
	s := e.site(10, "busy.example.com", state.SiteStatic)
	e.create(t, s)
	var userAtKill []bool
	e.Ops.KillUser = func(_ context.Context, user string) error {
		_, ok := e.accounts.User(user)
		userAtKill = append(userAtKill, ok)
		e.killed = append(e.killed, user)
		return nil
	}
	if _, err := e.Delete(context.Background(), rpc.SiteRef{Site: s}); err != nil {
		t.Fatal(err)
	}
	if len(userAtKill) != 1 || !userAtKill[0] {
		t.Fatalf("processes ended %d times, user present each time: %v", len(userAtKill), userAtKill)
	}
	if u, ok := e.accounts.User(s.SystemUser); ok {
		t.Fatalf("the user is still there (%+v)", u)
	}
}

// A process started between the kill and userdel makes userdel refuse:
// they are ended once more and userdel runs again.
func TestDeleteRetriesWhenUserdelFindsTheUserBusy(t *testing.T) {
	e := newEnv(t)
	s := e.site(11, "busy2.example.com", state.SiteStatic)
	e.create(t, s)
	e.Ops.KillUser = func(_ context.Context, user string) error {
		if len(e.killed) == 0 {
			e.accounts.FailNext = fmt.Errorf("userdel: %w", host.ErrUserBusy)
		}
		e.killed = append(e.killed, user)
		return nil
	}
	if _, err := e.Delete(context.Background(), rpc.SiteRef{Site: s}); err != nil {
		t.Fatal(err)
	}
	if len(e.killed) != 2 {
		t.Fatalf("killed %v, want twice", e.killed)
	}
	if _, ok := e.accounts.User(s.SystemUser); ok {
		t.Fatal("the user is still there")
	}
}

// Deleting a site whose record names a root outside the sites directory
// does nothing.
func TestDeleteRefusesARootElsewhere(t *testing.T) {
	e := newEnv(t)
	s := e.site(11, "x.example.com", state.SiteStatic)
	s.RootPath = e.root
	if _, err := e.Delete(context.Background(), rpc.SiteRef{Site: s}); err == nil {
		t.Fatal("accepted")
	}
	if !exists(e.root) {
		t.Fatal("removed")
	}
}

// ----------------------------------------------------------------- apply --

// R-0.7: an apply that changes nothing a master reads leaves it running
// untouched, and nginx is reloaded only when one of its files changed.
func TestApplyReloadsOnlyWhatChanged(t *testing.T) {
	e := newEnv(t)
	s := e.site(12, "app.example.com", state.SitePHP)
	e.create(t, s)
	unit := phpfpm.UnitName(s.SystemUser)
	reloaded := func(from int) bool {
		for _, op := range e.systemd.Ops()[from:] {
			if op == "restart "+unit || op == "reload-or-restart "+unit {
				return true
			}
		}
		return false
	}
	apply := func(s state.Site, onlyIfChanged bool) (int, int) {
		t.Helper()
		from := len(e.systemd.Ops())
		_, before := e.nginx.Counts()
		if _, err := e.Apply(context.Background(), rpc.SiteRef{Site: s, ReloadOnlyIfChanged: onlyIfChanged}); err != nil {
			t.Fatal(err)
		}
		_, after := e.nginx.Counts()
		return from, after - before
	}

	from, nginxReloads := apply(s, true)
	if reloaded(from) || nginxReloads != 0 {
		t.Fatalf("an unchanged apply reloaded (php %v, nginx %d)", reloaded(from), nginxReloads)
	}
	if !e.systemd.Did("start", unit) {
		t.Fatal("a master left alone is not started: one that was down would stay down")
	}
	from, nginxReloads = apply(s, false)
	if !reloaded(from) || nginxReloads != 1 {
		t.Fatal("a plain apply no longer reloads")
	}
	s.Config.PHP.MemoryLimitMB = 384
	calls := len(e.exec.Calls())
	from, nginxReloads = apply(s, true)
	if !reloaded(from) || nginxReloads != 0 {
		t.Fatalf("a changed pool: php reloaded %v, nginx %d", reloaded(from), nginxReloads)
	}
	if len(e.exec.Calls()) == calls {
		t.Fatal("a changed pool was loaded without a test")
	}
	if err := os.Remove(e.vhostPath(s)); err != nil {
		t.Fatal(err)
	}
	from, nginxReloads = apply(s, true)
	if reloaded(from) || nginxReloads != 1 {
		t.Fatalf("a missing vhost: php reloaded %v, nginx %d", reloaded(from), nginxReloads)
	}
}

// A panel upgrade re-applies every site: the live release's connector is
// brought up to date.
func TestApplyRefreshesTheConnector(t *testing.T) {
	e := newEnv(t)
	s := e.site(13, "blog.example.com", state.SiteWordPress)
	e.create(t, s)
	conn := filepath.Join(s.RootPath, "current", wp.ConnectorPlugin)
	if err := os.WriteFile(conn, []byte("<?php // old"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := e.Apply(context.Background(), rpc.SiteRef{Site: s})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(conn); string(b) != connector.PHP {
		t.Fatal("the old connector stayed")
	}
	found := 0
	for _, f := range res.Files {
		if f.Path == conn {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("the connector is reported %d times", found)
	}
}

// A failed apply restores every file as it was, its mode included: a
// staging vhost holds the key that passes its password, and was put back
// world-readable (0644) by the old rollback.
func TestFailedApplyRestoresFilesWithTheirModes(t *testing.T) {
	e := newEnv(t)
	s := e.site(14, "staging.example.com", state.SitePHP)
	s.Config.StagingAuthHash = "$2a$10$" + strings.Repeat("c", 53)
	e.create(t, s)
	vhost := e.vhostPath(s)
	before, _ := os.ReadFile(vhost)
	if fi, _ := os.Stat(vhost); fi.Mode().Perm() != 0o600 {
		t.Fatalf("control: vhost mode %v", fi.Mode().Perm())
	}
	poolBefore, _ := os.ReadFile(e.poolPath(s))
	s.Aliases = []string{"www.staging.example.com"}
	s.Config.PHP.MemoryLimitMB = 512
	e.nginx.TestErr = &host.ConfigError{Output: "bad"}
	if _, err := e.Apply(context.Background(), rpc.SiteRef{Site: s}); err == nil {
		t.Fatal("the apply succeeded")
	}
	e.nginx.TestErr = nil
	after, _ := os.ReadFile(vhost)
	if string(after) != string(before) {
		t.Fatal("the vhost was not restored")
	}
	if fi, _ := os.Stat(vhost); fi.Mode().Perm() != 0o600 {
		t.Fatalf("the restored vhost is %v", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(e.poolPath(s)); fi.Mode().Perm() != 0o640 {
		t.Fatalf("the restored pool is %v", fi.Mode().Perm())
	}
	if b, _ := os.ReadFile(e.poolPath(s)); string(b) != string(poolBefore) {
		t.Fatal("the pool was not restored")
	}
}

// A first render that fails has no master to go back to: the rollback
// does not try to start one, and so does not report its own failure.
func TestFailedFirstRenderRollsBackCleanly(t *testing.T) {
	e := newEnv(t)
	s := e.site(15, "first.example.com", state.SitePHP)
	e.Nginx = &badOnce{FakeNginx: e.nginx}
	_, err := e.Render(context.Background(), s)
	if err == nil || strings.Contains(err.Error(), "rolling the configuration back also failed") {
		t.Fatalf("err %v", err)
	}
	if exists(e.unitPath(s)) || exists(e.vhostPath(s)) || exists(e.dropinDir(s)) {
		t.Fatal("files of the failed render are left")
	}
	if u, ok := e.systemd.Unit(phpfpm.UnitName(s.SystemUser)); ok && u.ActiveState == "active" {
		t.Fatal("the master the failed render started is still running")
	}
}

// badOnce is nginx refusing the first configuration it tests: the one the
// render wrote. The restored one passes.
type badOnce struct {
	*host.FakeNginx
	tested bool
}

func (b *badOnce) Test(ctx context.Context) error {
	if !b.tested {
		b.tested = true
		return &host.ConfigError{Output: "bad"}
	}
	return b.FakeNginx.Test(ctx)
}

// ------------------------------------------------------------ the rest --

func TestBudgetSharesAQuarterOfTheMachine(t *testing.T) {
	for _, c := range []struct {
		total   int64
		sites   int
		want    int
		floored bool
	}{{2048, 1, 512, false}, {2048, 0, 512, false}, {2048, 2, 256, false}, {2048, 4, 160, true}, {2048, 100, 160, true}} {
		if got, floored := shareOf(c.total, c.sites); got != c.want || floored != c.floored {
			t.Errorf("shareOf(%d, %d) = %d %v", c.total, c.sites, got, floored)
		}
	}
	root := t.TempDir()
	for _, d := range []string{"a/releases", "b/releases", "c"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if n := provisionedSites(root); n != 2 {
		t.Errorf("counted %d sites, a half-built one included", n)
	}
	if n := provisionedSites(filepath.Join(root, "none")); n != 1 {
		t.Errorf("no sites directory counts %d", n)
	}
	e := newEnv(t)
	e.MemTotalMB = func() int64 { return 4096 }
	s := e.site(16, "auto.example.com", state.SitePHP)
	s.Config.Resources.MemoryHighMB = 0
	if got := e.budget(s); got != 1024 {
		t.Errorf("auto budget %d", got)
	}
}

// nginx -V is read once, from what BuildInfo returns (nginx writes it to
// stderr), for HTTP/3 and the http2 directive.
func TestNginxBuildIsProbedOnce(t *testing.T) {
	e := newEnv(t)
	e.nginx.Build = "nginx version: nginx/1.28.0\nconfigure arguments: --with-http_v3_module"
	in := e.input(context.Background(), e.site(17, "a.example.com", state.SiteStatic))
	if !in.HTTP3 || !in.HTTP2Directive {
		t.Fatalf("%+v", in)
	}
	e.nginx.Build = ""
	if in := e.input(context.Background(), e.site(17, "a.example.com", state.SiteStatic)); !in.HTTP3 {
		t.Fatal("probed again")
	}
	e = newEnv(t)
	if in := e.input(context.Background(), e.site(17, "a.example.com", state.SiteStatic)); in.HTTP3 || in.HTTP2Directive {
		t.Fatalf("nginx 1.24 without the module: %+v", in)
	}
}

// Creates and deletes of many sites at once leave exactly the live sites'
// vhosts, users and trees.
func TestConcurrentCreateAndDelete(t *testing.T) {
	e := newEnv(t)
	var initial []state.Site
	for i := int64(1); i <= 4; i++ {
		s := e.site(i, fmt.Sprintf("old%d.example.com", i), state.SitePHP)
		e.create(t, s)
		initial = append(initial, s)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := int64(5); i <= 8; i++ {
		wg.Add(2)
		go func(s state.Site) {
			defer wg.Done()
			_, err := e.Create(context.Background(), rpc.CreateSiteParams{Site: s})
			errs <- err
		}(e.site(i, fmt.Sprintf("new%d.example.com", i), state.SitePHP))
		go func(s state.Site) {
			defer wg.Done()
			_, err := e.Delete(context.Background(), rpc.SiteRef{Site: s})
			errs <- err
		}(initial[i-5])
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	vhosts, _ := filepath.Glob(filepath.Join(e.Paths.NginxSites, "*.conf"))
	if len(vhosts) != 4 {
		t.Fatalf("vhosts %v", vhosts)
	}
	for _, v := range vhosts {
		if !strings.HasPrefix(filepath.Base(v), "new") {
			t.Fatalf("a deleted site's vhost: %s", v)
		}
	}
	for _, s := range initial {
		if left := e.leftovers(s); len(left) != 0 {
			t.Fatalf("%s left %v", s.Domain, left)
		}
	}
}

// The sandbox wp-cli runs in binds the site's log directory: it must exist
// before the first wp-cli call of a WordPress provisioning (the download),
// or systemd refuses the unit with 226/NAMESPACE.
func TestCreateWordPressMakesTheLogDirBeforeWPCLI(t *testing.T) {
	e := newEnv(t)
	s := e.site(7, "logs.example.com", state.SiteWordPress)
	logDir := filepath.Join(e.Paths.LogRoot, s.Domain)
	var seen []bool
	e.wpcli.onRun = func(inv host.Invocation) {
		seen = append(seen, exists(logDir))
		if strings.HasPrefix(strings.Join(inv.Args, " "), "config create") {
			_ = os.WriteFile(filepath.Join(inv.Path, "wp-config.php"), []byte("<?php // config"), 0o640)
		}
	}
	p := rpc.CreateSiteParams{Site: s, DBPassword: strings.Repeat("d", 20), AdminUser: "admin",
		AdminPassword: "admin-secret", AdminEmail: "ops@example.com", ConnectorToken: "token-secret"}
	if _, err := e.Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if len(seen) == 0 || !seen[0] {
		t.Fatalf("the log directory existed at the wp-cli calls: %v", seen)
	}
}
