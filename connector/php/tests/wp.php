<?php
/**
 * A small stand-in for WordPress: just the functions the connector calls,
 * with the behaviour the connector relies on, over an in-memory site.
 * Real WordPress is what the e2e suite runs against.
 *
 * SPDX-License-Identifier: GPL-2.0-or-later
 */

define('ABSPATH', __DIR__ . '/');

class WP_Post
{
    public int $ID = 0;
    public string $post_type = 'post';
    public string $post_status = 'publish';
    public string $post_name = '';
    public int $post_parent = 0;
    public string $post_author = '1';
    public string $post_date = '2026-10-09 12:00:00';

    public function __construct(array $f = [])
    {
        foreach ($f as $k => $v) {
            $this->$k = $v;
        }
    }
}

class WP_Term
{
    public int $term_id = 0;
    public int $term_taxonomy_id = 0;
    public string $taxonomy = 'category';
    public string $slug = '';
    public int $parent = 0;
    public int $count = 1;

    public function __construct(array $f = [])
    {
        foreach ($f as $k => $v) {
            $this->$k = $v;
        }
        $this->term_taxonomy_id = $this->term_taxonomy_id ?: $this->term_id;
    }
}

class WP_Comment
{
    public string $comment_ID = '0';
    public string $comment_post_ID = '0';
    public string $comment_approved = '1';

    public function __construct(array $f = [])
    {
        foreach ($f as $k => $v) {
            $this->$k = $v;
        }
    }
}

class WP_User
{
    public string $user_login;
    public string $user_email;

    public function __construct(public int $ID, public array $caps = [], public array $roles = [])
    {
        $this->user_login = 'user' . $ID;
        $this->user_email = 'user' . $ID . '@example.com';
    }

    public function has_cap($cap) { return !empty($this->caps[$cap]); }
}

class WP_Error
{
    public array $errors = [];

    public function __construct(public string $code = '', public string $message = '')
    {
        if ($code !== '') {
            $this->errors[$code][] = $message;
        }
    }

    public function add($code, $message) { $this->errors[$code][] = $message; }
}

/** The site the stand-in serves. Tests reset and fill it. */
final class Site
{
    public static array $options = [];
    /** @var array<int, WP_Post> */
    public static array $posts = [];
    /** @var array<int, WP_Term> */
    public static array $terms = [];
    /** @var array<int, array<string, list<int>>> post id => taxonomy => term ids */
    public static array $relations = [];
    /** taxonomy => [object types, viewable, hierarchical] */
    public static array $taxonomies = [];
    /** post type => [viewable, hierarchical, has_archive] */
    public static array $types = [];
    public static array $users = [];
    public static array $comments = [];
    public static array $hooks = [];
    public static array $requests = [];
    public static array $dequeued = [];
    public static bool $loggedIn = false;
    /** user id => meta key => value */
    public static array $meta = [];
    public static array $mails = [];
    public static bool $mailWorks = true;
    public static int $current = 0;
    public static array $actions = [];
    public static bool $isCart = false;
    public static string $environment = 'production';

    public static function reset(): void
    {
        self::$options = [
            'home' => 'https://example.com',
            'permalink_structure' => '/%postname%/',
            'posts_per_page' => 10,
            'show_on_front' => 'posts',
            'page_on_front' => 0,
            'page_for_posts' => 0,
        ];
        self::$posts = [];
        self::$terms = [];
        self::$relations = [];
        self::$taxonomies = [
            'category' => [['post'], true, true],
            'post_tag' => [['post'], true, false],
            'post_format' => [['post'], false, false],
            'product_cat' => [['product'], true, true],
            'product_tag' => [['product'], true, false],
        ];
        self::$types = [
            'post' => [true, false, false],
            'page' => [true, true, false],
            'product' => [true, false, true],
            'product_variation' => [false, false, false],
            'wp_template_part' => [false, false, false],
            'wp_block' => [false, false, false],
            'nav_menu_item' => [false, false, false],
            'revision' => [false, false, false],
        ];
        self::$users = [1 => new WP_User(1, ['manage_options' => true, 'edit_posts' => true, 'read' => true], ['administrator']), 2 => new WP_User(2, [])];
        self::$meta = [];
        self::$mails = [];
        self::$mailWorks = true;
        self::$current = 0;
        self::$actions = [];
        self::$comments = [];
        self::$requests = [];
        self::$dequeued = [];
        self::$loggedIn = false;
        self::$isCart = false;
        \CloudGround\Connector\Queue::reset();
    }

