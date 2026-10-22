//go:build e2e

package e2e

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const wpAdminPW = "WpBench!2026Pass"

// section is one top-level subtest, TestE2E/<name>. Every section can run on
// its own (-test.run '^TestE2E$/^<name>$', spaces as underscores) or with
// any other set of sections, and gives the same verdicts as in a full run:
// what it needs from outside itself is a fixture it declares here (see
// fixtures_test.go), made by whichever section asks first. e2e/SECTIONS.md
// documents each section; e2e/sections.map says which code changes affect
// it.
type section struct {
	name     string
	run      func(*harness, *testing.T)
	fixtures []string
	// lane: with CG_E2E_PARALLEL=1, the sections of one lane run one after
	// another, in this order, beside the other lanes. A section with no lane
	// is exclusive: it changes or measures something every section shares
	// (a service restart, nginx's error log, the PATH the agent dumps
	// databases with, a performance measurement, a long request a PHP reload
	// would cut), so it runs alone, after the lanes, in this order. Without
	// CG_E2E_PARALLEL every section runs alone, in this order.
	lane string
}

func (s section) uses(fixture string) bool {
	for _, f := range s.fixtures {
		if f == fixture {
			return true
		}
	}
	return false
}

// sections, in the order of the bash suite (scripts/e2e-verify.sh), which a
// full sequential run keeps. A unit test without the e2e tag
// (sections_test.go) reads this table from the source: keep it a plain
// composite literal of string literals, fx* constants and
// (*harness).<method> values.
var sections = []section{
	{name: "R-6.1 installer", run: (*harness).installer},
	{name: "site types", run: (*harness).siteTypes, lane: "site types"},
	{name: "woocommerce", run: (*harness).woocommerce, fixtures: []string{fxWooSite}, lane: "woocommerce"},
	{name: "wordpress cache", run: (*harness).wordpressCache, fixtures: []string{fxFeatureSite}, lane: "feature"},
	{name: "wordpress staging", run: (*harness).wordpressStaging, fixtures: []string{fxFeatureSite, fxFeatureStaging}, lane: "feature"},
	{name: "wordpress status and drift", run: (*harness).wordpressStatusDrift, fixtures: []string{fxFeatureSite}},
	{name: "wordpress certificates", run: (*harness).wordpressCertificates, fixtures: []string{fxFeatureSite}, lane: "feature"},
	{name: "wordpress sftp", run: (*harness).wordpressSFTP, fixtures: []string{fxFeatureSite}, lane: "feature"},
	{name: "wordpress logs", run: (*harness).wordpressLogs, fixtures: []string{fxFeatureSite}},
	{name: "wordpress slow request", run: (*harness).wordpressSlowRequest, fixtures: []string{fxFeatureSite}},
	{name: "wordpress cron", run: (*harness).wordpressCron, fixtures: []string{fxFeatureSite}, lane: "feature"},
	{name: "wordpress vulnerabilities", run: (*harness).wordpressVulnerabilities, fixtures: []string{fxFeatureSite}, lane: "feature"},
	{name: "wordpress safe push", run: (*harness).wordpressSafePush, fixtures: []string{fxFeatureSite, fxFeatureStaging}},
	{name: "wordpress releases", run: (*harness).wordpressReleases, fixtures: []string{fxFeatureSite}, lane: "feature"},
	{name: "wordpress backups", run: (*harness).wordpressBackups, fixtures: []string{fxFeatureSite, fxFeatureStaging}},
	{name: "R-4.6 cgctl tokens", run: (*harness).cgctlTokens, fixtures: []string{fxFeatureSite}},
	{name: "R-4.6 backup interruption", run: (*harness).backupInterruption, fixtures: []string{fxFeatureSite}},
	{name: "helper units", run: (*harness).helperUnits, fixtures: []string{fxFeatureSite}},
	{name: "R-4.3 deploys and backups", run: (*harness).deploysAndBackups, fixtures: []string{fxFeatureSite, fxFeatureStaging}},
	{name: "firewall", run: (*harness).firewall},
	{name: "blocklist", run: (*harness).blocklistSection, fixtures: []string{fxFeatureSite}},
	{name: "site blocking", run: (*harness).siteBlocking, lane: "site blocking"},
	{name: "R-4.3 proxies and alerts", run: (*harness).proxiesAlerts, fixtures: []string{fxFeatureSite}},
	{name: "F9b site databases", run: (*harness).siteDatabases, fixtures: []string{fxFeatureSite, fxFeatureStaging}},
	{name: "migration safety snapshot", run: (*harness).migrationSafety},
	{name: "F9b site creation", run: (*harness).siteCreation, lane: "site creation"},
	{name: "F9b Quarry service", run: (*harness).dbStudioService, fixtures: []string{fxWooSite, fxFeatureSite}},
	{name: "wordpress teardown", run: (*harness).wordpressTeardown, fixtures: []string{fxFeatureSite, fxFeatureStaging}},
	{name: "migration import", run: (*harness).migration, lane: "migration"},
	{name: "security", run: (*harness).security, lane: "security"},
	{name: "woocommerce security", run: (*harness).wooSecurity, fixtures: []string{fxWooSite}, lane: "woocommerce"},
	{name: "wordpress two-factor", run: (*harness).wpTwoFactor, fixtures: []string{fxWooSite}, lane: "woocommerce"},
	{name: "wordpress hardening", run: (*harness).wpHardening, fixtures: []string{fxWooSite}, lane: "woocommerce"},
	{name: "F4b Quarry", run: (*harness).dbStudioSection, fixtures: []string{fxWooSite}},
	{name: "R-0.7 reconcile", run: (*harness).reconcile},
	{name: "R-5.2 reload drain", run: (*harness).reloadDrain},
	{name: "R-2.4 services", run: (*harness).services},
	{name: "R-3.9 certificates and panel", run: (*harness).certsPanel},
	{name: "R-3.7 site lifecycle", run: (*harness).siteLifecycle},
	{name: "R-3.13 interrupted create", run: (*harness).interruptedCreate},
	{name: "R-4.5 site shell", run: (*harness).siteShell, lane: "shell"},
	{name: "R-4.2 job queue", run: (*harness).jobQueue},
	{name: "F8 events", run: (*harness).events},
	{name: "R-4.7 capabilities", run: (*harness).capabilities, lane: "capabilities"},
	{name: "R-5.4 routing", run: (*harness).routing, lane: "routing"},
	{name: "R-4.3 accounts", run: (*harness).accounts},
	{name: "R-4.3 sign-in review", run: (*harness).signinReview},
	{name: "R-4.3 site settings", run: (*harness).siteSettings, lane: "settings"},
	{name: "dev mode", run: (*harness).devMode, lane: "dev mode"},
	{name: "maintenance page", run: (*harness).maintenancePage, lane: "maintenance page"},
	{name: "protected paths", run: (*harness).protectedPaths, lane: "protected paths"},
	{name: "uptime", run: (*harness).uptime, lane: "uptime"},
	{name: "malware scan", run: (*harness).malwareScan, lane: "malware"},
	{name: "R-6.3 release channel", run: (*harness).releaseChannel, lane: "settings"},
	{name: "SR tenant data", run: (*harness).tenantData, fixtures: []string{fxWooSite}},
	{name: "R-10.5 root boundary", run: (*harness).boundary, lane: "boundary"},
	{name: "R-10.5 agent limits", run: (*harness).agentLimits},
	{name: "site resources", run: (*harness).siteResources, lane: "resources"},
	{name: "PHP extensions and backup hour", run: (*harness).phpExtensions},
	{name: "woocommerce teardown", run: (*harness).woocommerceTeardown, fixtures: []string{fxWooSite}},
	{name: "R-6.5 upgrade", run: (*harness).upgrade},
}

