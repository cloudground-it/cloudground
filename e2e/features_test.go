//go:build e2e

package e2e

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const chromeAE = "gzip, deflate, br, zstd"

// The WordPress feature sections all work on one site, the "feature site"
// fixture (fixtures_test.go), in the bash suite's order: cache, object
// cache, magic login, staging, certificates, SFTP, logs, cron, safe push,
// releases and backups, then the firewall and the site's teardown. Each
// section puts in place what it needs from the site's state, so it can run
// on its own (e2e/SECTIONS.md).

func (h *harness) wordpressCache(t *testing.T) {
	h.withFeature(t, func(f *feat) {
		f.cache(t)
		f.objectCacheLoginCronPurge(t)
		f.warmer(t)
		f.objectCacheStats(t)
		f.objectCacheReenable(t)
		f.templatePartPurge(t)
		f.purgeCoverage(t)
		f.purgeWithPort(t)
	})
}

func (h *harness) wordpressStaging(t *testing.T) {
	h.withFeature(t, func(f *feat) { f.staging(t) })
}

func (h *harness) wordpressStatusDrift(t *testing.T) {
	h.withFeature(t, func(f *feat) {
		f.statusEndpoints(t)
		f.driftPlanted(t)
	})
}

func (h *harness) wordpressCertificates(t *testing.T) {
	h.withFeature(t, func(f *feat) {
		f.customCertificate(t)
		f.certificateRace(t)
		f.customCertificateDelete(t)
	})
}

func (h *harness) wordpressSFTP(t *testing.T) {
	h.withFeature(t, func(f *feat) {
		f.sftp(t)
		f.concurrentChanges(t)
	})
}

func (h *harness) wordpressLogs(t *testing.T) {
	h.withFeature(t, func(f *feat) {
		f.phpErrorLog(t)
		f.systemLogs(t)
		f.nginxReopen(t)
	})
}

func (h *harness) wordpressSlowRequest(t *testing.T) {
	h.withFeature(t, func(f *feat) { f.slowRequest(t) })
}

func (h *harness) wordpressCron(t *testing.T) {
	h.withFeature(t, func(f *feat) {
		f.cron(t)
		f.cronJobs(t)
	})
}

func (h *harness) wordpressSafePush(t *testing.T) {
	if f, stg, ok := h.featureStaging(t); ok {
		f.safePush(t, stg)
	}
}

func (h *harness) wordpressReleases(t *testing.T) {
	h.withFeature(t, func(f *feat) {
		f.releases(t)
		f.releaseFromSharedFolder(t)
	})
}

func (h *harness) wordpressBackups(t *testing.T) {
	h.clearStandIns(t, "mariadb-dump", "restic")
	if f, _, ok := h.featureStaging(t); ok {
		// The staging push checks read production's options through its
		// object cache (R-3.3 control below).
		f.ensureObjectCache(t, "the backup checks")
		f.backups(t)
	}
}

func (h *harness) firewall(t *testing.T) {
	h.firewallRules(t)
	h.firewallGuards(t)
	h.firewallEnable(t)
}

// wordpressTeardown deletes the feature site, and with it its staging copy.
func (h *harness) wordpressTeardown(t *testing.T) {
	f, ok := h.featureSite(t)
	if !ok {
		return
	}
	// The staging copy must exist for the R-3.3 check to mean anything; the
	// site is deleted (and checked) either way.
	_, stg, withStaging := h.featureStaging(t)
	auth := stagingAuthFile(stg.Domain)
	if withStaging {
		_, err := os.Stat(auth)
		h.assert(t, "R-3.3 control: the staging password file is there before the site is deleted", err == nil,
			"no password file for %s before the delete: %v", stg.Domain, err)
	}
	h.dropFixture(t, fxFeatureStaging)
	h.dropFixture(t, fxFeatureSite)
	h.check(t, "feature-site delete accepted", h.api.deleteSite(f.fid), "202")
	h.check(t, "feature-site teardown completes", h.api.teardownGone(f.fid), "yes")
	if withStaging {
		// Deleting the site deletes its staging copy; the copy's password
		// file (its password's hash) used to stay in nginx's auth directory.
		_, err := os.Stat(auth)
		h.assert(t, "R-3.3: the staging password file goes with the staging site", os.IsNotExist(err),
			"%s is still there after the site and its staging copy were deleted", auth)
	}
}

// stagingAuthFile is the nginx password file of a staging site.
func stagingAuthFile(domain string) string {
	return "/etc/nginx/cloudground-auth/" + domain + ".htpasswd"
}

type feat struct {
	h      *harness
	fs, fp string // domain, current release path
	fsu    string // system user
	fid    int64
}

func (f *feat) api(format string, a ...any) string {
	return fmt.Sprintf("/api/sites/%d", f.fid) + fmt.Sprintf(format, a...)
}

// --------------------------------------------------------------- cache --

func (f *feat) cache(t *testing.T) {
	h := f.h
	// The first view of a fresh site makes two writes of WordPress's own:
	// it stores the Custom CSS lookup in the theme mods
	// (wp_get_custom_css_post) and saves the Navigation block's fallback
	// menu as a wp_navigation post (WP_Navigation_Fallback). The connector
	// purged everything for either, after the response, so the page that
	// view cached was gone a moment later (MISS, HIT, HIT, MISS). Control:
	// neither was there before the view and both are after it.
	state := func() string {
		theme := h.wp(f.fsu, f.fp, "option", "get", "stylesheet").Trim()
		mods := h.wp(f.fsu, f.fp, "option", "get", "theme_mods_"+theme, "--format=json").Trim()
		menus := h.wp(f.fsu, f.fp, "post", "list", "--post_type=wp_navigation", "--format=count").Trim()
		return fmt.Sprintf("custom_css_post_id=%v menus=%s", strings.Contains(mods, "custom_css_post_id"), menus)
	}
	const fresh, viewed = "custom_css_post_id=false menus=0", "custom_css_post_id=true menus=1"
	if before := state(); before != fresh {
		t.Logf("the feature site was viewed before this section: the first-view check is skipped (%s)", before)
	} else {
		first := h.get(f.fs, "/")
		time.Sleep(1500 * time.Millisecond) // the connector's purge goes after the response
		again := h.get(f.fs, "/")
		h.check(t, "F7 control: the first view stores the Custom CSS lookup and the fallback menu", state(), viewed)
		h.assert(t, "F7: the first view of a fresh site leaves the page it cached in the cache", CacheStatus(again.Header) == "HIT",
			"first view %s %s, 1.5 s later %s", first.CodeStr(), CacheStatus(first.Header), CacheStatus(again.Header))
	}
	// warm then expect HIT
	var seen []string
	for i := 0; i < 3; i++ {
		r := h.get(f.fs, "/")
		seen = append(seen, fmt.Sprintf("%s %s %s cookie=%q cc=%q", time.Now().Format("15:04:05.000"), r.CodeStr(), CacheStatus(r.Header), r.Header.Values("Set-Cookie"), r.Header.Get("Cache-Control")))
	}
	last := h.get(f.fs, "/")
	h.assert(t, "page cache serves HIT", CacheStatus(last.Header) == "HIT", "got %q after %s; headers %v", CacheStatus(last.Header), strings.Join(seen, "; "), last.Header)
	// pre-compression: gzip from cache
	enc := "none"
	if strings.Contains(strings.ToLower(h.get(f.fs, "/", "Accept-Encoding", "gzip").Header.Get("Content-Encoding")), "gzip") {
		enc = "content-encoding: gzip"
	}
	h.check(t, "cache pre-compressed", enc, "content-encoding: gzip")

	// ---- cache correctness as real browsers see it (P-1..P-4, P-8) ----
	// One stored variant: a page warmed by one client is a HIT for every other.
	for i := 0; i < 2; i++ {
		h.get(f.fs, "/sample-page/", "Accept-Encoding", "gzip")
	}
	h.check(t, "a page warmed with gzip is a HIT for Chrome", h.cacheStatus(f.fs, chromeAE, "/sample-page/"), "HIT")
	h.check(t, "a page warmed with gzip is a HIT for Safari", h.cacheStatus(f.fs, "gzip, deflate, br", "/sample-page/"), "HIT")
	// A client without gzip still gets readable HTML from the gzip entry.
	plain := head(h.get(f.fs, "/sample-page/").Body, 200)
	h.assert(t, "client without gzip gets decompressed HTML", strings.Contains(plain, "<"), "client without gzip got unreadable bytes")
	// deflate-only must not poison the plain entry.
	h.get(f.fs, "/sample-page/", "Accept-Encoding", "deflate")
	plain = head(h.get(f.fs, "/sample-page/").Body, 200)
	h.assert(t, "deflate client did not poison the cache", strings.Contains(plain, "<"), "deflate client poisoned the cache")

	// Purge after an edit is seen by a browser-like client immediately. The
	// edit goes through PHP-FPM, as wp-admin does: wp-cli would not reach
	// FPM's object cache (separate APCu segments).
	spid := h.wp(f.fsu, f.fp, "post", "list", "--post_type=page", "--name=sample-page", "--field=ID").Trim()
	if spid == "" {
		spid = "2"
	}
	marker := fmt.Sprintf("edited-by-e2e-%d", h.pid)
	h.mustWriteAs(t, f.fp+"/cg-edit.php",
		fmt.Sprintf(`<?php require __DIR__.'/wp-load.php'; wp_update_post(['ID'=>%s,'post_content'=>'%s']); echo 'ok';`, spid, marker), f.fsu)
	h.get(f.fs, fmt.Sprintf("/cg-edit.php?run=%d", h.pid))
	h.remove(f.fp + "/cg-edit.php")
	// The URL purge the connector sends, through the API (the connector's own
	// request is asynchronous).
	h.api.post(f.api("/purge"), fmt.Sprintf(`{"urls":["https://%s/sample-page/"]}`, f.fs))
	time.Sleep(1500 * time.Millisecond) // the page-cache location's open_file_cache_valid is 1s
	// curl --compressed: decode whatever Chrome's Accept-Encoding negotiates
	// (gzip, br or zstd), which net/http cannot do for br and zstd.
	body := h.run("curl", "-sk", "--compressed", "--resolve", f.fs+":443:127.0.0.1",
		"-H", "Accept-Encoding: "+chromeAE, "https://"+f.fs+"/sample-page/").Out
	h.assert(t, "an edit is visible to a browser right after purge", strings.Contains(body, marker), "a browser still sees the page from before the edit")

	// Tracking-tagged landings are served from the cache.
	for i := 0; i < 2; i++ {
		h.get(f.fs, "/sample-page/?utm_source=e2e&utm_medium=test", "Accept-Encoding", "gzip")
	}
	h.check(t, "a utm-tagged URL is a HIT", h.cacheStatus(f.fs, "gzip", "/sample-page/?utm_source=x&fbclid=abc"), "HIT")
	h.check(t, "any other query string still bypasses", h.cacheStatus(f.fs, "gzip", "/sample-page/?p=2"), "BYPASS")

	// A response that sets a cookie is never cached or replayed.
	h.mustWriteAs(t, f.fp+"/cg-cookie.php", `<?php setcookie("cg_e2e_session", bin2hex(random_bytes(8))); echo "cookie page";`, f.fsu)
	c1 := SetCookieLine(h.get(f.fs, "/cg-cookie.php").Header, "cg_e2e_session")
	c2 := SetCookieLine(h.get(f.fs, "/cg-cookie.php").Header, "cg_e2e_session")
	h.assert(t, "a response setting a cookie is not cached", c1 != "" && c1 != c2, "a cookie was replayed from the cache (%s / %s)", c1, c2)
	h.remove(f.fp + "/cg-cookie.php")
}