    public static function post(array $f, array $terms = []): WP_Post
    {
        $p = new WP_Post($f);
        self::$posts[$p->ID] = $p;
        foreach ($terms as $tax => $ids) {
            self::$relations[$p->ID][$tax] = $ids;
        }
        return $p;
    }

    public static function term(array $f): WP_Term
    {
        $t = new WP_Term($f);
        self::$terms[$t->term_id] = $t;
        return $t;
    }
}

// ---------------------------------------------------------------- hooks --

function add_action($hook, $cb, $prio = 10, $args = 1) { return add_filter($hook, $cb, $prio, $args); }
function add_filter($hook, $cb, $prio = 10, $args = 1)
{
    Site::$hooks[$hook][$prio][] = [$cb, $args];
    return true;
}
function has_action($hook) { return !empty(Site::$hooks[$hook]); }
function apply_filters($hook, $value, ...$rest)
{
    $all = Site::$hooks[$hook] ?? [];
    ksort($all);
    foreach ($all as $cbs) {
        foreach ($cbs as [$cb, $n]) {
            $value = $cb(...array_slice([$value, ...$rest], 0, $n));
        }
    }
    return $value;
}
function did_action($hook) { return Site::$actions[$hook] ?? 0; }
function do_action($hook, ...$args)
{
    Site::$actions[$hook] = (Site::$actions[$hook] ?? 0) + 1;
    $all = Site::$hooks[$hook] ?? [];
    ksort($all);
    foreach ($all as $cbs) {
        foreach ($cbs as [$cb, $n]) {
            $cb(...array_slice($args, 0, $n));
        }
    }
}
function __return_true() { return true; }

// ------------------------------------------------------------- options --

function get_option($name, $default = false) { return Site::$options[$name] ?? $default; }
function home_url($path = '') { return rtrim(Site::$options['home'], '/') . '/' . ltrim($path, '/'); }
function admin_url($path = '') { return home_url('/wp-admin/' . ltrim($path, '/')); }
function trailingslashit($s) { return rtrim($s, '/\\') . '/'; }
function user_trailingslashit($s, $type = '') { return trailingslashit($s); }
function pretty(): bool { return (string) get_option('permalink_structure') !== ''; }

// --------------------------------------------------------------- posts --

function get_post($id)
{
    if ($id instanceof WP_Post) {
        return $id;
    }
    return Site::$posts[(int) $id] ?? null;
}
function get_post_type($id) { $p = get_post($id); return $p ? $p->post_type : false; }
function is_post_type_viewable($type) { return (bool) (Site::$types[$type][0] ?? false); }
function is_post_type_hierarchical($type) { return (bool) (Site::$types[$type][1] ?? false); }
function wp_is_post_revision($post) { return get_post($post)?->post_type === 'revision'; }
function wp_is_post_autosave($post) { return false; }
function get_permalink($post = 0)
{
    $p = get_post($post);
    if (!$p) {
        return false;
    }
    if (!pretty() || $p->post_status !== 'publish') {
        return home_url('/?p=' . $p->ID);
    }
    $path = $p->post_name;
    for ($parent = $p->post_parent; $parent; $parent = Site::$posts[$parent]->post_parent ?? 0) {
        $path = (Site::$posts[$parent]->post_name ?? '') . '/' . $path;
    }
    $base = $p->post_type === 'product' ? 'product/' : '';
    return home_url('/' . $base . $path . '/');
}
function get_post_comments_feed_link($id = 0) { $l = get_permalink($id); return $l && pretty() ? trailingslashit($l) . 'feed/' : $l . '&feed=rss2'; }
function get_feed_link($feed = '') { $f = $feed === 'comments_rss2' ? 'comments/feed/' : 'feed/'; return pretty() ? home_url('/' . $f) : home_url('/?feed=' . ($feed ?: 'rss2')); }
function get_post_type_archive_link($type)
{
    if ($type === 'post') {
        $blog = (int) get_option('page_for_posts');
        return get_option('show_on_front') === 'page' && $blog ? get_permalink($blog) : home_url('/');
    }
    if (empty(Site::$types[$type][2])) {
        return false;
    }
    return pretty() ? home_url('/' . ($type === 'product' ? 'shop' : $type) . '/') : home_url('/?post_type=' . $type);
}
function get_post_type_archive_feed_link($type) { $l = get_post_type_archive_link($type); return $l ? (pretty() ? trailingslashit($l) . 'feed/' : $l . '&feed=rss2') : false; }
function wp_count_posts($type)
{
    $n = 0;
    foreach (Site::$posts as $p) {
        $n += ($p->post_type === $type && $p->post_status === 'publish') ? 1 : 0;
    }
    return (object) ['publish' => Site::$options['count_' . $type] ?? $n];
}
function get_posts($args)
{
    $out = [];
    foreach (Site::$posts as $p) {
        if ($p->post_parent === ($args['post_parent'] ?? -1) && $p->post_type === ($args['post_type'] ?? 'post')) {
            $out[] = $p->ID;
        }
    }
    return $out;
}
function get_author_posts_url($id) { return home_url('/author/user' . $id . '/'); }
function get_author_feed_link($id) { return home_url('/author/user' . $id . '/feed/'); }
function count_user_posts($id, $type = 'post', $public = false) { return Site::$options['count_author'] ?? 1; }
function get_year_link($y) { return home_url(sprintf('/%04d/', $y)); }
function get_month_link($y, $m) { return home_url(sprintf('/%04d/%02d/', $y, $m)); }
function get_day_link($y, $m, $d) { return home_url(sprintf('/%04d/%02d/%02d/', $y, $m, $d)); }
function wp_sitemaps_get_max_urls($type) { return 2000; }

