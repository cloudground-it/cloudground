<?php
/**
 * The mu-plugin against the stand-in in wp.php (connector/SPEC.md, part 2).
 *
 *   php connector/php/tests/connector.php
 *
 * SPDX-License-Identifier: GPL-2.0-or-later
 */

use CloudGround\Connector\AuthFailure;
use CloudGround\Connector\Events;
use CloudGround\Connector\Guard;
use CloudGround\Connector\MagicLogin;
use CloudGround\Connector\Queue;
use CloudGround\Connector\Stats;
use CloudGround\Connector\Targets;

require __DIR__ . '/wp.php';
require __DIR__ . '/check.php';

define('CLOUDGROUND_SITE_TOKEN', 'site-token');
define('CLOUDGROUND_ENDPOINT', 'http://127.0.0.1:9080/');
define('CLOUDGROUND_DISABLE_CART_FRAGMENTS', true);
Site::$environment = 'staging';
require dirname(__DIR__) . '/cloudground-connector.php';

const H = 'https://example.com';

/** The URLs queued so far, without the host. */
function queued(): array
{
    $p = Queue::payload();
    if ($p === null) {
        return [];
    }
    if (!empty($p['all'])) {
        return ['ALL'];
    }
    return array_map(fn ($u) => substr($u, strlen(H)), $p['urls']);
}

function has(array $list, string ...$want): bool
{
    return array_diff($want, $list) === [];
}

/** A blog: a category with a parent, a tag, a custom taxonomy, a post. */
function blog(): WP_Post
{
    Site::reset();
    Site::$taxonomies['genre'] = [['post'], true, false];
    Site::term(['term_id' => 3, 'taxonomy' => 'category', 'slug' => 'news', 'count' => 25]);
    Site::term(['term_id' => 4, 'taxonomy' => 'category', 'slug' => 'local', 'parent' => 3, 'count' => 3]);
    Site::term(['term_id' => 5, 'taxonomy' => 'post_tag', 'slug' => 'rain', 'count' => 1]);
    Site::term(['term_id' => 6, 'taxonomy' => 'genre', 'slug' => 'jazz', 'count' => 12]);
    Site::term(['term_id' => 7, 'taxonomy' => 'post_format', 'slug' => 'post-format-aside']);
    Site::$options['count_post'] = 45;
    return Site::post(['ID' => 10, 'post_name' => 'hello'], ['category' => [4], 'post_tag' => [5], 'genre' => [6], 'post_format' => [7]]);
}

section('post set');
$post = blog();
do_action('transition_post_status', 'publish', 'publish', $post);
$q = queued();
check('the post and its comments feed', has($q, '/hello/', '/hello/feed/'), true);
check('home, main feed', has($q, '/', '/feed/'), true);
check('home pages 2..5 (45 posts, 10 a page)', has($q, '/page/2/', '/page/5/'), true);
check('no page beyond the count', in_array('/page/6/', $q, true), false);
check('the category and its parent, with pages by their own count', has($q, '/category/local/', '/category/news/', '/category/news/page/3/', '/category/news/feed/'), true);
check('the parent category stops at its count', in_array('/category/news/page/4/', $q, true), false);
check('a one-post term has no page 2', in_array('/category/local/page/2/', $q, true), false);
check('the tag', has($q, '/tag/rain/', '/tag/rain/feed/'), true);
check('a custom taxonomy term (the old connector: categories and tags only)', has($q, '/genre/jazz/', '/genre/jazz/page/2/'), true);
check('a taxonomy that is not viewable', in_array('/post_format/post-format-aside/', $q, true), false);
check('author archive and feed', has($q, '/author/user1/', '/author/user1/feed/'), true);
check('date archives with pages', has($q, '/2026/', '/2026/10/', '/2026/10/09/', '/2026/10/page/2/'), true);
check('core sitemaps', has($q, '/wp-sitemap.xml', '/wp-sitemap-posts-post-1.xml', '/wp-sitemap-taxonomies-category-1.xml', '/wp-sitemap-taxonomies-genre-1.xml', '/wp-sitemap-users-1.xml'), true);
check('no sitemap for a hidden taxonomy', in_array('/wp-sitemap-taxonomies-post_format-1.xml', $q, true), false);
check('no URL twice', count($q), count(array_unique($q)));