func head(b []byte, n int) string {
	if len(b) > n {
		b = b[:n]
	}
	return string(b)
}

// ------------------------------------- object cache, magic login, cron --

func (f *feat) objectCacheLoginCronPurge(t *testing.T) {
	h := f.h
	h.check(t, "object-cache enable accepted", code(h.api.do("PATCH", f.api(""), `{"object_cache":true}`)), "202")
	// magic login returns a URL
	_, b := h.api.post(f.api("/wp/login"), `{}`)
	ml := jsonField(b, "url")
	h.assert(t, "magic login issues URL", ml != "", "magic login")
	// The token is in the fragment, so it is never sent in a request line
	// (and never logged); the page posts it back. It works exactly once.
	h.assert(t, "magic login token is in the URL fragment", strings.Contains(ml, "?cloudground_login=1#"), "magic login token is not in the fragment: %s", ml)
	mtok := ml
	if i := strings.LastIndex(ml, "#"); i >= 0 {
		mtok = ml[i+1:]
	}
	login := func(tok string) resp {
		return h.fetch(req{URL: "https://" + f.fs + "/?cloudground_login=1", Form: "cloudground_token=" + tok})
	}
	h.check(t, "a wrong magic token is refused", login(strings.Repeat("a", 48)).CodeStr(), "403")
	first := login(mtok)
	loc := first.Header.Get("Location") // nil Header: ""
	h.assert(t, "the magic link signs in (after a wrong attempt)", first.Code == 302 && strings.Contains(loc, "wp-admin"),
		"magic link did not sign in: %s %s", first.CodeStr(), loc)
	h.check(t, "a used magic link is refused", login(mtok).CodeStr(), "403")
	alog, _ := os.ReadFile("/var/log/cloudground/" + f.fs + "/access.log")
	leaked := mtok != "" && strings.Contains(string(alog), mtok)
	h.assert(t, "the magic token is not in the access log", !leaked, "the magic token reached the access log")

	// cron add
	h.check(t, "cron create accepted", code(h.api.post(f.api("/cron"), `{"schedule":"*/15 * * * *","command":"echo hi"}`)), "201")
	// purge
	h.check(t, "cache purge accepted", code(h.api.post(f.api("/purge"), `{}`)), "200")
}

// ------------------------------------------------- warmer (R-3.8) --

// accessLog is a site's nginx access log.
func accessLog(dom string) string { return "/var/log/cloudground/" + dom + "/access.log" }

// logOffset is where a log ends now; logSince reads what was written after
// it. nginx buffers the site logs for up to a second (flush=1s), so
// logSince waits that out first.
func logOffset(p string) int64 {
	time.Sleep(1500 * time.Millisecond)
	fi, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return fi.Size()
}

func logSince(p string, off int64) string {
	time.Sleep(2500 * time.Millisecond)
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() < off {
		off = 0 // rotated meanwhile
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return ""
	}
	b, _ := io.ReadAll(f)
	return string(b)
}

// warmSite runs the API's cache warm on a site and returns the warmer's
// requests per path, read from the site's access log.
func (h *harness) warmSite(t *testing.T, id int64, dom, label string) map[string]int {
	off := logOffset(accessLog(dom))
	_, b := h.api.post(fmt.Sprintf("/api/sites/%d/warm", id), `{}`)
	h.check(t, label+" cache warm completes", h.api.waitTask(taskID(b), 300*time.Second), "succeeded")
	return WarmerRequests(logSince(accessLog(dom), off))
}

// warmer: one request per URL, counted where nginx counts them. The old
// warmer sent a "plain" request and one or two gzip requests per URL, for a
// second cache entry that no longer exists.
func (f *feat) warmer(t *testing.T) {
	h := f.h
	h.check(t, "R-3.8: purge before warming accepted", code(h.api.post(f.api("/purge"), `{}`)), "200")
	post := URLPath(h.wp(f.fsu, f.fp, "post", "list", "--post_type=post", "--post_status=publish",
		"--field=url", "--posts_per_page=1").Trim())
	counts := h.warmSite(t, f.fid, f.fs, "R-3.8:")
	var repeated []string
	for p, n := range counts {
		if n != 1 {
			repeated = append(repeated, fmt.Sprintf("%s x%d", p, n))
		}
	}
	sort.Strings(repeated)
	h.assert(t, "R-3.8: the warmer requests each URL exactly once", len(counts) >= 3 && len(repeated) == 0,
		"warmer requests per URL: %v (repeated: %v)", counts, repeated)
	h.assert(t, "R-3.8: the warmer covers the home page, a page and a post",
		counts["/"] == 1 && counts["/sample-page/"] == 1 && post != "" && counts[post] == 1,
		"warmer requests: %v (post %q)", counts, post)
	// The single gzip request filled the one entry every client is served
	// from: nginx gunzips it for a client without gzip.
	h.check(t, "R-3.8: a warmed page is a HIT for a gzip client", h.cacheStatus(f.fs, "gzip", "/sample-page/"), "HIT")
	if post != "" {
		h.check(t, "R-3.8: a warmed page is a HIT for a client without gzip", CacheStatus(h.get(f.fs, post).Header), "HIT")
	}
}

// --------------------------------------- object cache (R-3.8) --

// objectCacheSet turns the object cache on or off and waits for the task.
func (f *feat) objectCacheSet(t *testing.T, on bool, label string) {
	h := f.h
	_, b := h.api.do("PATCH", f.api(""), fmt.Sprintf(`{"object_cache":%v}`, on))
	h.check(t, label, h.api.waitTask(taskID(b), 120*time.Second), "succeeded")
}

// objectCacheStats: the numbers come from the site's own PHP workers through
// the connector, and reading them writes nothing into the site. The old
// agent wrote cloudground-cache-stats.php into the live docroot on every
// call, from where it travelled into every release, backup and staging copy.
func (f *feat) objectCacheStats(t *testing.T) {
	h := f.h
	f.objectCacheSet(t, true, "R-3.8: object cache on before reading statistics")
	h.get(f.fs, fmt.Sprintf("/?cg-stats=%d", h.pid)) // one uncached render through the drop-in
	c, b := h.api.get(f.api("/cache-stats"))
	h.check(t, "R-3.8: object-cache statistics answer", strconv.Itoa(c), "200")
	h.check(t, "R-3.8: object-cache statistics come from APCu", jsonField(b, "backend"), "apcu")
	total := jsonField(b, "mem_total")
	h.assert(t, "R-3.8: object-cache statistics report the FPM APCu segment", total != "" && total != "0",
		"object-cache statistics without a segment size: %s", b)
	stray := h.run("find", "/srv/sites/"+f.fs+"/", "-name", "cloudground-cache-stats.php").Trim()
	h.check(t, "R-3.8: reading cache statistics writes no file into the site", stray, "")
}

// optionProbe installs cg-opt.php in the live release: it reads (and with
// set=, first updates) the cg_e2e_oc option the way the site does, through
// its object cache. Every request has a fresh query (<tag>=<pid>-<n>), so
// the page cache never answers. done removes the option and the file.
func (f *feat) optionProbe(t *testing.T, tag string) (opt func(q string) string, done func()) {
	h := f.h
	const php = `<?php require __DIR__.'/wp-load.php'; if (isset($_GET['set'])) { update_option('cg_e2e_oc', (string) $_GET['set']); } echo 'opt=' . get_option('cg_e2e_oc') . ';';`
	h.mustWriteAs(t, f.fp+"/cg-opt.php", php, f.fsu)
	n := 0
	opt = func(q string) string {
		n++
		body := string(h.get(f.fs, fmt.Sprintf("/cg-opt.php?%s=%d-%d%s", tag, h.pid, n, q)).Body)
		if i := strings.Index(body, "opt="); i >= 0 {
			v, _, _ := strings.Cut(body[i+4:], ";")
			return v
		}
		return "?" + head([]byte(body), 120)
	}
	return opt, func() {
		h.wp(f.fsu, f.fp, "option", "delete", "cg_e2e_oc")
		h.remove(f.fp + "/cg-opt.php")
	}
}

// objectCacheReenable: turning the object cache back on must not serve what
// APCu held from before it was turned off. The FPM master (and its APCu)
// survives the reload, so a value changed in the database while the cache
// was off came back stale.
func (f *feat) objectCacheReenable(t *testing.T) {
	h := f.h
	opt, done := f.optionProbe(t, "n")
	defer done()
	before, after := fmt.Sprintf("before-%d", h.pid), fmt.Sprintf("after-%d", h.pid)

	f.objectCacheSet(t, true, "R-3.8: object cache on")
	opt("&set=" + before)
	h.check(t, "R-3.8: the site reads its option through the object cache", opt(""), before)
	f.objectCacheSet(t, false, "R-3.8: object cache off")
	h.wp(f.fsu, f.fp, "option", "update", "cg_e2e_oc", after)
	h.check(t, "R-3.8: with the object cache off the site reads the database", opt(""), after)
	f.objectCacheSet(t, true, "R-3.8: object cache back on")
	h.check(t, "R-3.8: object cache back on serves no value from before it was off", opt(""), after)
}