// TestE2E is the end-to-end suite: the sections above, against the server
// it runs on.
func TestE2E(t *testing.T) {
	h := newHarness(t)
	t.Logf("CloudGround E2E verification — %s  panel=%s ip=%s php=%s",
		time.Now().UTC().Format(time.RFC3339), h.Panel, h.IP, h.PHPV)
	if !h.api.login() {
		t.Fatalf("[login] FAILED as %s over %s", h.Email, h.Sock)
	}
	if parallel() {
		h.runLanes(t)
		return
	}
	for _, s := range sections {
		h.runSection(t, s)
	}
}

func parallel() bool { return os.Getenv("CG_E2E_PARALLEL") != "" }

// runSection runs one section as TestE2E/<name> (when -test.run selects it)
// and keeps its time for the summary at the end of the run.
func (h *harness) runSection(t *testing.T, s section) {
	t.Run(s.name, func(t *testing.T) {
		start := time.Now()
		defer func() { h.sectionDone(s.name, time.Since(start)) }()
		s.run(h, t)
	})
}

// runLanes is CG_E2E_PARALLEL=1: one goroutine per lane runs that lane's
// sections in order (t.Run may be called from several goroutines at once,
// and the subtests keep their TestE2E/<name> paths, so -test.run selects
// the same sections either way); once every lane is done, the exclusive
// sections run one at a time.
func (h *harness) runLanes(t *testing.T) {
	lanes := map[string][]section{}
	var order []string
	for _, s := range sections {
		if s.lane == "" {
			continue
		}
		if _, ok := lanes[s.lane]; !ok {
			order = append(order, s.lane)
		}
		lanes[s.lane] = append(lanes[s.lane], s)
	}
	var wg sync.WaitGroup
	for _, l := range order {
		wg.Add(1)
		go func(secs []section) {
			defer wg.Done()
			for _, s := range secs {
				h.runSection(t, s)
			}
		}(lanes[l])
	}
	wg.Wait()
	for _, s := range sections {
		if s.lane == "" {
			h.runSection(t, s)
		}
	}
}