$post = blog();
Site::$options['page_for_posts'] = 50;
Site::$options['show_on_front'] = 'page';
Site::$options['page_on_front'] = 51;
Site::post(['ID' => 50, 'post_type' => 'page', 'post_name' => 'blog']);
Site::post(['ID' => 51, 'post_type' => 'page', 'post_name' => 'welcome']);
do_action('transition_post_status', 'publish', 'draft', $post);
$q = queued();
check('a static front page and the posts page with its pages', has($q, '/welcome/', '/blog/', '/blog/page/5/'), true);
check('home is not paginated when it is a static page', in_array('/page/2/', $q, true), false);

$post = blog();
define('CLOUDGROUND_PURGE_PAGES', 3);
do_action('transition_post_status', 'publish', 'publish', $post);
$q = queued();
check('the page cap', has($q, '/page/3/') && !in_array('/page/4/', $q, true), true);

$post = blog();
Site::$options['permalink_structure'] = '';
do_action('transition_post_status', 'publish', 'publish', $post);
$q = queued();
check('plain permalinks: only what the cache can hold (no query strings)', count(array_filter($q, fn ($u) => str_contains($u, '?'))), 0);
check('plain permalinks: no pagination or sitemaps', in_array('/page/2/', $q, true) || in_array('/wp-sitemap.xml', $q, true), false);
check('plain permalinks: the home page', in_array('/', $q, true), true);

section('post events');
$post = blog();
do_action('transition_post_status', 'draft', 'pending', $post);
check('a change between unpublished states purges nothing', Queue::payload(), null);

$before = blog();
$after = clone $before;
$after->post_name = 'renamed';
Site::$posts[10] = $after;
do_action('transition_post_status', 'publish', 'publish', $after);
do_action('post_updated', 10, $after, $before);
$q = queued();
check('a slug change purges the new URL', in_array('/renamed/', $q, true), true);
check('a slug change purges the old URL (the old connector did not)', in_array('/hello/', $q, true), true);

$before = blog();
$after = clone $before;
$after->post_status = 'draft';
Site::$posts[10] = $after;
do_action('transition_post_status', 'draft', 'publish', $after);
do_action('post_updated', 10, $after, $before);
check('unpublishing purges the URL it had (the old connector purged ?p=ID)', in_array('/hello/', queued(), true), true);

Site::reset();
$parent = Site::post(['ID' => 20, 'post_type' => 'page', 'post_name' => 'about']);
Site::post(['ID' => 21, 'post_type' => 'page', 'post_name' => 'team', 'post_parent' => 20]);
$moved = clone $parent;
$moved->post_name = 'about-us';
Site::$posts[20] = $moved;
do_action('post_updated', 20, $moved, $parent);
check('a page with children changing its path purges everything', queued(), ['ALL']);

$post = blog();
do_action('before_delete_post', 10, $post);
check('deleting a published post outright', has(queued(), '/hello/', '/category/local/'), true);

$post = blog();
do_action('set_object_terms', 10, [], [5], 'post_tag', false, [5]);
check('terms saved unchanged purge nothing', Queue::payload(), null);
Site::term(['term_id' => 8, 'taxonomy' => 'post_tag', 'slug' => 'sun']);
do_action('set_object_terms', 10, [], [8], 'post_tag', false, [5]);
$q = queued();
check('a term removed from a post purges its archive too', has($q, '/tag/rain/', '/tag/sun/', '/hello/'), true);

foreach (['wp_template_part', 'wp_block'] as $type) {
    Site::reset();
    do_action('transition_post_status', 'publish', 'publish', Site::post(['ID' => 30, 'post_type' => $type]));
    check("$type purges everything", queued(), ['ALL']);
}