// -------------------------------------------- block theme: purge all --

// templatePartPurge: a site-wide template object must purge the whole site.
// This intercepts the connector's outgoing request and asserts the payload
// instead of watching the cache (a full purge would also empty the cache the
// checks around it rely on).
func (f *feat) templatePartPurge(t *testing.T) {
	h := f.h
	const php = `<?php
add_filter('pre_http_request', function ($pre, $args, $url) {
    if (strpos($url, '/api/connector/purge') !== false) { echo "PAYLOAD:" . $args['body'] . "\n"; }
    return ['response' => ['code' => 200], 'body' => '{}', 'headers' => [], 'cookies' => []];
}, 10, 3);
$id = wp_insert_post(['post_type' => 'wp_template_part', 'post_status' => 'publish',
    'post_name' => 'e2e-footer', 'post_title' => 'E2E Footer', 'post_content' => '<p>a</p>']);
wp_update_post(['ID' => $id, 'post_content' => '<p>b</p>']);
wp_delete_post($id, true);
`
	su := h.api.site(f.fid).SystemUser
	file := f.fp + "/cg-e2e-purge.php"
	h.mustWriteAs(t, file, php, su)
	out := h.wp(su, f.fp, "eval-file", file).Out
	h.remove(file)
	payload := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "PAYLOAD:") {
			payload = line
			break
		}
	}
	if payload == "" {
		payload = "nothing"
	}
	h.assert(t, "template part edit purges the whole site", strings.Contains(payload, `"all":true`),
		"template part edit purges only %s (interior pages keep a stale header)", payload)
}

// -------------------------------------------------------------- staging --

// staging makes the "feature staging" fixture itself, so that the layout
// probes see production before the clone and the copy after it.
func (f *feat) staging(t *testing.T) {
	h := f.h
	if h.fixtureMade(fxFeatureStaging) {
		h.fail(t, "R-2.3 setup: the staging copy is cloned here, after the layout probe",
			"an earlier section already cloned the staging copy: the layout checks around the clone cannot run")
		return
	}
	layout := f.layoutBeforeClone(t)
	_, stgInfo, ok := h.featureStaging(t)
	if !ok {
		return
	}

	// staging is password-protected, except for the server itself
	stg := stgInfo.Domain
	f.layoutAfterClone(t, layout, stg, h.api.site(stgInfo.ID).SystemUser)
	pub := h.IP + ":443"
	h.check(t, "staging asks outsiders for a password",
		h.fetch(req{URL: "https://" + stg + "/", Addr: pub}).CodeStr(), "401")
	_, creds := h.api.get(f.api("/staging/credentials"))
	spw := jsonField(creds, "password")
	h.check(t, "staging opens with its password",
		h.fetch(req{URL: "https://" + stg + "/", Addr: pub, User: "staging:" + spw}).CodeStr(), "200")
	// Every site's PHP can reach loopback: loopback alone must not pass.
	h.check(t, "loopback without the internal key gets no staging", h.get(stg, "/").CodeStr(), "401")
	svh := "/etc/nginx/sites-enabled/" + stg + ".conf"
	_, mode := statOwnerMode(svh)
	h.check(t, "the staging vhost (it holds the key) is root-only", mode, "600")
	_, err := os.Stat(stagingAuthFile(stg))
	h.assert(t, "R-3.3 control: the staging site has its password file", err == nil, "no password file for %s: %v", stg, err)
	vh, _ := os.ReadFile(svh)
	ikey := StagingInternalKey(string(vh))
	h.check(t, "the panel's own requests pass with the internal key",
		h.get(stg, "/", "X-CloudGround-Internal", ikey).CodeStr(), "200")
	_, list := h.api.get("/api/sites")
	h.assert(t, "the API does not expose the staging password hash", !contains(list, "staging_auth_hash"),
		"the API exposes the staging password hash")
}

func (f *feat) statusEndpoints(t *testing.T) {
	h := f.h
	h.check(t, "certificate status answers", code(h.api.get(f.api("/certificate"))), "200")
	h.check(t, "alerts answer", code(h.api.get("/api/system/alerts")), "200")
	h.check(t, "the drift check answers", code(h.api.get("/api/system/drift")), "200")
	// Every WordPress site runs the connector this build ships, not the one
	// it was deployed with (upgrades re-install it).
	want := sha256File(f.fp + "/wp-content/mu-plugins/cloudground-connector.php")
	matches, _ := filepath.Glob("/srv/sites/*/current/wp-content/mu-plugins/cloudground-connector.php")
	var stale []string
	for _, m := range matches {
		if sha256File(m) != want {
			stale = append(stale, strings.TrimPrefix(m, "/srv/sites/"))
		}
	}
	h.assert(t, "every WordPress site runs the current connector", len(stale) == 0, "stale connector on: %s", strings.Join(stale, " "))

	// W3: wp-cli lists a must-use plugin with "update": false (a bool); the
	// agent's parser wanted a string and the whole list answered 502.
	c, b := h.api.get(f.api("/wp/plugins"))
	var wp struct {
		Plugins []struct{ Name, Status, Update string } `json:"plugins"`
	}
	_ = json.Unmarshal(b, &wp)
	mu := false
	for _, p := range wp.Plugins {
		mu = mu || (p.Status == "must-use" && p.Update == "none")
	}
	h.assert(t, "W3: the plugin list answers, must-use plugins included", c == 200 && mu, "HTTP %d: %s", c, head(b, 400))
}

func sha256File(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// selfSigned is `openssl req -x509 -newkey ec(P-256) -days 30 -subj /CN=<cn>
// -addext subjectAltName=DNS:<dns>` as the JSON body the API takes.
func selfSigned(cn, dns string) (string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn},
		DNSNames:              []string{dns},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(30 * 24 * time.Hour),
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return "", err
	}
	kder, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(map[string]string{
		"fullchain": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		"key":       string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kder})),
	})
	return string(body), err
}

// servedCN is the subject CN nginx presents on IP:443 for an SNI name
// (`openssl s_client -connect IP:443 -servername <name>`).
func (h *harness) servedCN(sni string) string {
	h.step("TLS handshake " + h.IP + ":443 SNI " + sni)
	conn, err := tls.DialWithDialer(&netDialer, "tcp", h.IP+":443", &tls.Config{ServerName: sni, InsecureSkipVerify: true})
	if err != nil {
		return "error: " + err.Error()
	}
	defer conn.Close()
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return ""
	}
	return certs[0].Subject.String()
}

// -------------------------------------------- custom certificate ------

func (f *feat) customCertificate(t *testing.T) {
	h := f.h
	body, err := selfSigned("cg-e2e-custom", f.fs)
	if err != nil {
		t.Logf("fixture: certificate: %v", err)
	}
	// nginx reloads gracefully: old workers finish their connections first,
	// so give the new configuration a few seconds to take every handshake.
	servedWait := func(want bool) bool {
		for end := time.Now().Add(10 * time.Second); ; time.Sleep(250 * time.Millisecond) {
			if strings.Contains(h.servedCN(f.fs), "cg-e2e-custom") == want {
				return true
			}
			if time.Now().After(end) {
				return false
			}
		}
	}
	h.check(t, "a custom certificate is accepted", code(h.api.put(f.api("/certificate/custom"), body)), "200")
	h.assert(t, "nginx serves the custom certificate", servedWait(true), "nginx does not serve the custom certificate: %s", h.servedCN(f.fs))
	// R-3.9: the status says the certificate is the operator's, which keeps
	// the renewal pass from running certbot for it. The old status had no
	// such field, and the scheduler renewed a brought certificate near
	// expiry with Let's Encrypt every 12 hours.
	_, st := h.api.get(f.api("/certificate"))
	h.check(t, "R-3.9: the certificate status marks a brought certificate as custom", jsonBool(st, "custom"), "true")
	h.check(t, "the custom certificate can be removed", code(h.api.del(f.api("/certificate/custom"))), "200")
	h.assert(t, "removing it stops serving the custom certificate", servedWait(false), "the custom certificate is still served after removal")
}