// --------------------------------------------------------------- terms --

function get_object_taxonomies($type)
{
    $out = [];
    foreach (Site::$taxonomies as $tax => [$types]) {
        if (in_array($type, $types, true)) {
            $out[] = $tax;
        }
    }
    return $out;
}
function is_taxonomy_viewable($tax) { return (bool) (Site::$taxonomies[$tax][1] ?? false); }
function get_the_terms($post, $tax)
{
    $ids = Site::$relations[get_post($post)->ID][$tax] ?? [];
    return $ids === [] ? false : array_map(fn ($id) => Site::$terms[$id], $ids);
}
function get_term($id, $tax = '') { return Site::$terms[(int) $id] ?? null; }
function get_term_by($field, $value, $tax)
{
    foreach (Site::$terms as $t) {
        if ($field === 'term_taxonomy_id' && $t->term_taxonomy_id === (int) $value && $t->taxonomy === $tax) {
            return $t;
        }
    }
    return false;
}
function get_ancestors($id, $tax, $type = '')
{
    $out = [];
    for ($p = Site::$terms[$id]->parent ?? 0; $p; $p = Site::$terms[$p]->parent ?? 0) {
        $out[] = $p;
    }
    return $out;
}
function get_term_link($term)
{
    $base = ['category' => 'category', 'post_tag' => 'tag', 'product_cat' => 'product-category', 'product_tag' => 'product-tag'][$term->taxonomy] ?? $term->taxonomy;
    return pretty() ? home_url('/' . $base . '/' . $term->slug . '/') : home_url('/?' . $term->taxonomy . '=' . $term->slug);
}
function get_term_feed_link($id, $tax) { return trailingslashit(get_term_link(Site::$terms[$id])) . 'feed/'; }

// ------------------------------------------------------------ comments --

function get_comment($id) { return Site::$comments[(int) $id] ?? null; }

// --------------------------------------------------------------- users --