// WordPress's fallback menu, saved by a Navigation block while a visitor's
// page renders, changes nothing the page shows; a menu made anywhere else,
// or a fallback menu edited later, purges everything.
Site::reset();
Events::$pageView = true;
do_action('transition_post_status', 'publish', 'new', Site::post(['ID' => 33, 'post_type' => 'wp_navigation']));
check('the fallback menu a page view saves purges nothing', Queue::payload(), null);
do_action('transition_post_status', 'publish', 'publish', Site::post(['ID' => 33, 'post_type' => 'wp_navigation']));
check('a menu edited during a page view purges everything', queued(), ['ALL']);
Site::reset();
Events::$pageView = false;
do_action('transition_post_status', 'publish', 'new', Site::post(['ID' => 34, 'post_type' => 'wp_navigation']));
check('a menu made in the editor purges everything', queued(), ['ALL']);
Site::reset();
Events::$pageView = true;
do_action('transition_post_status', 'publish', 'new', Site::post(['ID' => 35, 'post_type' => 'wp_template_part']));
check('a template part a page view saves purges everything', queued(), ['ALL']);
Events::$pageView = null;
check('wp-cli is not a page view', (new ReflectionMethod(Events::class, 'pageView'))->invoke(null), false);

Site::reset();
do_action('transition_post_status', 'publish', 'publish', Site::post(['ID' => 31, 'post_type' => 'revision']));
do_action('transition_post_status', 'publish', 'publish', Site::post(['ID' => 32, 'post_type' => 'nav_menu_item']));
check('revisions and types without pages purge nothing', Queue::payload(), null);

section('comments, terms, options');
$post = blog();
Site::$comments[90] = new WP_Comment(['comment_ID' => '90', 'comment_post_ID' => '10', 'comment_approved' => '0']);
do_action('comment_post', 90, 0, []);
do_action('comment_post', 90, 'spam', []);
check('a held or spam comment purges nothing', Queue::payload(), null);
do_action('comment_post', 90, 1, []);
check('an approved comment', queued(), ['/hello/', '/hello/feed/', '/comments/feed/']);
Queue::reset();
do_action('transition_comment_status', 'unapproved', 'approved', Site::$comments[90]);
check('a comment unapproved', in_array('/hello/', queued(), true), true);

Site::reset();
do_action('edited_term', 3, 3, 'category');
check('a category edited purges everything', queued(), ['ALL']);
Site::reset();
do_action('edited_term', 7, 7, 'post_format');
check('a hidden taxonomy term purges nothing', Queue::payload(), null);

foreach (['blogname', 'theme_mods_twentytwentyfive', 'widget_block', 'woocommerce_currency', 'permalink_structure'] as $opt) {
    Site::reset();
    do_action('updated_option', $opt, 'a', 'b');
    check("option $opt purges everything", queued(), ['ALL']);
}
// WordPress keeps the Custom CSS post lookup in the theme mods the first
// time a page renders (wp_get_custom_css_post): nothing a page shows.
Site::reset();
do_action('added_option', 'theme_mods_twentytwentyfive', ['custom_css_post_id' => -1]);
check('the first Custom CSS lookup of a fresh site purges nothing', Queue::payload(), null);
Site::reset();
do_action('updated_option', 'theme_mods_twentytwentyfive', ['header_textcolor' => 'fff'], ['header_textcolor' => 'fff', 'custom_css_post_id' => 12]);
check('the first Custom CSS lookup next to other mods purges nothing', Queue::payload(), null);
Site::reset();
do_action('updated_option', 'theme_mods_twentytwentyfive', ['custom_css_post_id' => -1], ['custom_css_post_id' => 42]);
check('a Custom CSS post id that changes purges everything', queued(), ['ALL']);
Site::reset();
do_action('updated_option', 'theme_mods_twentytwentyfive', ['header_textcolor' => 'fff'], ['header_textcolor' => '000', 'custom_css_post_id' => -1]);
check('another mod changing with the lookup purges everything', queued(), ['ALL']);
Site::reset();
do_action('added_option', 'theme_mods_twentytwentyfive', ['custom_css_post_id' => -1, 'nav_menu_locations' => [2]]);
check('new theme mods beyond the lookup purge everything', queued(), ['ALL']);
Site::reset();
do_action('added_option', 'blogname', 'x');
check('an added site-wide option purges everything', queued(), ['ALL']);
Site::reset();
do_action('deleted_option', 'theme_mods_twentytwentyfive');
check('deleted theme mods purge everything', queued(), ['ALL']);
Site::reset();
do_action('updated_option', 'cron', [], []);
do_action('updated_option', '_transient_x', 1, 2);
do_action('updated_option', 'woocommerce_admin_notices', 1, 2);
check('other options purge nothing', Queue::payload(), null);
foreach (['switch_theme', 'activated_plugin', 'upgrader_process_complete', 'wp_update_nav_menu', 'customize_save_after', 'woocommerce_attribute_updated'] as $hook) {
    Site::reset();
    do_action($hook, 'x', 'y');
    check("$hook purges everything", queued(), ['ALL']);
}