// certificateRace (R-0.4): a certificate call racing a deletion must not
// resurrect the vhost. Keep installing a certificate while the delete runs;
// every call must finish before the deletion or be refused, and nothing of
// the site may be left in nginx afterwards.
func (f *feat) certificateRace(t *testing.T) {
	h := f.h
	rs := h.dom("certrace")
	rid := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"static"}`, rs))
	if rid == 0 || h.api.waitActive(rid, 120*time.Second) != "active" {
		h.fail(t, "race: the site for the certificate/delete race did not provision", "race: the site for the certificate/delete race did not provision")
		return
	}
	rbody, err := selfSigned(rs, rs)
	if err != nil {
		t.Logf("fixture: certificate: %v", err)
	}
	h.check(t, "race: the delete is accepted", h.api.deleteSite(rid), "202")
	var codes []string
	for i := 0; i < 50; i++ {
		rc := code(h.api.put(fmt.Sprintf("/api/sites/%d/certificate/custom", rid), rbody))
		codes = append(codes, rc)
		if rc == "404" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	h.check(t, "race: teardown completes", h.api.teardownGone(rid), "yes")
	nt := h.run("nginx", "-T")
	h.assert(t, "a certificate racing the delete leaves nothing of "+rs+" in nginx", !strings.Contains(nt.Out, rs),
		"a custom certificate racing the delete put %s back into nginx (nginx -T still has it; PUT codes: %s)", rs, strings.Join(codes, " "))
	vh := "/etc/nginx/sites-enabled/" + rs + ".conf"
	back := exists(vh)
	h.assert(t, "the deleted site's vhost stays gone", !back,
		"the deleted site's vhost %s was rendered back (PUT codes: %s)", vh, strings.Join(codes, " "))
	if back { // do not leave it for the checks below
		h.remove(vh)
		h.run("nginx", "-s", "reload")
	}
}

// customCertificateDelete (R-1.2): deleting a site removes the certificate
// the operator brought. CustomCertDir/<domain> (key included) outlived the
// site, and a site created later under the same domain served it again. The
// directory is checked to exist before the delete, so this cannot pass on a
// certificate never written.
func (f *feat) customCertificateDelete(t *testing.T) {
	h := f.h
	cs := h.dom("custcert")
	ccd := "/etc/cloudground/certs/custom/" + cs
	cid := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"static"}`, cs))
	if cid == 0 || h.api.waitActive(cid, 120*time.Second) != "active" {
		h.fail(t, "R-1.2: the site for the custom-certificate delete did not provision", "R-1.2: the site for the custom-certificate delete did not provision")
		return
	}
	cbody, err := selfSigned(cs, cs)
	if err != nil {
		t.Logf("fixture: certificate: %v", err)
	}
	h.check(t, "R-1.2: a custom certificate is installed on "+cs, code(h.api.put(fmt.Sprintf("/api/sites/%d/certificate/custom", cid), cbody)), "200")
	fi, err := os.Stat(ccd + "/privkey.pem")
	h.assert(t, "R-1.2: its key is on disk before the delete", err == nil && fi.Mode().IsRegular(), "R-1.2: %s/privkey.pem missing after the PUT", ccd)

	// W3: back to Let's Encrypt requests the certificate for the site's
	// current names (aliases added over the custom certificate were never
	// issued for). Without an ACME e-mail on this box an address Let's
	// Encrypt refuses stands in: the request is what is checked.
	_, settings := h.api.get("/api/settings")
	if jsonField(settings, "acme_email") == "" {
		h.api.put("/api/settings", map[string]string{"acme_email": "cg-e2e@example.invalid"})
		defer h.undo("clear the stand-in ACME e-mail", func() { h.api.put("/api/settings", map[string]string{"acme_email": ""}) })()
	}
	c, back := h.api.del(fmt.Sprintf("/api/sites/%d/certificate/custom", cid))
	var answer struct {
		Task struct {
			ID   int64  `json:"id"`
			Kind string `json:"kind"`
		} `json:"task"`
	}
	_ = json.Unmarshal(back, &answer)
	h.assert(t, "W3: back to Let's Encrypt queues the certificate for the site's names",
		c == 200 && answer.Task.Kind == "certificate.issue", "HTTP %d: %s", c, head(back, 400))
	if answer.Task.ID != 0 {
		h.api.waitTask(answer.Task.ID, 180*time.Second) // its outcome is Let's Encrypt's
	}
	// The site's delete must take a custom certificate with it: put it back.
	h.check(t, "R-1.2: the custom certificate is installed again for the delete", code(h.api.put(fmt.Sprintf("/api/sites/%d/certificate/custom", cid), cbody)), "200")
	h.check(t, "R-1.2: the delete is accepted", h.api.deleteSite(cid), "202")
	h.check(t, "R-1.2: teardown completes", h.api.teardownGone(cid), "yes")
	h.assert(t, "R-1.2: the deleted site's custom certificate directory is gone", !exists(ccd), "R-1.2: %s outlived the site", ccd)
}

// ---------------------------------------------------------------- SFTP --

func (h *harness) sshPort() string {
	return SSHDPort(h.run("sshd", "-T").Out, "22")
}

func (f *feat) sftpSet(body string) string {
	return code(f.h.api.post(f.api("/sftp"), body))
}

