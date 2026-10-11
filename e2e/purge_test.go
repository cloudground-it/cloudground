//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The connector's purge coverage (F7, connector/SPEC.md §2.2–2.3), watched
// where it matters: the page cache's own X-CloudGround-Cache header. Every
// change is made through the site's PHP-FPM, as wp-admin makes it, so the
// connector runs where it runs for a real editor. Each check fails with the
// connector before F7, which purged only the post's permalink, its
// categories and tags, and nothing for a WooCommerce data-layer save.

// sitePHP writes a PHP file into a live release as the site user, runs it
// once through PHP-FPM (a query string keeps it out of the page cache),
// removes it, and decodes what it printed between the markers into out.
func (h *harness) sitePHP(t *testing.T, dom, release, user, name, code string, out any) bool {
	t.Helper()
	file := release + "/" + name
	h.mustWriteAs(t, file, "<?php require __DIR__ . '/wp-load.php';\n"+code, user)
	defer h.remove(file)
	body := string(h.get(dom, fmt.Sprintf("/%s?run=%d-%d", name, h.pid, time.Now().UnixNano())).Body)
	_, rest, ok := strings.Cut(body, "CGJSON")
	js, _, ok2 := strings.Cut(rest, "CGEND")
	if !ok || !ok2 || json.Unmarshal([]byte(js), out) != nil {
		t.Logf("%s printed: %.400s", name, body)
		return false
	}
	return true
}

// cachedHit requests a page until the page cache serves it (at most four
// times) and reports the last cache status and HTTP status.
func (h *harness) cachedHit(dom, path string) (string, int) {
	st, code := "", 0
	for i := 0; i < 4 && st != "HIT"; i++ {
		r := h.get(dom, path, "Accept-Encoding", "gzip")
		st, code = CacheStatus(r.Header), r.Code
	}
	return st, code
}

// afterPurge polls a cached page until it stops being a HIT and returns the
// first other status, or "HIT" after 10 s. The connector sends its purge
// after the response, and the cache location keeps a deleted entry's
// descriptor for up to 1 s, so the first poll may still be a HIT.
func (h *harness) afterPurge(dom, path string) string {
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(500 * time.Millisecond) {
		st := CacheStatus(h.get(dom, path, "Accept-Encoding", "gzip").Header)
		if st != "HIT" || time.Now().After(end) {
			return st
		}
	}
}

// warmControl caches a page and records that it is a 200 HIT: without it
// the purge check below would pass on a page that was never cached.
func (h *harness) warmControl(t *testing.T, label, dom, path string) bool {
	st, code := h.cachedHit(dom, path)
	ok := st == "HIT" && code == 200
	h.assert(t, label, ok, "%s is %d %s, not a cached 200", path, code, st)
	return ok
}