section('WooCommerce');
function shop(): void
{
    Site::reset();
    Site::$taxonomies['pa_color'] = [['product'], true, false];
    Site::term(['term_id' => 40, 'taxonomy' => 'product_cat', 'slug' => 'shoes', 'count' => 40]);
    Site::term(['term_id' => 41, 'taxonomy' => 'product_tag', 'slug' => 'sale', 'count' => 2]);
    Site::term(['term_id' => 42, 'taxonomy' => 'pa_color', 'slug' => 'red', 'count' => 2]);
    Site::post(['ID' => 60, 'post_type' => 'product', 'post_name' => 'runner'], ['product_cat' => [40], 'product_tag' => [41], 'pa_color' => [42]]);
    Site::post(['ID' => 61, 'post_type' => 'product_variation', 'post_parent' => 60, 'post_name' => 'runner-red']);
    Site::$options['count_product'] = 40;
}
shop();
do_action('woocommerce_product_object_updated_props', new FakeProduct(60), ['regular_price']);
$q = queued();
check('a price change purges the product page (the old connector: nothing)', in_array('/product/runner/', $q, true), true);
check('a price change purges its category archive', has($q, '/product-category/shoes/', '/product-category/shoes/page/2/'), true);
check('…its tag and attribute archives', has($q, '/product-tag/sale/', '/pa_color/red/'), true);
check('…the shop with its pages and the home page', has($q, '/shop/', '/shop/page/2/', '/'), true);
check('…the product sitemap', has($q, '/wp-sitemap-posts-product-1.xml', '/wp-sitemap-taxonomies-product_cat-1.xml'), true);
check('products are not in the date archives', in_array('/2026/', $q, true), false);

shop();
do_action('woocommerce_product_object_updated_props', new FakeProduct(61), ['sale_price']);
check('a variation purges its parent product', has(queued(), '/product/runner/', '/product-category/shoes/'), true);
shop();
do_action('woocommerce_product_object_updated_props', new FakeProduct(60), ['stock_quantity', 'total_sales']);
check('a stock quantity change purges the product page only', queued(), ['/product/runner/']);
shop();
do_action('woocommerce_product_object_updated_props', new FakeProduct(60), ['total_sales']);
check('an order that changes only total_sales purges nothing', Queue::payload(), null);
shop();
do_action('woocommerce_variation_set_stock', new FakeProduct(61));
check('woocommerce_variation_set_stock', queued(), ['/product/runner/']);
shop();
do_action('woocommerce_product_set_stock_status', 60, 'outofstock', new FakeProduct(60));
check('a stock status change purges the listings', has(queued(), '/product/runner/', '/product-category/shoes/', '/shop/'), true);
shop();
do_action('updated_post_meta', 1, 61, '_price', '9');
check('a price written straight to meta (a variation)', has(queued(), '/product/runner/', '/product-category/shoes/'), true);
shop();
do_action('updated_post_meta', 1, 60, '_stock', '3');
check('stock written straight to meta', queued(), ['/product/runner/']);
shop();
do_action('updated_post_meta', 1, 60, '_edit_lock', 'x');
do_action('updated_post_meta', 1, 10, '_price', 'x');
check('other meta, or _price on a non-product, purges nothing', Queue::payload(), null);