// sftp: SFTP with an SSH key; the site cannot touch its own key file.
func (f *feat) sftp(t *testing.T) {
	h := f.h
	kdir, err := os.MkdirTemp("", "cg-e2e-key-")
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	cleanK := h.undo("remove SSH key fixture", func() { _ = os.RemoveAll(kdir) })
	h.run("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "cg-e2e", "-f", kdir+"/k")
	pub := readTrim(kdir + "/k.pub")
	kbody, _ := json.Marshal(map[string]string{"public_key": strings.TrimSpace(pub)})
	h.check(t, "an SSH key is added", code(h.api.post(f.api("/ssh-keys"), kbody)), "201")
	// R-3.1: the old agent "unlocked" the account for keys with passwd -d,
	// which left the site user with an empty password while SFTP was off.
	shadow, _ := os.ReadFile("/etc/shadow")
	spw, sfound := ShadowPassword(string(shadow), f.fsu)
	h.assert(t, "R-3.1: adding a key while SFTP is off leaves no empty password", sfound && spw != "",
		"%s's password field is %q (in /etc/shadow: %v)", f.fsu, spw, sfound)
	port := h.sshPort()
	keyLogin := func() bool {
		return h.runIn("ls\n", "sftp", "-q", "-b", "-", "-i", kdir+"/k", "-P", port,
			"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
			f.fsu+"@127.0.0.1").OK()
	}
	// SFTP is off on a new site: its keys must not let anyone in either.
	h.assert(t, "SFTP off refuses the site's keys", !keyLogin(), "a key signs in while SFTP is off")
	h.check(t, "SFTP can be turned on", f.sftpSet(`{"enable":true,"password":"cg-e2e-sftp-pass-1"}`), "200")
	h.check(t, "R-2.4: the SFTP password is the one set", h.shadowVerifies(f.fsu, "cg-e2e-sftp-pass-1"), "ok")
	h.assert(t, "R-2.4: SFTP on takes the user out of the deny group", !h.inGroup(f.fsu, "cg-sftp-off"),
		"%s is still in cg-sftp-off", f.fsu)
	h.assert(t, "SFTP signs in with the key", keyLogin(), "SFTP refused the key")
	f.sftpKeysOnly(t, kdir, port, keyLogin)
	h.check(t, "SFTP can be turned off", f.sftpSet(`{"enable":false}`), "200")
	h.assert(t, "R-2.4: SFTP off puts the user in the deny group", h.inGroup(f.fsu, "cg-sftp-off"),
		"%s is not in cg-sftp-off", f.fsu)
	h.assert(t, "turning SFTP off refuses the key", !keyLogin(), "the key still signs in after SFTP was turned off")
	replaced := h.run("runuser", "-u", f.fsu, "--", "ln", "-sf", "/etc/passwd", "/srv/sites/"+f.fs+"/.ssh/authorized_keys").OK()
	h.assert(t, "the site user cannot replace its authorized_keys", !replaced, "the site user replaced its authorized_keys")
	fingerprints := func() map[string]string { // label -> fingerprint
		var keys struct {
			Keys []struct {
				Fingerprint string `json:"fingerprint"`
				Label       string `json:"label"`
			} `json:"keys"`
		}
		_, b := h.api.get(f.api("/ssh-keys"))
		_ = json.Unmarshal(b, &keys)
		m := map[string]string{}
		for _, k := range keys.Keys {
			m[k.Label] = k.Fingerprint
		}
		return m
	}

	// R-3.1: the panel's fingerprint is the one ssh-keygen (and sshd's log)
	// print. The old agent used the URL-safe base64 alphabet, so a key whose
	// hash has a "/" or "+" showed a fingerprint matching nothing. That "/"
	// must also survive the DELETE URL.
	slash := SlashKey("cg-e2e-fp")
	_ = os.WriteFile(kdir+"/slash.pub", []byte(slash+"\n"), 0o600)
	want := KeygenFingerprint(h.run("ssh-keygen", "-lf", kdir+"/slash.pub").Out)
	sbody, _ := json.Marshal(map[string]string{"public_key": slash})
	h.check(t, "control: a key whose fingerprint has a / is added", code(h.api.post(f.api("/ssh-keys"), sbody)), "201")
	got := fingerprints()["cg-e2e-fp"]
	h.assert(t, "R-3.1: the panel shows a key's fingerprint as ssh-keygen prints it", want != "" && got == want,
		"panel %q, ssh-keygen %q", got, want)
	h.check(t, "R-3.1: a key whose fingerprint has a / is removed by it",
		code(h.api.del(f.api("/ssh-keys/%s", QuoteAll(want)))), "200")
	_, still := fingerprints()["cg-e2e-fp"]
	h.assert(t, "R-3.1: the removed key is gone from the list", !still, "the key is still listed")

	if fp := fingerprints()["cg-e2e"]; fp != "" {
		h.api.del(f.api("/ssh-keys/%s", QuoteAll(fp)))
	}
	cleanK()
}

// sftpKeysOnly (#47): a login set to keys only refuses its password, which
// sshd does not even offer, and still takes the key; a password login (the
// control) signs in with it; a public key sent with no keys_only makes a
// keys-only login. Leaves SFTP on with a password.
func (f *feat) sftpKeysOnly(t *testing.T, kdir, port string, keyLogin func() bool) {
	h := f.h
	const pw = "cg-e2e-sftp-pass-1"
	askpass := kdir + "/askpass"
	if err := os.WriteFile(askpass, []byte("#!/bin/sh\necho '"+pw+"'\n"), 0o700); err != nil {
		h.fail(t, "#47 control: an askpass helper", "%v", err)
		return
	}
	// Not -b, which makes sftp's ssh BatchMode and so never send a
	// password; not -q, which hides the methods sshd offers.
	passwordLogin := func() result {
		return h.runIn("ls\n", "env", "SSH_ASKPASS="+askpass, "SSH_ASKPASS_REQUIRE=force", "DISPLAY=:0",
			"sftp", "-P", port, "-o", "PubkeyAuthentication=no",
			"-o", "PreferredAuthentications=password,keyboard-interactive", "-o", "NumberOfPasswordPrompts=1",
			"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", f.fsu+"@127.0.0.1")
	}
	r := passwordLogin()
	h.assert(t, "#47 control: a password login signs in with its password", r.OK(), "sftp: %s", r)

	h.check(t, "#47: SFTP can be set to keys only", f.sftpSet(`{"enable":true,"keys_only":true}`), "200")
	s := h.api.site(f.fid)
	h.check(t, "#47: keys only is stored", PyBool(s.Config.SFTPKeysOnly), "True")
	r = passwordLogin()
	h.assert(t, "#47: a keys-only login refuses its old password", !r.OK(), "the password still signs in")
	h.assert(t, "#47: sshd does not offer a keys-only login a password", strings.Contains(r.Err, "Permission denied (publickey)"),
		"sftp: %s", r)
	shadow, _ := os.ReadFile("/etc/shadow")
	spw, _ := ShadowPassword(string(shadow), f.fsu)
	h.assert(t, "#47: a keys-only login's password is locked", strings.HasPrefix(spw, "!"), "password field %.3q…", spw)
	h.assert(t, "#47: a keys-only login takes its key", keyLogin(), "SFTP refused the key")
	h.check(t, "#47: a keys-only request with a password is refused",
		f.sftpSet(`{"enable":true,"keys_only":true,"password":"cg-e2e-sftp-pass-9"}`), "400")

	// A key given with no keys_only: keys only, that key added and signing in.
	h.run("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "cg-e2e-ko", "-f", kdir+"/k2")
	body, _ := json.Marshal(map[string]any{"enable": true, "public_key": readTrim(kdir + "/k2.pub")})
	h.check(t, "#47: SFTP turned on with a key", f.sftpSet(string(body)), "200")
	s = h.api.site(f.fid)
	h.check(t, "#47: a login that comes with a key takes keys only", PyBool(s.Config.SFTPKeysOnly), "True")
	k2 := h.runIn("ls\n", "sftp", "-q", "-b", "-", "-i", kdir+"/k2", "-P", port,
		"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", f.fsu+"@127.0.0.1")
	h.assert(t, "#47: the key sent with the request signs in", k2.OK(), "sftp: %s", k2)

	h.check(t, "#47: back to a password", f.sftpSet(`{"enable":true,"keys_only":false,"password":"`+pw+`"}`), "200")
	r = passwordLogin()
	h.assert(t, "#47: a password login signs in again", r.OK(), "sftp: %s", r)
}

// concurrentChanges (R-0.5): two changes to one site at once both stick.
// The object-cache task wrote back the whole site as its request had read
// it, so an SFTP change saved while the agent was busy was undone.
func (f *feat) concurrentChanges(t *testing.T) {
	h := f.h
	// The change sent below turns the object cache off: it must be on for
	// "the drop-in is gone" to mean anything.
	f.ensureObjectCache(t, "the concurrent changes")
	var ocBody []byte
	var sftpCode string
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, ocBody = h.api.do("PATCH", f.api(""), `{"object_cache":false}`) }()
	go func() { defer wg.Done(); sftpCode = f.sftpSet(`{"enable":true,"password":"cg-e2e-sftp-pass-2"}`) }()
	wg.Wait()
	tid := taskID(ocBody)
	got := ""
	if tid != 0 {
		got = h.api.waitTask(tid, 180*time.Second)
	}
	h.check(t, "R-0.5: object-cache change sent alongside an SFTP change completes", got, "succeeded")
	h.check(t, "R-0.5: SFTP change sent alongside an object-cache change is accepted", sftpCode, "200")
	s := h.api.site(f.fid)
	h.check(t, "R-0.5: object cache stays off after concurrent changes (stored)", PyBool(s.Config.ObjectCache), "False")
	h.check(t, "R-0.5: SFTP stays on after concurrent changes (stored)", PyBool(s.Config.SFTPEnabled), "True")
	h.assert(t, "R-0.5: the object-cache drop-in is gone", !exists(f.fp+"/wp-content/object-cache.php"),
		"R-0.5: the object-cache drop-in is still installed after it was turned off")
	// Put the site back as the rest of the suite expects it.
	h.check(t, "R-0.5: SFTP can be turned off again", f.sftpSet(`{"enable":false}`), "200")
	_, b := h.api.do("PATCH", f.api(""), `{"object_cache":true}`)
	got = ""
	if tid := taskID(b); tid != 0 {
		got = h.api.waitTask(tid, 180*time.Second)
	}
	h.check(t, "R-0.5: object cache can be turned back on", got, "succeeded")
	s = h.api.site(f.fid)
	h.check(t, "R-0.5: SFTP off is stored", PyBool(s.Config.SFTPEnabled), "False")
	h.check(t, "R-0.5: object cache on is stored", PyBool(s.Config.ObjectCache), "True")
}

// ---------------------------------------------- logs and slow requests --

func (f *feat) phpErrorLog(t *testing.T) {
	h := f.h
	marker := fmt.Sprintf("cg-e2e-php-error-%d", h.pid)
	h.mustWriteAs(t, f.fp+"/cg-err.php", fmt.Sprintf(`<?php error_log('%s'); echo 'logged';`, marker), f.fsu)
	h.get(f.fs, fmt.Sprintf("/cg-err.php?x=%d", h.pid))
	logb, _ := os.ReadFile("/var/log/cloudground/" + f.fs + "/php-error.log")
	h.assert(t, "PHP errors reach the site's php-error.log", strings.Contains(string(logb), marker), "PHP errors are lost (php-error.log not written)")
	h.remove(f.fp + "/cg-err.php")
}

// slowRequest (R-0.8): a raised max_execution_seconds is honoured end to
// end. A request that runs 130 s must complete once the limit is 300.
func (f *feat) slowRequest(t *testing.T) {
	h := f.h
	_, b := h.api.do("PATCH", f.api(""), `{"max_execution_seconds":300}`)
	got := ""
	if pt := taskID(b); pt != 0 {
		got = h.api.waitTask(pt, 120*time.Second)
	}
	h.check(t, "max_execution_seconds 300 is applied", got, "succeeded")
	// Settle a site-count reconcile still pending from the sections before
	// (sites they created or deleted): the scheduler's own tick would
	// otherwise resize this site's pool in the middle of the request, and
	// the master's reload ends a running request after
	// process_control_timeout (30 s; 10 s before R-5.2). A build without the route serves the
	// UI's page here, and nothing happens.
	h.api.runMaintenance()
	h.mustWriteAs(t, f.fp+"/cg-slow.php", `<?php sleep(130); echo 'cg-slow-ok';`, f.fsu)
	r := h.fetch(req{URL: fmt.Sprintf("https://%s/cg-slow.php?x=%d", f.fs, h.pid), Timeout: 200 * time.Second})
	slow := fmt.Sprintf("%s %.3f", r.CodeStr(), r.Duration.Seconds())
	h.assert(t, "a 130s PHP request completes under a 300s limit", r.Code == 200 && contains(r.Body, "cg-slow-ok"),
		"a 130s PHP request under max_execution_seconds=300 did not complete: HTTP/time '%s' (502/504 near 120s = FPM request_terminate_timeout or nginx fastcgi_read_timeout still 120s)", slow)
	h.remove(f.fp + "/cg-slow.php")
}

func (f *feat) nginxReopen(t *testing.T) {
	h := f.h
	const elog = "/var/log/nginx/error.log"
	before, _ := os.ReadFile(elog)
	n := strings.Count(string(before), "\n")
	h.run("nginx", "-s", "reopen")
	time.Sleep(time.Second)
	after, _ := os.ReadFile(elog)
	lines := strings.Split(string(after), "\n")
	tail := ""
	if n < len(lines) {
		tail = strings.Join(lines[n:], "\n")
	}
	h.assert(t, "nginx reopens the site logs", !strings.Contains(tail, "Permission denied"), "nginx cannot reopen the site logs")
}

// cron: the site's cron.d file drives WordPress's scheduled events, through the
// sandboxed runner (R-4.5); the site user has no crontab of its own.
func (f *feat) cron(t *testing.T) {
	h := f.h
	ct := readTrim("/etc/cron.d/cloudground-" + f.fsu)
	h.assert(t, "site crontab runs WordPress's due events", strings.HasSuffix(ct, " root /usr/local/bin/cg-site-cron "+f.fsu+" wp") || strings.Contains(ct, " root /usr/local/bin/cg-site-cron "+f.fsu+" wp\n"), "site cron file has no WordPress line: %q", ct)
	h.assert(t, "R-4.5: the site user has no crontab of its own", !h.run("crontab", "-l", "-u", f.fsu).OK(), "crontab -u %s -l answers", f.fsu)
	h.check(t, "wp-cron is off in requests", h.wp(f.fsu, f.fp, "config", "get", "DISABLE_WP_CRON").Trim(), "1")
}

// -------------------------------------------------------------- safe push --

// safePush: Performance Guard gates promotion. Identical code must pass and
// be promoted; staging made deliberately slower must be blocked with
// production untouched.
// safePush runs on the feature site and its staging copy stg.
func (f *feat) safePush(t *testing.T, stg siteInfo) {
	h := f.h
	stgs := stg.Domain
	push := func(body string) string {
		_, b := h.api.post(f.api("/safe-push"), body)
		return h.api.waitTask(taskID(b), 900*time.Second)
	}
	h.check(t, "safe push of identical code passes the guard", push(`{"paths":["/sample-page/"]}`), "succeeded")
	stgu := h.api.site(stg.ID).SystemUser
	slow := "/srv/sites/" + stgs + "/current/wp-content/mu-plugins/cg-e2e-slow.php"
	h.mustWriteAs(t, slow, `<?php usleep(400000);`, stgu)
	cleanSlow := h.undo("remove staging slow mu-plugin", func() { _ = os.Remove(slow) })
	before, _ := os.Readlink("/srv/sites/" + f.fs + "/current")
	h.check(t, "safe push of a 400 ms slower staging is blocked", push(`{"paths":["/sample-page/"]}`), "failed")
	after, _ := os.Readlink("/srv/sites/" + f.fs + "/current")
	h.check(t, "a blocked push leaves production untouched", after, before)
	f.blockedGuardReport(t)
	_, diff := h.api.get(f.api("/staging/diff"))
	h.assert(t, "staging diff shows the file added on staging", contains(diff, "wp-content/mu-plugins/cg-e2e-slow.php"),
		"staging diff misses the added file: %s", head(diff, 200))
	cleanSlow()
	f.heavyPageAA(t, stg, push)

	// Selective push: only the chosen folder goes live.
	themesDir := "/srv/sites/" + stgs + "/current/wp-content/themes/"
	ents, _ := os.ReadDir(themesDir)
	var themes []string
	for _, e := range ents {
		if e.IsDir() {
			themes = append(themes, e.Name())
		}
	}
	sort.Strings(themes)
	theme := ""
	if len(themes) > 0 {
		theme = themes[0]
	}
	h.mustWriteAs(t, themesDir+theme+"/cg-e2e-marker.txt", fmt.Sprintf("theme-%d\n", h.pid), stgu)
	h.mustWriteAs(t, "/srv/sites/"+stgs+"/current/cg-e2e-other.txt", fmt.Sprintf("other-%d\n", h.pid), stgu)
	h.check(t, "selective push passes the guard",
		push(fmt.Sprintf(`{"paths":["/sample-page/"],"only":["wp-content/themes/%s"]}`, theme)), "succeeded")
	h.check(t, "the chosen folder went live",
		readTrim("/srv/sites/"+f.fs+"/current/wp-content/themes/"+theme+"/cg-e2e-marker.txt"), fmt.Sprintf("theme-%d", h.pid))
	h.assert(t, "nothing outside the chosen folder went live", !exists("/srv/sites/"+f.fs+"/current/cg-e2e-other.txt"),
		"a file outside the chosen folder went live")
	// Recreate staging from production. W3: with an ACME e-mail set, the
	// clone requests staging.<domain>'s certificate, as a new site does (it
	// used to stay on the default certificate). Without one on this box an
	// address Let's Encrypt refuses stands in: the request is what is
	// checked, and its failure must not fail the clone.
	_, settings := h.api.get("/api/settings")
	if jsonField(settings, "acme_email") == "" {
		h.api.put("/api/settings", map[string]string{"acme_email": "cg-e2e@example.invalid"})
		defer h.undo("clear the stand-in ACME e-mail", func() { h.api.put("/api/settings", map[string]string{"acme_email": ""}) })()
	}
	_, b := h.api.post(f.api("/staging/recreate"), `{}`)
	rid := taskID(b)
	h.check(t, "staging is recreated from production", h.api.waitTask(rid, 600*time.Second), "succeeded")
	_, tb := h.api.get(fmt.Sprintf("/api/tasks/%d", rid))
	h.assert(t, "W3: recreating staging requests the staging name's certificate",
		strings.Contains(jsonField(tb, "log"), "Requesting Let's Encrypt certificate for "+stgs), "task: %s", head(tb, 600))
	h.assert(t, "recreated staging starts from production", !exists("/srv/sites/"+stgs+"/current/cg-e2e-other.txt"),
		"recreated staging kept the old staging's files")
}

// blockedGuardReport (R-4.5): the report stored with the blocked push comes
// from the one paired engine (internal/guard/SPEC.md): it judged the
// connector's render time on uncached pairs, under the lower-bound policy.
func (f *feat) blockedGuardReport(t *testing.T) {
	h := f.h
	_, b := h.api.get(f.api("/deployments"))
	var deps []struct {
		Status    string `json:"status"`
		GuardJSON string `json:"guard_json"`
	}
	_ = json.Unmarshal(b, &deps)
	var rep struct {
		Verdict    string `json:"verdict"`
		Thresholds struct {
			MaxSlowdownPct *float64 `json:"max_slowdown_pct"`
		} `json:"thresholds"`
		Paths []struct {
			CILow       float64 `json:"ci_low"`
			Pairs       int     `json:"pairs"`
			ServerTimed bool    `json:"server_timed"`
		} `json:"paths"`
	}
	if len(deps) > 0 && deps[0].Status == "blocked" {
		_ = json.Unmarshal([]byte(deps[0].GuardJSON), &rep)
	}
	h.check(t, "R-4.5: the newest deployment is the blocked push", rep.Verdict, "block")
	h.assert(t, "R-4.5: the report states the lower-bound slowdown policy",
		rep.Thresholds.MaxSlowdownPct != nil && *rep.Thresholds.MaxSlowdownPct == 10, "thresholds: %s", head([]byte(deps0(deps)), 300))
	ok := len(rep.Paths) == 1 && rep.Paths[0].ServerTimed && rep.Paths[0].Pairs >= 20 && rep.Paths[0].CILow > 1.10
	h.assert(t, "R-4.5: the block rests on the connector's render time, its interval above +10%", ok, "paths: %+v", rep.Paths)
}

// heavyPageAA (R-4.5): identical code on a heavy page (hundreds of blocks
// and a list of generated posts, the same on both sides) must pass the guard
// with neither a block nor a warning: the systematic gap between two copies
// of a site has to stay under the lower-bound threshold.
func (f *feat) heavyPageAA(t *testing.T, stg siteInfo, push func(string) string) {
	h := f.h
	stgu := h.api.site(stg.ID).SystemUser
	stgp := "/srv/sites/" + stg.Domain + "/current"
	slug := fmt.Sprintf("cg-e2e-heavy-%d", h.pid)
	var b strings.Builder
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&b, "<!-- wp:heading --><h2>Section %d</h2><!-- /wp:heading --><!-- wp:paragraph --><p>Paragraph %d, some text for the block parser to render.</p><!-- /wp:paragraph -->", i, i)
	}
	b.WriteString(`<!-- wp:latest-posts {"postsToShow":60,"displayPostContent":true} /-->`)
	var cleanups []func()
	for _, side := range []struct{ u, p string }{{f.fsu, f.fp}, {stgu, stgp}} {
		ids := h.wp(side.u, side.p, "post", "generate", "--count=60", "--format=ids").Trim()
		page := h.wp(side.u, side.p, "post", "create", "--post_type=page", "--post_status=publish", "--post_name="+slug,
			"--post_title=Heavy", "--post_content="+b.String(), "--porcelain").Trim()
		u, p, del := side.u, side.p, append([]string{"post", "delete", "--force", page}, strings.Fields(ids)...)
		cleanups = append(cleanups, h.undo("delete the heavy page and its posts", func() { h.wp(u, p, del...) }))
	}
	// Unforced, a warn or an inconclusive verdict fails the push as a block
	// does: only a pass succeeds.
	h.check(t, "R-4.5: identical code on a heavy page passes the guard without a warning",
		push(fmt.Sprintf(`{"paths":["/%s/"]}`, slug)), "succeeded")
	for _, c := range cleanups {
		c()
	}
}