func (h *harness) dom(label string) string { return label + "." + h.Dash + ".sslip.io" }

func (h *harness) wpSiteJSON(dom, typ, extra string) string {
	return fmt.Sprintf(`{"domain":%q,"type":%q,"php_version":%q,"admin_email":%q,"admin_user":"admin","admin_password":%q%s}`,
		dom, typ, h.PHPV, h.Email, wpAdminPW, extra)
}

// ------------------------------------------------------------ site types --

type siteType struct {
	label, dom, json, path string
	expect                 string // exact code, or "ok" for any non-5xx (empty app is fine)
	prep                   func(t *testing.T, u, dom string)
	upstream               bool // proxy: stand up a real upstream on 127.0.0.1:9099
}

// siteTypes: per type create -> active -> serves -> delete(async) -> teardown gone.
// The types share nothing, so CG_E2E_PARALLEL=1 runs them side by side;
// by default they run in the bash order, one at a time. Their checks are
// named after the type and sit directly under the section either way.
func (h *harness) siteTypes(t *testing.T) {
	types := []siteType{
		{label: "static", dom: h.dom("static"), path: "/", expect: "200",
			json: fmt.Sprintf(`{"domain":%q,"type":"static"}`, h.dom("static"))},
		// php: inject an index.php so PHP execution is exercised (empty = 403).
		{label: "php", dom: h.dom("php"), path: "/", expect: "200",
			json: fmt.Sprintf(`{"domain":%q,"type":"php","php_version":%q}`, h.dom("php"), h.PHPV),
			prep: func(t *testing.T, u, dom string) {
				h.mustWriteAs(t, "/srv/sites/"+dom+"/current/index.php", `<?php echo "php-exec-ok";`, u)
			}},
		// proxy: a real upstream, so forwarding is tested and not a bad gateway.
		{label: "proxy", dom: h.dom("proxy"), path: "/", expect: "200", upstream: true,
			json: fmt.Sprintf(`{"domain":%q,"type":"proxy","proxy_upstream":"http://127.0.0.1:9099"}`, h.dom("proxy"))},
		// laravel: no app code deployed yet, so any non-5xx (200/404) is fine.
		{label: "laravel", dom: h.dom("laravel"), path: "/", expect: "ok",
			json: fmt.Sprintf(`{"domain":%q,"type":"laravel","php_version":%q}`, h.dom("laravel"), h.PHPV)},
		{label: "wordpress", dom: h.dom("wp"), path: "/", expect: "200",
			json: h.wpSiteJSON(h.dom("wp"), "wordpress", "")},
	}
	if parallel() {
		// All five at once, then the R-1.2 check once they are all done.
		var wg sync.WaitGroup
		for _, st := range types {
			wg.Add(1)
			go func() { defer wg.Done(); h.runType(t, st) }()
		}
		wg.Wait()
		h.distFPMInactive(t)
		return
	}
	for _, st := range types {
		h.runType(t, st)
		if st.label == "php" { // the bash suite checks it right after the php type
			h.distFPMInactive(t)
		}
	}
}

// distFPMInactive (R-1.2): PHP ran in the site's own master; the
// distribution's shared php<ver>-fpm serves nothing and is not running.
func (h *harness) distFPMInactive(t *testing.T) {
	h.check(t, "R-1.2: the distribution's php"+h.PHPV+"-fpm is not running",
		h.run("systemctl", "is-active", "php"+h.PHPV+"-fpm").Trim(), "inactive")
}