// purgeCoverage: a slug change, a custom taxonomy and the core sitemap.
func (f *feat) purgeCoverage(t *testing.T) {
	h := f.h
	tag := fmt.Sprintf("%d", h.pid)
	php := func(name, code string, out any) bool { return h.sitePHP(t, f.fs, f.fp, f.fsu, name, code, out) }

	// A custom public taxonomy, registered the way a plugin does it.
	mu := f.fp + "/wp-content/mu-plugins/cg-e2e-genre.php"
	h.mustWriteAs(t, mu, `<?php add_action('init', function () {
    register_taxonomy('cg_genre', ['post'], ['public' => true, 'label' => 'Genre', 'rewrite' => ['slug' => 'cg-genre']]);
});`, f.fsu)
	defer h.remove(mu)

	var made struct {
		Post, GenrePost int
		Old, Genre      string
	}
	ok := php("cg-e2e-setup.php", `
flush_rewrite_rules(false);
$id = wp_insert_post(['post_title' => 'F7 slug `+tag+`', 'post_name' => 'f7-slug-`+tag+`', 'post_status' => 'publish', 'post_content' => 'a']);
$term = wp_insert_term('Jazz `+tag+`', 'cg_genre', ['slug' => 'f7-jazz-`+tag+`']);
$gid = wp_insert_post(['post_title' => 'F7 genre `+tag+`', 'post_status' => 'publish', 'post_content' => 'a']);
wp_set_object_terms($gid, [(int) $term['term_id']], 'cg_genre');
echo 'CGJSON' . json_encode(['Post' => $id, 'GenrePost' => $gid,
    'Old' => wp_make_link_relative(get_permalink($id)),
    'Genre' => wp_make_link_relative(get_term_link((int) $term['term_id'], 'cg_genre'))]) . 'CGEND';
`, &made)
	h.assert(t, "F7 control: posts and a custom taxonomy term created through PHP-FPM",
		ok && made.Post > 0 && made.GenrePost > 0 && strings.HasPrefix(made.Genre, "/cg-genre/"),
		"setup failed: %+v", made)
	defer php("cg-e2e-cleanup.php", fmt.Sprintf(`
wp_delete_post(%d, true); wp_delete_post(%d, true);
$t = get_term_by('slug', 'f7-jazz-%s', 'cg_genre'); if ($t) { wp_delete_term($t->term_id, 'cg_genre'); }
echo 'CGJSON{}CGEND';`, made.Post, made.GenrePost, tag), &struct{}{})
	if !ok {
		return
	}

	// A slug change: the old URL must not go on serving the old page.
	if h.warmControl(t, "F7 control: the post's URL is cached", f.fs, made.Old) {
		php("cg-e2e-slug.php", fmt.Sprintf(`wp_update_post(['ID' => %d, 'post_name' => 'f7-renamed-%s']); echo 'CGJSON{}CGEND';`, made.Post, tag), &struct{}{})
		st := h.afterPurge(f.fs, made.Old)
		h.assert(t, "F7: a slug change purges the old URL", st != "HIT",
			"%s is still served from the cache after its slug changed", made.Old)
	}

	// A post with a term of a custom taxonomy changes: its archive lists it.
	if h.warmControl(t, "F7 control: the custom taxonomy term archive is cached", f.fs, made.Genre) {
		php("cg-e2e-genre-edit.php", fmt.Sprintf(`wp_update_post(['ID' => %d, 'post_title' => 'F7 genre edited %s']); echo 'CGJSON{}CGEND';`, made.GenrePost, tag), &struct{}{})
		st := h.afterPurge(f.fs, made.Genre)
		h.assert(t, "F7: a custom taxonomy term archive is purged", st != "HIT",
			"%s is still served from the cache after a post in it changed", made.Genre)
	}

	// A new post: the core sitemap lists it.
	const sitemap = "/wp-sitemap-posts-post-1.xml"
	if h.warmControl(t, "F7 control: the posts sitemap is cached", f.fs, sitemap) {
		var np struct{ ID int }
		php("cg-e2e-new.php", `echo 'CGJSON' . json_encode(['ID' => wp_insert_post(['post_title' => 'F7 new `+tag+`', 'post_name' => 'f7-new-`+tag+`', 'post_status' => 'publish', 'post_content' => 'b'])]) . 'CGEND';`, &np)
		st := h.afterPurge(f.fs, sitemap)
		h.assert(t, "F7: publishing a post purges the sitemap", st != "HIT",
			"%s is still served from the cache after a post was published", sitemap)
		body := string(h.get(f.fs, sitemap).Body)
		h.assert(t, "F7: the sitemap lists the new post", strings.Contains(body, "f7-new-"+tag),
			"the new post is not in %s", sitemap)
		if np.ID > 0 {
			php("cg-e2e-new-del.php", fmt.Sprintf(`wp_delete_post(%d, true); echo 'CGJSON{}CGEND';`, np.ID), &struct{}{})
		}
	}
}