func deps0[T any](deps []T) string {
	if len(deps) == 0 {
		return ""
	}
	b, _ := json.Marshal(deps[0])
	return string(b)
}

// stagingPush: a database push as production's own PHP sees it (R-3.3).
// The push policy is the agent's and knows a store by its tables; the
// pushed rows are served at once (production's object cache is flushed);
// production keeps its own indexing setting and gets its own addresses
// back, the JSON-escaped ones included.
func (f *feat) stagingPush(t *testing.T) {
	h := f.h
	si := h.api.stagingOf(f.fid)
	if si.ID == 0 {
		h.fail(t, "R-3.3: no staging site for the database push checks", "R-3.3: no staging site for the database push checks")
		return
	}
	stg := h.api.site(si.ID)
	stgu, sdb := stg.SystemUser, stg.Config.Database.Name
	fdb := h.api.site(f.fid).Config.Database.Name
	sp := "/srv/sites/" + si.Domain + "/current"
	push := func(tables string) (int, []byte) {
		return h.api.post(f.api("/staging/database"), fmt.Sprintf(`{"tables":[%s],"confirm":%q}`, tables, f.fs))
	}

	// WooCommerce installed by hand on a WordPress site: its order table
	// in both databases. The old policy went by the site type and let it go.
	for _, d := range []string{fdb, sdb} {
		h.run("mariadb", "-e", "CREATE TABLE IF NOT EXISTS `"+d+"`.wp_wc_orders (id BIGINT PRIMARY KEY)")
	}
	dropOrders := h.undo("drop the planted WooCommerce tables", func() {
		for _, d := range []string{fdb, sdb} {
			h.run("mariadb", "-e", "DROP TABLE IF EXISTS `"+d+"`.wp_wc_orders")
		}
	})
	h.check(t, "R-3.3: a store installed by hand cannot push its orders table", code(push(`"wp_wc_orders"`)), "400")
	dropOrders()

	// An options push, read through production's PHP-FPM, which holds
	// alloptions in APCu once it has served a request.
	h.mustWriteAs(t, f.fp+"/cg-name.php", `<?php require __DIR__.'/wp-load.php'; echo 'name=' . get_option('blogname') . ';';`, f.fsu)
	defer h.remove(f.fp + "/cg-name.php")
	n := 0
	name := func() string {
		n++
		body := string(h.get(f.fs, fmt.Sprintf("/cg-name.php?n=%d-%d", h.pid, n)).Body)
		if i := strings.Index(body, "name="); i >= 0 {
			v, _, _ := strings.Cut(body[i+5:], ";")
			return v
		}
		return "?" + head([]byte(body), 120)
	}
	orig := h.wp(f.fsu, f.fp, "option", "get", "blogname").Trim()
	defer h.wp(f.fsu, f.fp, "option", "update", "blogname", orig)
	h.assert(t, "R-3.3 control: production's object cache is on", exists(f.fp+"/wp-content/object-cache.php"),
		"production has no object-cache drop-in: the next check would not tell")
	h.check(t, "R-3.3 control: production's PHP serves its own site name", name(), orig)
	h.check(t, "R-3.3 control: production is indexable before the push", h.wp(f.fsu, f.fp, "option", "get", "blog_public").Trim(), "1")
	h.check(t, "R-3.3 control: staging is not indexable", h.wp(stgu, sp, "option", "get", "blog_public").Trim(), "0")
	marker := fmt.Sprintf("cg-e2e-pushed-%d", h.pid)
	h.wp(stgu, sp, "option", "update", "blogname", marker)
	// In SQL, \\ is one backslash: the row holds https:\/\/<staging>\/x.
	h.run("mariadb", "-e", "INSERT INTO `"+sdb+"`.wp_options (option_name, option_value, autoload) VALUES ('cg_e2e_url', 'https:\\\\/\\\\/"+si.Domain+"\\\\/x', 'no')")
	defer h.wp(f.fsu, f.fp, "option", "delete", "cg_e2e_url")

	_, b := push(`"wp_options"`)
	h.check(t, "R-3.3: staging's options push succeeds", h.api.waitTask(taskID(b), 600*time.Second), "succeeded")
	h.check(t, "R-3.3: production's PHP serves the pushed option at once (object cache flushed)", name(), marker)
	h.check(t, "R-3.3: production stays indexable after an options push (blog_public)", h.wp(f.fsu, f.fp, "option", "get", "blog_public").Trim(), "1")
	h.check(t, "R-3.3: the pushed home address is production's", h.wp(f.fsu, f.fp, "option", "get", "home").Trim(), "https://"+f.fs)
	h.check(t, "R-3.3: a JSON-escaped staging address is rewritten to production's", h.wp(f.fsu, f.fp, "option", "get", "cg_e2e_url").Trim(),
		`https:\/\/`+f.fs+`\/x`)
}

// ---------------------------------------------------------------- backups --

type backupRow struct {
	ID           int64  `json:"id"`
	Kind         string `json:"kind"`
	VerifyStatus string `json:"verify_status"`
	SnapshotID   string `json:"snapshot_id"`
}

func (f *feat) backupsList() []backupRow {
	var rows []backupRow
	_, b := f.h.api.get(f.api("/backups"))
	_ = json.Unmarshal(b, &rows)
	return rows
}