func (h *harness) runType(t *testing.T, st siteType) {
	if st.upstream {
		ln, err := net.Listen("tcp", "127.0.0.1:9099")
		if err != nil {
			t.Logf("proxy upstream: %v", err)
		} else {
			srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintln(w, "<html><body>cg-e2e upstream</body></html>")
			})}
			go srv.Serve(ln)
			defer srv.Close()
		}
	}
	id := h.api.createSite(st.json)
	if id == 0 {
		h.fail(t, st.label+": create rejected", "%s: create rejected", st.label)
		return
	}
	status := h.api.waitActive(id, 120*time.Second)
	h.check(t, st.label+" provisions", status, "active")
	if status == "active" {
		u := h.api.site(id).SystemUser
		h.siteModes(t, st.label, st.dom, u, st.label != "static" && st.label != "proxy")
		if st.prep != nil {
			st.prep(t, u, st.dom)
		}
		c, fatal := h.serves(st.dom, st.path)
		if st.expect == "ok" {
			n, err := strconv.Atoi(c)
			h.assert(t, st.label+" serves", err == nil && n > 0 && n < 500, "%s serves (%s)", st.label, c)
		} else {
			h.check(t, st.label+" serves HTTP", c, st.expect)
		}
		h.check(t, st.label+" no PHP fatal", fatal, "no")
	}
	h.check(t, st.label+" delete accepted", h.api.deleteSite(id), "202")
	h.check(t, st.label+" teardown completes", h.api.teardownGone(id), "yes")
}

// ----------------------------------------------------------- woocommerce --

// woocommerce: the PRODUCT must install WooCommerce itself and serve its
// pages. Do NOT install the plugin from the test: that would test the test's
// setup and mask a provisioner that ships plain WordPress. The site is the
// "woocommerce site" fixture, kept until the "woocommerce teardown" section.
func (h *harness) woocommerce(t *testing.T) {
	w, ok := h.wooSite(t)
	if !ok {
		return
	}
	wid, woo, u, p := w.id, w.dom, w.user, w.path
	// The provisioner, not this suite, must have installed and activated Woo.
	ver := h.wp(u, p, "plugin", "get", "woocommerce", "--field=version").Trim()
	h.assert(t, "woocommerce installed by provisioner", ver != "", "provisioner did not install WooCommerce")
	if ver != "" {
		t.Logf("woocommerce %s", ver)
	}
	c, fatal := h.serves(woo, "/")
	h.check(t, "woocommerce home serves (was the 500 bug)", c, "200")
	h.check(t, "woocommerce no PHP fatal", fatal, "no")
	// The Woo-registered routes must resolve, not 404 (the stale-APCu rewrite bug).
	sc, _ := h.serves(woo, "/shop/")
	h.check(t, "woocommerce /shop resolves", sc, "200")
	cc, _ := h.serves(woo, "/cart/")
	h.check(t, "woocommerce /cart resolves", cc, "200")

	// Issue #10: the site's PHP runs with the tracing JIT. The mode set in the
	// pool was ignored (PHP reads it once, at the master's start), so every
	// site ran without it while its configuration said otherwise. Asked of
	// the site's own PHP-FPM, not of the configuration.
	h.mustWriteAs(t, p+"/cg-e2e-jit.php",
		`<?php $j = opcache_get_status(false)['jit'] ?? []; echo 'jit=', !empty($j['on']) ? 'on' : 'off', ' mode=', ini_get('opcache.jit');`, u)
	jit := strings.TrimSpace(string(h.get(woo, fmt.Sprintf("/cg-e2e-jit.php?x=%d", time.Now().UnixNano())).Body))
	h.check(t, "issue #10: the site's PHP-FPM runs the tracing JIT", jit, "jit=on mode=tracing")
	h.remove(p + "/cg-e2e-jit.php")

	// R-3.8: the warmer covers WooCommerce products (it used to list only
	// posts and pages). The product is test data; WooCommerce is the
	// provisioner's.
	pid := h.wp(u, p, "post", "create", "--post_type=product", "--post_status=publish",
		"--post_title=E2E warm product", "--porcelain").Trim()
	purl := URLPath(h.wp(u, p, "post", "list", "--post_type=product", "--post__in="+pid, "--field=url").Trim())
	h.assert(t, "R-3.8: woocommerce test product created", pid != "" && strings.HasPrefix(purl, "/"),
		"could not create a test product: %q %q", pid, purl)
	if pid != "" && strings.HasPrefix(purl, "/") {
		counts := h.warmSite(t, wid, woo, "R-3.8: woocommerce")
		h.check(t, "R-3.8: the warmer warms a WooCommerce product once", strconv.Itoa(counts[purl]), "1")
		h.wp(u, p, "post", "delete", pid, "--force")
	}
	h.wooPricePurge(t, woo, u, p)
}

// woocommerceTeardown deletes the WooCommerce site, which also verifies
// that a commerce site tears down.
func (h *harness) woocommerceTeardown(t *testing.T) {
	w, ok := h.wooSite(t)
	if !ok {
		return
	}
	h.dropFixture(t, fxWooSite)
	h.check(t, "woocommerce delete accepted", h.api.deleteSite(w.id), "202")
	h.check(t, "woocommerce teardown completes", h.api.teardownGone(w.id), "yes")
}