function get_userdata($id) { return Site::$users[(int) $id] ?? false; }
function get_user_meta($id, $key, $single = false) { return Site::$meta[(int) $id][$key] ?? ''; }
function update_user_meta($id, $key, $value) { Site::$meta[(int) $id][$key] = $value; return true; }
function delete_user_meta($id, $key)
{
    if (!isset(Site::$meta[(int) $id][$key])) {
        return false;
    }
    unset(Site::$meta[(int) $id][$key]);
    return true;
}
function get_current_user_id() { return Site::$current; }
function wp_get_current_user() { return Site::$users[Site::$current] ?? new WP_User(0); }
function current_user_can($cap, ...$args) { return !empty(Site::$users[Site::$current]->caps[$cap]) || ($cap === 'edit_user' && Site::$current === (int) ($args[0] ?? 0)); }
function wp_salt($scheme = 'auth') { return 'salt-' . $scheme . (Site::$options['salt_suffix'] ?? ''); }
function wp_mail($to, $subject, $body) { Site::$mails[] = [$to, $subject, $body]; return Site::$mailWorks; }
function wp_nonce_field($action, $name) { echo '<input type="hidden" name="' . $name . '" value="' . wp_create_nonce($action) . '">'; }
function wp_create_nonce($action) { return substr(hash('sha256', $action . Site::$current), 0, 10); }
function wp_verify_nonce($nonce, $action) { return hash_equals(wp_create_nonce($action), (string) $nonce) ? 1 : false; }
function user_can($user, $cap) { return !empty($user->caps[$cap]); }
function is_user_logged_in() { return Site::$loggedIn; }

// ---------------------------------------------------------- the request --

function wp_remote_post($url, $args) { Site::$requests[] = [$url, $args]; return ['response' => ['code' => 200]]; }
function wp_json_encode($v) { return json_encode($v); }
function status_header($code) {}
function nocache_headers() {}
function wp_set_auth_cookie($user, $remember = false, $secure = '') {}
function esc_html($s) { return htmlspecialchars((string) $s, ENT_QUOTES); }
function esc_attr($s) { return htmlspecialchars((string) $s, ENT_QUOTES); }
function esc_url($s) { return htmlspecialchars((string) $s, ENT_QUOTES); }
function __($s, $domain = 'default') { return $s; }
function _n($one, $many, $n, $domain = 'default') { return $n === 1 ? $one : $many; }
function esc_html__($s, $domain = 'default') { return esc_html($s); }
function esc_attr__($s, $domain = 'default') { return esc_attr($s); }
function wp_unslash($v) { return is_array($v) ? array_map('wp_unslash', $v) : (is_string($v) ? stripslashes($v) : $v); }
function wp_specialchars_decode($s, $q = ENT_NOQUOTES) { return htmlspecialchars_decode($s, $q); }
function get_bloginfo($what) { return 'Example'; }
function site_url($path = '', $scheme = null) { return home_url('/' . ltrim($path, '/')); }
function wp_login_url() { return site_url('wp-login.php'); }
function wp_parse_url($url, $component = -1) { return parse_url($url, $component); }
function wp_doing_ajax() { return false; }
function wp_validate_redirect($location, $fallback = '')
{
    $host = parse_url($location, PHP_URL_HOST);
    return $location !== '' && ($host === null || $host === 'example.com') ? $location : $fallback;
}
function wp_cache_delete($key, $group = '') { return true; }
function get_num_queries() { return 7; }
function wp_get_environment_type() { return Site::$environment; }
function is_cart() { return Site::$isCart; }
function is_checkout() { return false; }
function wp_dequeue_script($h) { Site::$dequeued[] = $h; }

// --------------------------------------------------------- WooCommerce --

function wc_get_page_permalink($page) { return home_url('/shop/'); }

final class FakeProduct
{
    public function __construct(private int $id) {}
    public function get_id(): int { return $this->id; }
}

// ------------------------------------------------------------- database --

/** The options table, enough for the login row. */
final class FakeWpdb
{
    public string $options = 'wp_options';
    public array $rows = [];
    public array $queries = [];

    public function prepare($sql, ...$args)
    {
        return vsprintf(str_replace('%s', "'%s'", $sql), array_map('addslashes', $args));
    }

    public function get_row($sql)
    {
        $this->queries[] = $sql;
        if (preg_match("/WHERE option_name = '([^']*)'/", $sql, $m) && isset($this->rows[$m[1]])) {
            return (object) ['option_name' => $m[1], 'option_value' => $this->rows[$m[1]]];
        }
        return null;
    }

    public function query($sql)
    {
        $this->queries[] = $sql;
        if (preg_match("/^DELETE FROM wp_options WHERE option_name = '([^']*)'$/", $sql, $m)) {
            if (isset($this->rows[$m[1]])) {
                unset($this->rows[$m[1]]);
                return 1;
            }
            return 0;
        }
        return false;
    }
}

$GLOBALS['wpdb'] = new FakeWpdb();
$GLOBALS['wp_rewrite'] = (object) ['pagination_base' => 'page'];