// newest returns the backup with the highest id among those keep accepts.
func newest(rows []backupRow, keep func(backupRow) bool) (backupRow, bool) {
	var best backupRow
	found := false
	for _, r := range rows {
		if keep(r) && (!found || r.ID > best.ID) {
			best, found = r, true
		}
	}
	return best, found
}

// backups: snapshot, verified restore, in-place restore, the R-0.2/0.3/0.6/
// 0.9/0.10 regressions. "Verified restores" is a core claim.
func (f *feat) backups(t *testing.T) {
	h := f.h
	brepo := fmt.Sprintf("/var/backups/cloudground/e2e-%d", h.pid)
	bpwRaw := make([]byte, 24)
	_, _ = rand.Read(bpwRaw)
	bpw := strings.NewReplacer("/", "", "+", "", "=", "").Replace(b64(bpwRaw))
	_, sb := h.api.get("/api/settings")
	prevRepo := jsonField(sb, "backup_repository")
	// Leave the box as it was: a repository the test deletes must not stay
	// configured, or every scheduled backup (and its restore test) fails.
	restoreSettings := h.undo("restore the backup repository setting", func() {
		h.api.relogin()
		if prevRepo == "" {
			h.api.put("/api/settings", `{"backup_repository":"","backup_password":""}`)
		} else {
			h.root.Logf("(note: this run replaced the configured backup repository %s; set it again in Settings)", prevRepo)
		}
		_ = os.RemoveAll(brepo)
	})
	setBody, _ := json.Marshal(map[string]string{"backup_repository": brepo, "backup_password": bpw})
	h.check(t, "backup repository accepted", code(h.api.put("/api/settings", setBody)), "200")
	// R-2.1: the password just saved is sealed in the secret store, not
	// written to the database file in the clear. bpw is random, so finding
	// it anywhere in the files is the product's doing. The WAL is where a
	// fresh write lands first.
	plain := false
	for _, p := range []string{"/var/lib/cloudground/state.db", "/var/lib/cloudground/state.db-wal"} {
		if b, err := os.ReadFile(p); err == nil && contains(b, bpw) {
			plain = true
			h.root.Logf("%s contains the backup password in the clear", p)
		}
	}
	h.assert(t, "R-2.1: the backup password is not in state.db in plaintext", !plain, "R-2.1: the backup password is stored in plaintext")
	// The same for the site's own database password, which the panel
	// generated and the site's wp-config.php holds.
	if dbpw := strings.TrimSpace(h.wp(f.fsu, f.fp, "config", "get", "DB_PASSWORD").Out); len(dbpw) >= 16 {
		leaked := false
		for _, p := range []string{"/var/lib/cloudground/state.db", "/var/lib/cloudground/state.db-wal"} {
			if b, err := os.ReadFile(p); err == nil && contains(b, dbpw) {
				leaked = true
			}
		}
		h.assert(t, "R-2.1: the site's database password is not in state.db in plaintext", !leaked, "R-2.1: the site's database password is stored in plaintext")
	} else {
		h.fail(t, "R-2.1: the site's database password is not in state.db in plaintext", "fixture: could not read DB_PASSWORD from wp-config.php (%q)", dbpw)
	}
	_, sb2 := h.api.get("/api/settings")
	h.check(t, "R-2.1: settings show the backup password masked", jsonField(sb2, "backup_password"), "••••••••")
	o, m := statOwnerMode("/var/lib/cloudground/secret.key")
	h.check(t, "R-2.1: the secret key is the API user's alone", strings.TrimSpace(o+" "+m), "cloudground 600")
	h.wp(f.fsu, f.fp, "option", "update", "cg_e2e_marker", "before-backup")
	h.mustWriteAs(t, f.fp+"/cg-marker.txt", "before\n", f.fsu)

	// R-0.2: while this backup runs, the site user keeps replacing
	// logs/db-latest.sql with a dump of their own (the real data plus a
	// marker table); the restore below must not bring the marker table in.
	fdb := h.api.site(f.fid).Config.Database.Name
	flogs := "/srv/sites/" + f.fs + "/logs"
	dump := h.run("mariadb-dump", "--single-transaction", fdb)
	if err := h.writeAs(flogs+"/.cg-plant.sql", dump.Out+"CREATE TABLE cg_e2e_planted (id int);\n", f.fsu, 0o600); err != nil {
		t.Logf("fixture: planted dump: %v", err)
	}
	// The copy is made in tmp/ (which backups leave out, R-4.6) and renamed
	// over: a temporary file in logs/ that vanished while restic read the
	// directory would fail the backup (exit 3) for the test's own doing.
	ftmp := "/srv/sites/" + f.fs + "/tmp"
	plant := fmt.Sprintf("while :; do cp '%[1]s/.cg-plant.sql' '%[2]s/.cg-plant.tmp' && mv -f '%[2]s/.cg-plant.tmp' '%[1]s/db-latest.sql'; sleep 0.05; done", flogs, ftmp)
	h.step("start the planting loop as " + f.fsu)
	loop := execBackground("sudo", "-u", f.fsu, "bash", "-c", plant)
	stopLoop := h.undo("stop the planting loop", func() {
		loop.stop()
		h.run("pkill", "-u", f.fsu, "-f", "cg-plant")
	})
	cleanPlant := h.undo("remove the planted dump", func() {
		h.run("mariadb", "-e", "DROP TABLE IF EXISTS `"+fdb+"`.cg_e2e_planted")
		_ = os.Remove(flogs + "/db-latest.sql")
		_ = os.Remove(flogs + "/.cg-plant.sql")
		_ = os.Remove(ftmp + "/.cg-plant.tmp")
	})
	_, b := h.api.post(f.api("/backups"), `{}`)
	h.check(t, "backup runs", h.api.waitTask(taskID(b), 300*time.Second), "succeeded")
	stopLoop()

	restic := h.resticOn("restic", brepo, bpw)
	ndb := 0
	dbSnaps := restic("snapshots", "--json", "--tag", "site:"+f.fs+",part:database")
	if ids, err := ResticSnapshotIDs(dbSnaps.Out); err == nil && dbSnaps.OK() {
		ndb = len(ids)
	}
	h.assert(t, "R-0.2: the backup stored the database as its own restic snapshot", ndb >= 1,
		"R-0.2: no restic snapshot tagged site:%s,part:database; the dump still goes through the site's tree (restic snapshots: %s)", f.fs, dbSnaps)
	snap := int64(0)
	if r, ok := newest(f.backupsList(), func(backupRow) bool { return true }); ok {
		snap = r.ID
	}
	_, b = h.api.post(fmt.Sprintf("/api/backups/%d/verify", snap), `{}`)
	h.check(t, "verified restore passes", h.api.waitTask(taskID(b), 300*time.Second), "succeeded")
	h.wp(f.fsu, f.fp, "option", "update", "cg_e2e_marker", "after-backup")
	// R-3.6: a table made after the backup is not in the snapshot, so a
	// restore must not keep it (the dump used to be imported on top).
	h.run("mariadb", "-e", "CREATE TABLE IF NOT EXISTS `"+fdb+"`.cg_e2e_after (id INT)")
	cleanAfter := h.undo("drop the post-backup table", func() {
		h.run("mariadb", "-e", "DROP TABLE IF EXISTS `"+fdb+"`.cg_e2e_after")
	})
	_ = os.WriteFile(f.fp+"/cg-marker.txt", []byte("after\n"), 0o644) // as `echo after >`: owner kept
	// R-4.2: a restore canceled while its safety snapshot runs (the agent
	// frozen there, so the cancel lands mid-step) stops before the restore:
	// the site keeps the files and the database it has now.
	frozen := h.run("systemctl", "kill", "-s", "STOP", "cloudground-agent")
	thaw := h.undo("resume the agent", func() { h.run("systemctl", "kill", "-s", "CONT", "cloudground-agent") })
	h.assert(t, "R-4.2 control: the agent is frozen for the canceled restore", frozen.OK(), "systemctl kill -s STOP: %s", frozen.Err)
	_, b = h.api.post(fmt.Sprintf("/api/backups/%d/restore", snap), fmt.Sprintf(`{"confirm":%q,"mode":"full"}`, f.fs))
	ct := taskID(b)
	h.check(t, "R-4.2 control: the restore to cancel is running (its safety snapshot)", h.api.waitJobState(ct, 30*time.Second, "running"), "running")
	_, cb := h.api.post(fmt.Sprintf("/api/tasks/%d/cancel", ct), `{}`)
	h.check(t, "R-4.2: the running restore accepts the cancel", jsonBool(cb, "cancel_requested"), "true")
	thaw()
	h.check(t, "R-4.2: the canceled restore ends canceled", h.api.waitJobState(ct, 300*time.Second, "succeeded", "failed", "canceled"), "canceled")
	h.check(t, "R-4.2: a canceled restore leaves the site's files as they were", readTrim(f.fp+"/cg-marker.txt"), "after")
	h.check(t, "R-4.2: a canceled restore leaves the site's database as it was", h.wp(f.fsu, f.fp, "option", "get", "cg_e2e_marker").Trim(), "after-backup")
	_, b = h.api.post(fmt.Sprintf("/api/backups/%d/restore", snap), fmt.Sprintf(`{"confirm":%q,"mode":"full"}`, f.fs))
	h.check(t, "in-place restore succeeds", h.api.waitTask(taskID(b), 300*time.Second), "succeeded")
	h.check(t, "restore brought the database back", h.wp(f.fsu, f.fp, "option", "get", "cg_e2e_marker").Trim(), "before-backup")
	h.check(t, "R-3.6: the restored database is the snapshot's, without tables made since",
		h.run("mariadb", "-N", "-e", "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA='"+fdb+"' AND TABLE_NAME='cg_e2e_after'").Trim(), "0")
	cleanAfter()
	h.check(t, "restore brought the files back", readTrim(f.fp+"/cg-marker.txt"), "before")
	// R-4.6: backups leave tmp/ out; the restore makes PHP's sessions
	// directory again, the site user's.
	so, sm := statOwnerMode("/srv/sites/" + f.fs + "/tmp/sessions")
	h.check(t, "R-4.6: a restored site has its sessions directory (tmp/sessions, the site user's, 750)", strings.TrimSpace(so+" "+sm), f.fsu+" 750")
	h.check(t, "R-0.2: the restore did not import the dump the site user planted in logs/ (cg_e2e_planted tables)",
		h.run("mariadb", "-N", "-e", "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='"+fdb+"' AND table_name='cg_e2e_planted'").Trim(), "0")
	cleanPlant()
	rc, _ := h.serves(f.fs, "/")
	h.check(t, "restored site serves", rc, "200")
	nstage := 0
	if ents, err := os.ReadDir("/srv/sites"); err == nil {
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), ".cg-restore-") {
				nstage++
			}
		}
	}
	h.check(t, "no restore staging left next to the sites", strconv.Itoa(nstage), "0")
	// R-0.3: a restore empties the page cache but keeps its root; nginx
	// creates it only at start or reload and www-data cannot recreate it.
	fi, err := os.Stat("/var/cache/cloudground/" + f.fs)
	h.assert(t, "R-0.3: page cache root survives a restore", err == nil && fi.IsDir(),
		"R-0.3: restore removed /var/cache/cloudground/%s; nginx cannot recreate it until a reload", f.fs)
	for i := 0; i < 2; i++ {
		h.get(f.fs, "/", "Accept-Encoding", "gzip")
	}
	h.check(t, "R-0.3: the restored site caches again (3rd request after the restore)", h.cacheStatus(f.fs, "gzip", "/"), "HIT")

	// R-0.10: the safety snapshots taken before a restore and before a
	// staging database push are restore-tested like every other backup.
	bkStatus := func(kind string) string {
		if r, ok := newest(f.backupsList(), func(r backupRow) bool { return r.Kind == kind }); ok {
			return r.VerifyStatus
		}
		return "none"
	}
	h.check(t, "R-0.10: the pre-restore safety snapshot passed its restore test (verify_status)", bkStatus("pre-restore"), "passed")
	if h.api.stagingOf(f.fid).ID != 0 {
		_, b = h.api.post(f.api("/staging/database"), fmt.Sprintf(`{"tables":["wp_terms"],"confirm":%q}`, f.fs))
		h.check(t, "staging database push of wp_terms succeeds", h.api.waitTask(taskID(b), 600*time.Second), "succeeded")
		h.check(t, "R-0.10: the pre-database-push safety snapshot passed its restore test (verify_status)", bkStatus("pre-database-push"), "passed")
		f.stagingPush(t)
	} else {
		h.fail(t, "R-0.10: no staging site, the database-push safety snapshot was not checked", "R-0.10: no staging site, the database-push safety snapshot was not checked")
	}

	f.databaseRestoreReachesSite(t)
	f.corruptDump(t)

	// R-0.6: retention is applied. A snapshot dated 2020 is planted with
	// restic's own --time, with the agent's tag, host and path, so only the
	// product's retention run can remove it. The newest verified snapshot
	// must survive.
	// A repository restic cannot list holds neither: "unreadable" fails
	// both the "removed" and the "kept" check.
	hasSnap := func(id string) string {
		r := restic("snapshots", "--json")
		ids, err := ResticSnapshotIDs(r.Out)
		if !r.OK() || err != nil {
			return "unreadable"
		}
		return yesNo(HasSnapshot(ids, id))
	}
	vsnap := ""
	if r, ok := newest(f.backupsList(), func(r backupRow) bool { return r.VerifyStatus == "passed" }); ok {
		vsnap = r.SnapshotID
	}
	aged := restic("backup", "--json", "--tag", "site:"+f.fs, "--tag", "kind:full",
		"--time", "2020-01-01 00:00:00", "/srv/sites/"+f.fs)
	oldsnap := ResticSummarySnapshot(aged.Out)
	if vsnap != "" && oldsnap != "" && hasSnap(oldsnap) == "yes" {
		h.assert(t, "control: an aged snapshot is in the repository", true, "")
		_, b = h.api.post(f.api("/backups/retention"), `{}`)
		h.check(t, "retention runs", h.api.waitTask(taskID(b), 600*time.Second), "succeeded")
		h.check(t, "retention removed the snapshot beyond the window", hasSnap(oldsnap), "no")
		h.check(t, "retention kept the newest verified snapshot", hasSnap(vsnap), "yes")
	} else {
		h.fail(t, "retention fixture: no verified snapshot or the aged snapshot was not created", "retention fixture: no verified snapshot (%s) or the aged snapshot was not created (%s; restic backup: %s)", vsnap, oldsnap, head([]byte(aged.Err), 300))
	}
	restoreSettings()
}