// ---------------------------------------------------------- migration ----

// migration: a WordPress tree with its mu-plugins REMOVED, which is what a
// tree from any other host looks like. If the importer forgets to reinstate
// the connector, cache invalidation is silently dead on every imported site.
func (h *harness) migration(t *testing.T) {
	h.clearStandIns(t, "wp")
	ms := h.dom("mig")
	mid := h.api.createSite(h.wpSiteJSON(ms, "wordpress", ""))
	st := h.api.waitActive(mid, 120*time.Second)
	h.check(t, "migration-site provisions", st, "active")
	if st != "active" {
		return
	}
	mu := h.api.site(mid).SystemUser
	mroot := "/srv/sites/" + ms
	fix, err := os.MkdirTemp("", "cg-e2e-mig-")
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	cleanFix := h.undo("remove migration fixture", func() { _ = os.RemoveAll(fix) })

	// A foreign-looking tree: real WordPress, no connector, plus a marker
	// file proving the archive's contents landed.
	h.run("cp", "-a", mroot+"/current/.", fix+"/")
	h.remove(fix + "/wp-content/mu-plugins")
	_ = os.WriteFile(fix+"/imported-marker.txt", []byte("imported-by-e2e\n"), 0o644)
	if r := h.run("tar", "czf", mroot+"/migration.tar.gz", "-C", fix, "."); !r.OK() {
		t.Logf("fixture: tar: %s", r)
	}
	// The dump goes in shared/: the root is root:site 0750, so the site user
	// cannot write there. Its exit status is checked, unlike the old version
	// that failed silently for as long as the check existed.
	if r := h.wp(mu, mroot+"/current", "db", "export", mroot+"/shared/migration.sql", "--quiet"); !r.OK() {
		h.fail(t, "migration fixture: could not export the database dump", "migration fixture: could not export the database dump: %s", r)
	}
	cleanFix()

	_, b := h.api.post(fmt.Sprintf("/api/sites/%d/migration", mid),
		fmt.Sprintf(`{"archive":"migration.tar.gz","sql":"shared/migration.sql","old_domain":%q,"confirm":%q}`, ms, ms))
	mtask := taskID(b)
	h.check(t, "migration import accepted", yesNo(mtask != 0), "yes")
	h.check(t, "migration import succeeds", h.api.waitTask(mtask, 240*time.Second), "succeeded")
	// #41: with no backup repository there is no safety snapshot, and the
	// task says so (the lanes run before any section configures one).
	if _, sb := h.api.get("/api/settings"); jsonField(sb, "backup_repository") == "" {
		_, rec := h.api.get(fmt.Sprintf("/api/tasks/%d", mtask))
		h.assert(t, "#41: without a backup repository the migration's task says it cannot be undone",
			strings.Contains(string(rec), "this import cannot be undone"), "task record: %s", head(rec, 600))
	}

	// The archive's own content must be live.
	h.assert(t, "migrated files are live", exists(mroot+"/current/imported-marker.txt"), "migrated files are live")
	// THE regression: the connector must survive an import.
	h.assert(t, "cache connector survives migration", exists(mroot+"/current/wp-content/mu-plugins/cloudground-connector.php"),
		"cache connector survives migration (imported release has no mu-plugin)")

	// A migrated site must still serve, and still cache.
	c, fatal := h.serves(ms, "/")
	h.check(t, "migrated site serves", c, "200")
	h.check(t, "migrated site no PHP fatal", fatal, "no")
	for i := 0; i < 3; i++ {
		h.get(ms, "/")
	}
	h.check(t, "migrated site caches", CacheStatus(h.get(ms, "/").Header), "HIT")

	// The importer must not leave a full database dump lying in the site tree.
	left := exists(mroot+"/migration.tar.gz") || exists(mroot+"/migration.sql")
	h.check(t, "migration inputs cleaned up", yesNo(left), "no")

	h.migrationPrefix(t, mid, ms, mu, mroot)
	h.migrationArchives(t, mid, ms, mroot)

	h.check(t, "migration-site delete accepted", h.api.deleteSite(mid), "202")
	h.check(t, "migration-site teardown completes", h.api.teardownGone(mid), "yes")
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func contains(b []byte, s string) bool { return strings.Contains(string(b), s) }

// dbStudioSection runs F4b on the WooCommerce site, the one with a database.
func (h *harness) dbStudioSection(t *testing.T) {
	w, ok := h.wooSite(t)
	if !ok {
		return
	}
	h.dbStudio(t, w.id)
}