section('transport');
$post = blog();
do_action('transition_post_status', 'publish', 'publish', $post);
check('one shutdown action, last', isset(Site::$hooks['shutdown'][PHP_INT_MAX]), true);
do_action('shutdown');
check('one request to the panel', count(Site::$requests), 1);
[$url, $args] = Site::$requests[0];
check('the endpoint', $url, 'http://127.0.0.1:9080/api/connector/purge');
check('blocking, so wp-cli does not drop it', $args['blocking'], true);
check('a short timeout', $args['timeout'], 3);
check('the bearer token', $args['headers']['Authorization'], 'Bearer site-token');
check('JSON', $args['headers']['Content-Type'], 'application/json');
$body = json_decode($args['body'], true);
check('the body has only the fields the API accepts', array_keys($body), ['urls']);
check('absolute URLs on the site', count(array_filter($body['urls'], fn ($u) => !str_starts_with($u, H . '/'))), 0);
do_action('shutdown');
check('the queue is empty after sending', count(Site::$requests), 1);

Site::reset();
Queue::add(array_map(fn ($i) => H . "/p$i/", range(1, Queue::LIMIT + 1)));
check('more than the limit becomes a full purge', Queue::payload(), ['all' => true]);
Site::reset();
Queue::add([H . '/a/']);
Queue::everything();
check('everything wins over URLs', Queue::payload(), ['all' => true]);
Site::reset();
add_filter('cloudground_purge_urls', fn ($u) => array_merge($u, [H . '/sitemap_index.xml']));
Queue::add([H . '/a/']);
check('the cloudground_purge_urls filter', Queue::payload(), ['urls' => [H . '/a/', H . '/sitemap_index.xml']]);
unset(Site::$hooks['cloudground_purge_urls']);

section('magic login');
$wpdb = $GLOBALS['wpdb'];
function issue(string $value): string
{
    $token = bin2hex(random_bytes(24));
    $GLOBALS['wpdb']->rows[MagicLogin::PREFIX . hash('sha256', $token)] = $value;
    return $token;
}
Site::reset();
$tok = issue('1:' . (time() + 300));
$r = MagicLogin::signIn(str_repeat('a', 48));
check('a wrong token is refused', $r->status, 403);
check('a wrong token does not use up the real one', count($wpdb->rows), 1);
$r = MagicLogin::signIn($tok);
check('the token signs in', [$r->status, $r->user, $r->headers['Location'] ?? ''], [302, 1, H . '/wp-admin/']);
check('the row is gone', $wpdb->rows, []);
check('a used token is refused', MagicLogin::signIn($tok)->status, 403);
check('a malformed token is a bad request', MagicLogin::signIn('<script>')->status, 400);
check('upper-case hex is not the token', MagicLogin::signIn(strtoupper(issue('1:' . (time() + 300))))->status, 400);
$wpdb->rows = [];
$tok = issue('1:' . (time() - 1));
check('an expired token is refused', MagicLogin::signIn($tok)->status, 403);
check('…and consumed', $wpdb->rows, []);
$tok = issue('2:' . (time() + 300));
check('a user who is no longer an administrator', MagicLogin::signIn($tok)->status, 403);
$tok = issue('99:' . (time() + 300));
check('a user who no longer exists', MagicLogin::signIn($tok)->status, 403);
$tok = issue('garbage');
check('a malformed row', MagicLogin::signIn($tok)->status, 403);
$lookups = array_filter($wpdb->queries, fn ($q) => str_starts_with($q, 'SELECT'));
check('looked up by option name, never by LIKE', count(array_filter($lookups, fn ($q) => stripos($q, 'LIKE') !== false || !str_contains($q, "WHERE option_name = '"))), 0);
$page = MagicLogin::page();
preg_match("/'nonce-([0-9a-f]+)'/", $page->headers['Content-Security-Policy'], $m);
check('the page allows only its own script', isset($m[1]) && str_contains($page->body, '<script nonce="' . $m[1] . '">'), true);
check('the page never carries a token', preg_match('/[0-9a-f]{48}/', $page->body), 0);
check('a fresh nonce each time', MagicLogin::page()->headers['Content-Security-Policy'] !== $page->headers['Content-Security-Policy'], true);