// wooPricePurge: a price changed through WooCommerce's data layer (what the
// REST API, bulk edit and imports do) updates post meta only, and fired no
// hook the old connector listened to.
func (h *harness) wooPricePurge(t *testing.T, woo, user, release string) {
	tag := fmt.Sprintf("%d", h.pid)
	php := func(name, code string, out any) bool { return h.sitePHP(t, woo, release, user, name, code, out) }
	var made struct {
		ID         int
		Page, Cat  string
		CategoryID int
	}
	ok := php("cg-e2e-product.php", `
$cat = wp_insert_term('F7 shoes `+tag+`', 'product_cat', ['slug' => 'f7-shoes-`+tag+`']);
$p = new WC_Product_Simple();
$p->set_name('F7 runner `+tag+`');
$p->set_slug('f7-runner-`+tag+`');
$p->set_regular_price('10');
$p->set_category_ids([(int) $cat['term_id']]);
$p->set_status('publish');
$id = $p->save();
echo 'CGJSON' . json_encode(['ID' => $id, 'CategoryID' => (int) $cat['term_id'],
    'Page' => wp_make_link_relative(get_permalink($id)),
    'Cat' => wp_make_link_relative(get_term_link((int) $cat['term_id'], 'product_cat'))]) . 'CGEND';
`, &made)
	h.assert(t, "F7 control: a product in a category created through PHP-FPM", ok && made.ID > 0 && made.Cat != "",
		"product setup failed: %+v", made)
	if !ok {
		return
	}
	defer php("cg-e2e-product-del.php", fmt.Sprintf(`wp_delete_post(%d, true); wp_delete_term(%d, 'product_cat'); echo 'CGJSON{}CGEND';`,
		made.ID, made.CategoryID), &struct{}{})

	page := h.warmControl(t, "F7 control: the product page is cached", woo, made.Page)
	cat := h.warmControl(t, "F7 control: the product category archive is cached", woo, made.Cat)
	if !page || !cat {
		return
	}
	var price struct{ Price string }
	php("cg-e2e-price.php", fmt.Sprintf(`$p = wc_get_product(%d); $p->set_regular_price('12'); $p->save();
echo 'CGJSON' . json_encode(['Price' => wc_get_product(%d)->get_price()]) . 'CGEND';`, made.ID, made.ID), &price)
	h.check(t, "F7 control: the price changed", price.Price, "12")
	h.assert(t, "F7: a price change purges the product page", h.afterPurge(woo, made.Page) != "HIT",
		"%s is still served from the cache with the old price", made.Page)
	h.assert(t, "F7: a price change purges the product's category archive", h.afterPurge(woo, made.Cat) != "HIT",
		"%s is still served from the cache with the old price", made.Cat)
}

// purgeWithPort (R-5.2): nginx keys the page cache on $host, which never
// carries the port, so a request for https://<site>:443/ is served from the
// same entry as one without it, and a purge naming the port must remove it.
// The old PurgeURLs keyed on "<site>:443"; the agent also tries the site's
// own domain and aliases, which covered it, so this passes on the old build
// too: the unit test (velocity TestPurgeURLsKeysOnHostWithoutPort) is the
// one that fails there.
func (f *feat) purgeWithPort(t *testing.T) {
	h := f.h
	const path = "/sample-page/"
	withPort := func() resp {
		return h.fetch(req{URL: "https://" + f.fs + ":443" + path, Header: map[string]string{"Accept-Encoding": "gzip"}})
	}
	// The checks before this one end with PHP requests that delete what
	// they made; the connector sends their purges after the response, so
	// one could land between the warm-up below and the purge under test
	// (seen on the box: the purge naming :443 found nothing to remove, or
	// the request with :443 was a MISS). Let them land first.
	time.Sleep(2 * time.Second)
	if !h.warmControl(t, "R-5.2 control: the page is cached", f.fs, path) {
		return
	}
	h.check(t, "R-5.2: a request with :443 in Host is served from the same cache entry", CacheStatus(withPort().Header), "HIT")
	c, b := h.api.post(f.api("/purge"), fmt.Sprintf(`{"urls":["https://%s:443%s"]}`, f.fs, path))
	h.check(t, "R-5.2: a purge naming :443 is accepted", strconv.Itoa(c), "200")
	removed, _ := strconv.Atoi(jsonField(b, "removed"))
	h.assert(t, "R-5.2: a purge naming :443 removes the entry", removed >= 1, "the purge removed %d entries: %s", removed, head(b, 200))
	time.Sleep(1500 * time.Millisecond) // the cache location's open_file_cache_valid is 1 s
	h.assert(t, "R-5.2: after a purge naming :443 the page is not a HIT without the port",
		CacheStatus(h.get(f.fs, path, "Accept-Encoding", "gzip").Header) != "HIT", "the page is still served from the cache after the purge")
	if !h.warmControl(t, "R-5.2 control: the page is cached again", f.fs, path) {
		return
	}
	h.api.post(f.api("/purge"), fmt.Sprintf(`{"urls":["https://%s%s"]}`, f.fs, path))
	time.Sleep(1500 * time.Millisecond)
	h.assert(t, "R-5.2: after a purge without the port the page is not a HIT with :443", CacheStatus(withPort().Header) != "HIT",
		"a request with :443 is still served from the cache after the purge")
}