// databaseRestoreReachesSite (R-3.4): a database-only restore must reach the
// running site. With the object cache on, WordPress keeps its options in the
// PHP pool's APCu, which the import does not touch: the site went on serving
// the values from before the restore. The option is read back over HTTP,
// the site's own path, not with wp-cli (which has an APCu of its own).
func (f *feat) databaseRestoreReachesSite(t *testing.T) {
	h := f.h
	f.objectCacheSet(t, true, "R-3.4: object cache on before a database restore")
	opt, done := f.optionProbe(t, "dbr")
	defer done()
	inBackup, after := fmt.Sprintf("in-backup-%d", h.pid), fmt.Sprintf("after-backup-%d", h.pid)
	opt("&set=" + inBackup)
	_, b := h.api.post(f.api("/backups"), `{}`)
	h.check(t, "R-3.4: the backup for the database restore runs", h.api.waitTask(taskID(b), 300*time.Second), "succeeded")
	snap, ok := newest(f.backupsList(), func(r backupRow) bool { return r.Kind == "full" && r.VerifyStatus == "passed" })
	if !ok {
		h.fail(t, "R-3.4: database restore fixture", "R-3.4: no verified backup to restore the database from")
		return
	}
	opt("&set=" + after)
	h.check(t, "R-3.4 control: the site reads the changed option through its object cache", opt(""), after)
	_, b = h.api.post(fmt.Sprintf("/api/backups/%d/restore", snap.ID), fmt.Sprintf(`{"confirm":%q,"mode":"database"}`, f.fs))
	h.check(t, "R-3.4: database-only restore succeeds", h.api.waitTask(taskID(b), 600*time.Second), "succeeded")
	h.check(t, "R-3.4: after a database-only restore the site serves the restored option, not its object cache's", opt(""), inBackup)
}

// corruptDump (R-0.9): the restore test must import the dump, not just grep
// its header. A mariadb-dump stand-in ahead of the real one on the agent's
// PATH writes a valid header and a truncated CREATE TABLE and exits 0; the
// backup's automatic restore test must fail on it.
func (f *feat) corruptDump(t *testing.T) {
	h := f.h
	const fake = "/usr/local/sbin/mariadb-dump"
	const used = "/run/cg-e2e-corrupt-dump.used"
	script := "#!/bin/sh\ntouch " + used + "\n" +
		"printf -- '-- MariaDB dump 10.19  cloudground e2e corrupt dump\\nCREATE TABLE `cg_corrupt` (\\n  `id` int(11) NOT NULL,\\n'\n"
	// Leave the box as it was, even a binary someone put there before us.
	prev, prevErr := os.ReadFile(fake)
	removeFake := h.undo("remove the fake mariadb-dump", func() {
		_ = os.Remove(fake)
		if prevErr == nil {
			_ = os.WriteFile(fake, prev, 0o755)
		}
		_ = os.Remove(used)
	})
	h.step("install " + fake)
	_ = os.Remove(fake)
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Logf("fixture: %v", err)
	}
	_ = os.Chmod(fake, 0o755)
	_ = os.Remove(used)
	_, b := h.api.post(f.api("/backups"), `{}`)
	cbst := h.api.waitTask(taskID(b), 300*time.Second)
	h.step("rm " + fake)
	_ = os.Remove(fake)
	if prevErr == nil {
		_ = os.WriteFile(fake, prev, 0o755)
	}
	if exists(used) {
		h.check(t, "R-0.9: a backup whose dump does not import fails its restore test (task)", cbst, "failed")
		cbv := ""
		if r, ok := newest(f.backupsList(), func(backupRow) bool { return true }); ok {
			cbv = r.VerifyStatus
		}
		h.check(t, "R-0.9: a backup whose dump does not import is marked verify failed, not passed", cbv, "failed")
	} else {
		h.fail(t, "R-0.9 setup: the agent did not run /usr/local/sbin/mariadb-dump (PATH?); the corrupt-dump check did not run",
			"R-0.9 setup: the agent did not run /usr/local/sbin/mariadb-dump (PATH?); the corrupt-dump check did not run")
	}
	removeFake()
	h.check(t, "R-0.9: restore tests leave no cgv_ scratch database behind",
		strconv.Itoa(CountNonEmptyLines(h.run("mariadb", "-N", "-e", `SHOW DATABASES LIKE 'cgv\_%'`).Out)), "0")
}

// ---------------------------------------------------------------- firewall --

func (h *harness) firewallRules(t *testing.T) {
	// firewall status is readable through the agent
	h.check(t, "firewall status answers", code(h.api.get("/api/firewall")), "200")
	// The panel lives on its own port; 80/443 belong to the sites.
	h.check(t, "the panel answers on :8443",
		h.fetch(req{URL: "https://" + h.IP + ":8443/login", Addr: h.IP + ":8443", Timeout: 8 * time.Second}).CodeStr(), "200")
	r := h.fetch(req{URL: "https://not-hosted.example/login", Addr: h.IP + ":443", Timeout: 8 * time.Second})
	h.assert(t, "an unknown hostname on 443 does not get the panel", !contains(r.Body, "CloudGround"), "an unknown hostname on 443 gets the panel")
	refused := func(action, port string) bool {
		c, _ := h.api.post("/api/firewall/rule", fmt.Sprintf(`{"action":%q,"port":%s,"proto":"tcp"}`, action, port))
		return c >= 400
	}
	h.assert(t, "the firewall refuses to close 443", refused("deny", "443"), "the firewall closed 443")
	port := h.sshPort()
	h.assert(t, "the firewall refuses to drop SSH", refused("delete", port), "the firewall dropped the SSH port (%s)", port)
	if st := h.run("ufw", "status"); strings.Contains(st.Out, "Status: active") {
		allowed := false
		for _, line := range strings.Split(st.Out, "\n") {
			if strings.Contains(line, "ALLOW") && strings.Contains(line, "443") {
				allowed = true
			}
		}
		h.assert(t, "443 is still allowed", allowed, "443 is no longer allowed")
	} else {
		t.Logf("(ufw is inactive on this box: the rule table is not checked)")
	}
}