section('statistics');
check('no token sent', Stats::answer('')->status, 403);
check('a wrong token', [Stats::answer('nope')->status, Stats::answer('nope')->body], [403, '{"error":"forbidden"}']);
$r = Stats::answer('site-token');
check('the right token answers 200', $r->status, 200);
check('never cached', $r->headers['Cache-Control'], 'no-store');
$data = json_decode($r->body, true);
if (function_exists('apcu_enabled') && apcu_enabled()) {
    check('numbers from APCu', array_keys($data), ['hits', 'misses', 'entries', 'mem_used', 'mem_total']);
    check('a segment size', $data['mem_total'] > 0, true);
} else {
    check('APCu unavailable here', $data, ['error' => 'apcu unavailable']);
}
check('statistics run before regular plugins load', isset(Site::$hooks['muplugins_loaded'][0]), true);
check('there is no flush endpoint any more', str_contains((string) file_get_contents(dirname(__DIR__) . '/cloudground-connector.php'), 'cloudground_flush'), false);

section('Guard footer');
Site::reset();
$key = hash_hmac('sha256', 'guard-probe', 'site-token');
check('an anonymous visitor gets nothing', Guard::comment(''), '');
check('a wrong probe key gets nothing', Guard::comment(str_repeat('0', 64)), '');
check('the token itself is not the key', Guard::comment('site-token'), '');
$GLOBALS['timestart'] = microtime(true) - 0.25;
setlocale(LC_NUMERIC, 'it_IT.UTF-8', 'de_DE.UTF-8', 'it_IT', 'de_DE');
$c = Guard::comment($key);
check("Guard's probe gets the numbers", preg_match('/\A\n<!-- cloudground queries=7 time=0\.2\d{3} mem=\d+ -->\n\z/', $c), 1);
check('the time is a decimal Guard can parse in any locale', str_contains($c, 'time=0,'), false);
setlocale(LC_NUMERIC, 'C');
Site::$loggedIn = true;
check('a logged-in user gets nothing', Guard::comment($key), '');

section('cart fragments and staging');
Site::reset();
do_action('wp_enqueue_scripts');
check('cart fragments dequeued on a normal page', Site::$dequeued, ['wc-cart-fragments']);
Site::reset();
Site::$isCart = true;
do_action('wp_enqueue_scripts');
do_action('wp_print_footer_scripts');
check('kept on the cart page', Site::$dequeued, []);
Site::reset();
do_action('wp_print_footer_scripts');
check('dequeued again before footer scripts print (widgets enqueue late)', Site::$dequeued, ['wc-cart-fragments']);
check('staging sends no mail', apply_filters('pre_wp_mail', null, []), true);

section('failed sign-ins');
$sent = [];
AuthFailure::$send = function (string $line) use (&$sent) { $sent[] = $line; };
AuthFailure::reset();
do_action('wp_login_failed', 'admin', null);
check('a failed sign-in is marked', $sent, ['X-CloudGround-Auth: failed=1']);
do_action('application_password_failed_authentication', null);
check('a second failure in the request carries the count', end($sent), 'X-CloudGround-Auth: failed=2');
AuthFailure::reset();
$sent = [];
do_action('wp_login', 'admin', null);
check('a successful sign-in is not marked', $sent, []);

done();
