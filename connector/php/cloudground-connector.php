<?php
/**
 * Plugin Name: CloudGround Connector
 * Description: Purges exactly the cached pages a change affects, signs the panel in, reports the object cache, and adds two-factor sign-in when the panel turns it on. Managed by CloudGround.
 * Version: 2.2.0
 * Requires at least: 6.6
 * Requires PHP: 8.2
 * Author: CloudGround
 * License: GPL-2.0-or-later
 * License URI: https://www.gnu.org/licenses/old-licenses/gpl-2.0.html
 *
 * The contract is connector/SPEC.md in the CloudGround repository.
 *
 * Copyright (C) 2026 CloudGround contributors
 * SPDX-License-Identifier: GPL-2.0-or-later
 *
 * This program is free software; you can redistribute it and/or modify it
 * under the terms of the GNU General Public License as published by the Free
 * Software Foundation; either version 2 of the License, or (at your option)
 * any later version. It is distributed WITHOUT ANY WARRANTY; see the GNU
 * General Public License for more details.
 */

namespace CloudGround\Connector;

defined('ABSPATH') || exit;

/** The constants the agent writes into wp-config.php (SPEC 1). */
final class Config
{
    public static function token(): string
    {
        return defined('CLOUDGROUND_SITE_TOKEN') ? (string) \CLOUDGROUND_SITE_TOKEN : '';
    }

    public static function endpoint(): string
    {
        $url = defined('CLOUDGROUND_ENDPOINT') ? (string) \CLOUDGROUND_ENDPOINT : '';
        return rtrim($url !== '' ? $url : 'http://127.0.0.1:9080', '/');
    }

    /** How many pages of a listing a change purges. */
    public static function pages(): int
    {
        return defined('CLOUDGROUND_PURGE_PAGES') ? max(0, min(50, (int) \CLOUDGROUND_PURGE_PAGES)) : 10;
    }
}

/**
 * The URLs one request has asked to purge, sent once when it ends
 * (SPEC 2.1).
 */
final class Queue
{
    public const LIMIT = 2000;

    /** @var array<string, true> */
    private static array $urls = [];
    private static bool $all = false;
    private static bool $armed = false;

    /** @param iterable<mixed> $urls */
    public static function add(iterable $urls): void
    {
        foreach ($urls as $url) {
            if (is_string($url) && $url !== '') {
                self::$urls[$url] = true;
            }
        }
        self::arm();
    }

    public static function everything(): void
    {
        self::$all = true;
        self::arm();
    }

    private static function arm(): void
    {
        if (!self::$armed) {
            self::$armed = true;
            add_action('shutdown', [self::class, 'send'], PHP_INT_MAX);
        }
    }

    /** The body to send, or null when there is nothing to purge. */
    public static function payload(): ?array
    {
        $urls = array_keys(self::$urls);
        if (!self::$all && $urls !== []) {
            $urls = array_values(array_unique(array_filter((array) apply_filters('cloudground_purge_urls', $urls), 'is_string')));
        }
        $all = (bool) apply_filters('cloudground_purge_all', self::$all || count($urls) > self::LIMIT);
        if ($all) {
            return ['all' => true];
        }
        return $urls === [] ? null : ['urls' => $urls];
    }

    public static function send(): void
    {
        $payload = self::payload();
        self::reset();
        $token = Config::token();
        if ($payload === null || $token === '') {
            return;
        }
        if (function_exists('fastcgi_finish_request')) {
            fastcgi_finish_request();
        }
        wp_remote_post(Config::endpoint() . '/api/connector/purge', [
            'timeout' => 3,
            'blocking' => true,
            'redirection' => 0,
            'headers' => [
                'Authorization' => 'Bearer ' . $token,
                'Content-Type' => 'application/json',
            ],
            'body' => wp_json_encode($payload),
        ]);
    }

    public static function reset(): void
    {
        self::$urls = [];
        self::$all = false;
    }
}

/** Which URLs show a given thing (SPEC 2.3). Pure reads of WordPress. */
final class Targets
{
    /** Post types whose objects render on any URL of the site. */
    public const SITEWIDE = ['wp_template', 'wp_template_part', 'wp_global_styles', 'wp_navigation', 'wp_block'];

    /** Every URL a published post appears on. @return list<string> */
    public static function post(\WP_Post $post): array
    {
        if (!is_post_type_viewable($post->post_type)) {
            return [];
        }
        $type = $post->post_type;
        $per = self::perPage($type);
        $u = [get_permalink($post), get_post_comments_feed_link($post->ID), home_url('/'), get_feed_link()];

        $front = get_option('show_on_front') === 'page' ? (int) get_option('page_on_front') : 0;
        $blog = (int) get_option('page_for_posts');
        if ($front > 0) {
            $u[] = get_permalink($front);
        }
        if ($type === 'post') {
            $count = self::published('post');
            if ($blog > 0) {
                array_push($u, ...self::pages(get_permalink($blog), $count, $per));
            } elseif ($front === 0) {
                array_push($u, ...self::pages(home_url('/'), $count, $per));
            }
        }

        // A post's type archive is the home page or the posts page, done above.
        $archive = $type === 'post' ? false : get_post_type_archive_link($type);
        if (is_string($archive)) {
            array_push($u, ...self::pages($archive, self::published($type), $per));
            $u[] = get_post_type_archive_feed_link($type);
        }
        if ($type === 'product' && function_exists('wc_get_page_permalink')) {
            array_push($u, ...self::pages(wc_get_page_permalink('shop'), self::published('product'), $per));
        }

        foreach (self::taxonomies($type) as $tax) {
            $terms = get_the_terms($post, $tax);
            foreach (is_array($terms) ? $terms : [] as $term) {
                array_push($u, ...self::term($term, $per));
            }
        }

        if ($type === 'post') {
            $author = (int) $post->post_author;
            if ($author > 0) {
                array_push($u, ...self::pages(get_author_posts_url($author), (int) count_user_posts($author, 'post', true), $per));
                $u[] = get_author_feed_link($author);
            }
            // post_date is the site's local time, which is what the date
            // archives are built from.
            if (sscanf((string) $post->post_date, '%d-%d-%d', $y, $m, $d) === 3 && $y > 0) {
                $count = self::published('post');
                foreach ([get_year_link($y), get_month_link($y, $m), get_day_link($y, $m, $d)] as $link) {
                    array_push($u, ...self::pages($link, $count, $per));
                }
            }
        }

        array_push($u, ...self::sitemaps($type));
        return self::clean($u);
    }

    /** A term's archive and its ancestors'. @return list<string> */
    public static function term(\WP_Term $term, ?int $per = null): array
    {
        $per ??= self::perPage(str_starts_with($term->taxonomy, 'pa_') || in_array($term->taxonomy, ['product_cat', 'product_tag'], true) ? 'product' : 'post');
        $u = [];
        $chain = [$term];
        foreach (get_ancestors($term->term_id, $term->taxonomy, 'taxonomy') as $id) {
            $parent = get_term((int) $id, $term->taxonomy);
            if ($parent instanceof \WP_Term) {
                $chain[] = $parent;
            }
        }
        foreach ($chain as $t) {
            $link = get_term_link($t);
            if (is_string($link)) {
                array_push($u, ...self::pages($link, (int) $t->count, $per));
                $u[] = get_term_feed_link($t->term_id, $t->taxonomy);
            }
        }
        return self::clean($u);
    }

    /** @return list<string> */
    public static function comment(\WP_Comment $comment): array
    {
        $post = get_post((int) $comment->comment_post_ID);
        if (!$post instanceof \WP_Post || $post->post_status !== 'publish' || !is_post_type_viewable($post->post_type)) {
            return [];
        }
        return self::clean([get_permalink($post), get_post_comments_feed_link($post->ID), get_feed_link('comments_rss2')]);
    }

    /** The product page itself. @return list<string> */
    public static function productPage(\WP_Post $post): array
    {
        return self::clean([get_permalink($post)]);
    }

    /** @return list<string> */
    public static function taxonomies(string $type): array
    {
        $out = [];
        foreach (get_object_taxonomies($type) as $tax) {
            if (is_taxonomy_viewable($tax)) {
                $out[] = $tax;
            }
        }
        return $out;
    }

    /** A listing URL and its next pages, up to the cap. @return list<string> */
    public static function pages(mixed $base, int $count, int $per): array
    {
        if (!is_string($base) || $base === '') {
            return [];
        }
        $out = [$base];
        if (!self::pretty()) {
            return $out;
        }
        global $wp_rewrite;
        $slug = isset($wp_rewrite->pagination_base) ? (string) $wp_rewrite->pagination_base : 'page';
        $last = min(Config::pages(), (int) ceil($count / max(1, $per)));
        for ($n = 2; $n <= $last; $n++) {
            $out[] = trailingslashit($base) . user_trailingslashit($slug . '/' . $n, 'paged');
        }
        return $out;
    }

    /** @return list<string> */
    private static function sitemaps(string $type): array
    {
        if (!self::pretty()) {
            return [];
        }
        $per = function_exists('wp_sitemaps_get_max_urls') ? max(1, (int) wp_sitemaps_get_max_urls('post')) : 2000;
        $u = [home_url('/wp-sitemap.xml'), home_url('/wp-sitemap-users-1.xml')];
        $last = max(1, (int) ceil(self::published($type) / $per));
        for ($n = 1; $n <= $last; $n++) {
            $u[] = home_url("/wp-sitemap-posts-{$type}-{$n}.xml");
        }
        foreach (self::taxonomies($type) as $tax) {
            $u[] = home_url("/wp-sitemap-taxonomies-{$tax}-1.xml");
        }
        return $u;
    }

    private static function perPage(string $type): int
    {
        $per = (int) get_option('posts_per_page');
        if ($type === 'product' && function_exists('wc_get_default_products_per_row') && function_exists('wc_get_default_product_rows_per_page')) {
            $per = (int) apply_filters('loop_shop_per_page', wc_get_default_products_per_row() * wc_get_default_product_rows_per_page());
        }
        return max(1, $per);
    }

    private static function published(string $type): int
    {
        $counts = wp_count_posts($type);
        return (int) ($counts->publish ?? 0);
    }

    private static function pretty(): bool
    {
        return (string) get_option('permalink_structure') !== '';
    }

    /**
     * Absolute URLs only, once each. A URL with a query string is dropped:
     * the page cache never stores one (a plain permalink, a feed without
     * pretty permalinks), so there is nothing to purge.
     *
     * @param array<mixed> $urls
     * @return list<string>
     */
    private static function clean(array $urls): array
    {
        $out = [];
        foreach ($urls as $u) {
            if (!is_string($u) || !str_contains($u, '://') || str_contains($u, '?')) {
                continue;
            }
            // "https://host" is the same page as "https://host/".
            $out[preg_match('#\A[a-z]+://[^/]+\z#i', $u) === 1 ? $u . '/' : $u] = true;
        }
        return array_keys($out);
    }
}

/** WordPress events mapped to purges (SPEC 2.2). */
final class Events
{
    /** Options that change what every page shows. */
    public const SITEWIDE_OPTIONS = [
        'blogname', 'blogdescription', 'permalink_structure', 'category_base', 'tag_base',
        'show_on_front', 'page_on_front', 'page_for_posts', 'posts_per_page', 'date_format',
        'time_format', 'start_of_week', 'WPLANG', 'site_icon', 'blog_public', 'sidebars_widgets',
        'woocommerce_currency', 'woocommerce_currency_pos', 'woocommerce_price_thousand_sep',
        'woocommerce_price_decimal_sep', 'woocommerce_price_num_decimals', 'woocommerce_shop_page_id',
        'woocommerce_catalog_columns', 'woocommerce_catalog_rows', 'woocommerce_default_catalog_orderby',
        'woocommerce_shop_page_display', 'woocommerce_category_archive_display',
        'woocommerce_hide_out_of_stock_items', 'woocommerce_tax_display_shop', 'woocommerce_prices_include_tax',
    ];

    /** WooCommerce product props that listings show. */
    public const LISTED_PROPS = [
        'regular_price', 'sale_price', 'date_on_sale_from', 'date_on_sale_to', 'stock_status',
        'product_type', 'image_id', 'average_rating', 'rating_counts', 'review_count',
    ];

    public const PRICE_META = ['_price', '_regular_price', '_sale_price', '_stock_status'];

    public static function register(): void
    {
        add_action('transition_post_status', [self::class, 'transition'], 10, 3);
        add_action('post_updated', [self::class, 'updated'], 10, 3);
        add_action('before_delete_post', [self::class, 'deleting'], 10, 2);
        add_action('set_object_terms', [self::class, 'terms'], 10, 6);

        add_action('comment_post', [self::class, 'commentPosted'], 10, 2);
        add_action('transition_comment_status', [self::class, 'commentStatus'], 10, 3);
        add_action('edit_comment', [self::class, 'commentEdited'], 10, 1);

        add_action('edited_term', [self::class, 'termChanged'], 10, 3);
        add_action('delete_term', [self::class, 'termChanged'], 10, 3);

        foreach (['switch_theme', 'activated_plugin', 'deactivated_plugin', 'upgrader_process_complete',
            'wp_update_nav_menu', 'wp_delete_nav_menu', 'customize_save_after',
            'woocommerce_attribute_added', 'woocommerce_attribute_updated', 'woocommerce_attribute_deleted'] as $hook) {
            add_action($hook, [Queue::class, 'everything'], 10, 0);
        }
        add_action('added_option', [self::class, 'optionAdded'], 10, 2);
        add_action('updated_option', [self::class, 'optionUpdated'], 10, 3);
        add_action('deleted_option', [self::class, 'option'], 10, 1);

        add_action('woocommerce_product_object_updated_props', [self::class, 'productProps'], 10, 2);
        add_action('woocommerce_product_set_stock', [self::class, 'stock'], 10, 1);
        add_action('woocommerce_variation_set_stock', [self::class, 'stock'], 10, 1);
        add_action('woocommerce_product_set_stock_status', [self::class, 'stockStatus'], 10, 3);
        add_action('woocommerce_variation_set_stock_status', [self::class, 'stockStatus'], 10, 3);
        foreach (['added_post_meta', 'updated_post_meta', 'deleted_post_meta'] as $hook) {
            add_action($hook, [self::class, 'meta'], 10, 3);
        }
    }

    public static function transition(string $new, string $old, \WP_Post $post): void
    {
        if ($post->post_type === 'wp_navigation' && $old === 'new' && self::pageView()) {
            // WordPress's fallback menu: a Navigation block with no menu
            // saves the blocks it falls back to as a new wp_navigation
            // while it renders (WP_Navigation_Fallback, the first view of
            // a fresh site). The page shows what it showed; purging
            // everything emptied the cache that view had just filled.
            return;
        }
        if ($new === 'publish' || $old === 'publish') {
            self::post($post);
        }
    }

    /** Set by the tests: whether this request is a visitor's page view. */
    public static ?bool $pageView = null;

    /**
     * Whether this request is a visitor's page view: a GET or HEAD of the
     * front end, not the admin, the REST API, Ajax, cron or wp-cli, which
     * is where a menu is made on purpose.
     */
    private static function pageView(): bool
    {
        if (self::$pageView !== null) {
            return self::$pageView;
        }
        if (PHP_SAPI === 'cli' || (defined('WP_CLI') && WP_CLI) || (defined('REST_REQUEST') && REST_REQUEST)
            || is_admin() || wp_doing_ajax() || wp_doing_cron()) {
            return false;
        }
        return in_array(strtoupper((string) ($_SERVER['REQUEST_METHOD'] ?? '')), ['GET', 'HEAD'], true);
    }

    /** The old URLs: a slug, date, author or parent change moves the post. */
    public static function updated(int $id, \WP_Post $after, \WP_Post $before): void
    {
        if ($before->post_status !== 'publish') {
            return;
        }
        self::post($before);
        if (is_post_type_hierarchical($before->post_type) && $before->post_status === 'publish'
            && get_permalink($before) !== get_permalink($after)) {
            $children = get_posts(['post_type' => $before->post_type, 'post_parent' => $id, 'post_status' => 'any', 'fields' => 'ids', 'numberposts' => 1]);
            if ($children !== []) {
                Queue::everything();
            }
        }
    }

    public static function deleting(int $id, ?\WP_Post $post = null): void
    {
        $post ??= get_post($id);
        if ($post instanceof \WP_Post && $post->post_status === 'publish') {
            self::post($post);
        }
    }

    /** @param array<int> $tt_ids @param array<int> $old_tt_ids */
    public static function terms(int $object_id, $terms, array $tt_ids, string $taxonomy, bool $append, array $old_tt_ids): void
    {
        $tt_ids = array_map('intval', $tt_ids);
        $old_tt_ids = array_map('intval', $old_tt_ids);
        $changed = array_merge(array_diff($tt_ids, $old_tt_ids), array_diff($old_tt_ids, $tt_ids));
        if ($changed === [] || !is_taxonomy_viewable($taxonomy)) {
            return;
        }
        $post = get_post($object_id);
        if (!$post instanceof \WP_Post || $post->post_status !== 'publish' || !is_post_type_viewable($post->post_type)) {
            return;
        }
        $urls = Targets::productPage($post);
        foreach ($changed as $tt_id) {
            $term = get_term_by('term_taxonomy_id', $tt_id, $taxonomy);
            if ($term instanceof \WP_Term) {
                array_push($urls, ...Targets::term($term));
            }
        }
        Queue::add($urls);
    }

    public static function post(\WP_Post $post): void
    {
        if (wp_is_post_revision($post) || wp_is_post_autosave($post)) {
            return;
        }
        if (in_array($post->post_type, Targets::SITEWIDE, true)) {
            Queue::everything();
            return;
        }
        Queue::add(Targets::post($post));
    }

    public static function commentPosted(int $id, $approved): void
    {
        if ($approved === 1 || $approved === '1') {
            self::comment($id);
        }
    }

    public static function commentStatus(string $new, string $old, \WP_Comment $comment): void
    {
        if ($new === 'approved' || $old === 'approved') {
            Queue::add(Targets::comment($comment));
        }
    }

    public static function commentEdited(int $id): void
    {
        $comment = get_comment($id);
        if ($comment instanceof \WP_Comment && (string) $comment->comment_approved === '1') {
            Queue::add(Targets::comment($comment));
        }
    }

    private static function comment(int $id): void
    {
        $comment = get_comment($id);
        if ($comment instanceof \WP_Comment) {
            Queue::add(Targets::comment($comment));
        }
    }

    public static function termChanged(int $term_id, int $tt_id, string $taxonomy): void
    {
        if (is_taxonomy_viewable($taxonomy)) {
            Queue::everything();
        }
    }

    public static function option(string $name): void
    {
        if (in_array($name, self::SITEWIDE_OPTIONS, true) || str_starts_with($name, 'theme_mods_') || str_starts_with($name, 'widget_')) {
            Queue::everything();
        }
    }

    /** @param mixed $value */
    public static function optionAdded(string $name, $value = null): void
    {
        if (!self::customCSSLookup($name, false, $value)) {
            self::option($name);
        }
    }

    /**
     * @param mixed $old
     * @param mixed $new
     */
    public static function optionUpdated(string $name, $old = null, $new = null): void
    {
        if (!self::customCSSLookup($name, $old, $new)) {
            self::option($name);
        }
    }

    /**
     * Whether a theme mods write only stores the Custom CSS post's id for the
     * first time. WordPress looks that post up while it renders a page
     * (wp_get_custom_css_post, from wp_head) and keeps the answer, -1 for
     * none, as the theme mod custom_css_post_id: the first view of a fresh
     * site writes it, and purging everything then emptied the cache that
     * view had just filled. Nothing a page shows changes. A Custom CSS edit
     * is customize_save_after, or changes an id already stored.
     *
     * @param mixed $old
     * @param mixed $new
     */
    private static function customCSSLookup(string $name, $old, $new): bool
    {
        if (!str_starts_with($name, 'theme_mods_') || !is_array($new) || !array_key_exists('custom_css_post_id', $new)) {
            return false;
        }
        $old = is_array($old) ? $old : [];
        if (array_key_exists('custom_css_post_id', $old)) {
            return false;
        }
        unset($new['custom_css_post_id']);
        return $old == $new;
    }

    /** @param array<string> $props */
    public static function productProps($product, $props): void
    {
        $props = array_values(array_diff((array) $props, ['total_sales']));
        if ($props === []) {
            return;
        }
        $post = self::productPost($product);
        if ($post === null) {
            return;
        }
        Queue::add(array_intersect($props, self::LISTED_PROPS) !== [] ? Targets::post($post) : Targets::productPage($post));
    }

    public static function stock($product): void
    {
        $post = self::productPost($product);
        if ($post !== null) {
            Queue::add(Targets::productPage($post));
        }
    }

    public static function stockStatus($id, $status = null, $product = null): void
    {
        $post = self::productPost($product ?? $id);
        if ($post !== null) {
            Queue::add(Targets::post($post));
        }
    }

    public static function meta($meta_id, $object_id, $key): void
    {
        if ($key !== '_stock' && !in_array($key, self::PRICE_META, true)) {
            return;
        }
        $type = get_post_type((int) $object_id);
        if ($type !== 'product' && $type !== 'product_variation') {
            return;
        }
        $post = self::productPost((int) $object_id);
        if ($post !== null) {
            Queue::add($key === '_stock' ? Targets::productPage($post) : Targets::post($post));
        }
    }

    /** The published product behind a product, a variation or an ID. */
    private static function productPost($product): ?\WP_Post
    {
        $id = is_object($product) && method_exists($product, 'get_id') ? (int) $product->get_id() : (int) $product;
        $post = $id > 0 ? get_post($id) : null;
        if ($post instanceof \WP_Post && $post->post_type === 'product_variation') {
            $post = get_post((int) $post->post_parent);
        }
        if (!$post instanceof \WP_Post || $post->post_status !== 'publish') {
            return null;
        }
        return $post;
    }
}

/** A response an endpoint decided on; emit() sends it and ends the request. */
final class Reply
{
    /** @param array<string, string> $headers */
    /** @var array<string, array{0: string, 1: int}> name => [value, expiry] */
    public array $cookies = [];
    /** Whether a sign-in (user > 0) is remembered past the browser session. */
    public bool $remember = false;
    /** Run once the auth cookie is set: the actions a sign-in fires. */
    public ?\Closure $then = null;

    public function __construct(
        public int $status,
        public string $body = '',
        public array $headers = [],
        public int $user = 0,
    ) {
    }

    public static function json(int $status, array $data): self
    {
        return new self($status, (string) wp_json_encode($data), ['Content-Type' => 'application/json', 'Cache-Control' => 'no-store']);
    }

    public function emit(): never
    {
        status_header($this->status);
        foreach ($this->headers as $name => $value) {
            header($name . ': ' . $value);
        }
        foreach ($this->cookies as $name => [$value, $expires]) {
            setcookie($name, $value, [
                'expires' => $expires,
                'path' => defined('COOKIEPATH') ? (string) \COOKIEPATH : '/',
                'domain' => defined('COOKIE_DOMAIN') && \COOKIE_DOMAIN ? (string) \COOKIE_DOMAIN : '',
                'secure' => true,
                'httponly' => true,
                'samesite' => 'Strict',
            ]);
        }
        if ($this->user > 0) {
            wp_set_auth_cookie($this->user, $this->remember, true);
            if ($this->then !== null) {
                ($this->then)();
            }
        }
        echo $this->body;
        exit;
    }
}

/** One-shot sign-in from the panel (SPEC 2.5). */
final class MagicLogin
{
    public const PREFIX = 'cloudground_login_';

    public static function handle(): void
    {
        if (!isset($_GET['cloudground_login'])) {
            return;
        }
        $method = strtoupper((string) ($_SERVER['REQUEST_METHOD'] ?? 'GET'));
        $reply = $method === 'POST'
            ? self::signIn(is_string($_POST['cloudground_token'] ?? null) ? $_POST['cloudground_token'] : '')
            : self::page();
        nocache_headers();
        $reply->headers += ['Referrer-Policy' => 'no-referrer', 'X-Robots-Tag' => 'noindex'];
        $reply->emit();
    }

    /** The page that posts the token from the fragment back. */
    public static function page(): Reply
    {
        $nonce = bin2hex(random_bytes(16));
        $html = '<!doctype html><html><head><meta charset="utf-8"><meta name="robots" content="noindex"><title>Signing in</title></head><body>'
            . '<form id="cg" method="post"><input type="hidden" name="cloudground_token" id="cgt"></form>'
            . '<script nonce="' . $nonce . '">(function(){var t=location.hash.slice(1);'
            . 'history.replaceState(null,"",location.pathname+location.search);'
            . 'if(!/^[0-9a-f]{48}$/.test(t)){document.body.textContent="Invalid login link.";return;}'
            . 'document.getElementById("cgt").value=t;document.getElementById("cg").submit();})();</script>'
            . '</body></html>';
        return new Reply(200, $html, [
            'Content-Type' => 'text/html; charset=utf-8',
            'Content-Security-Policy' => "default-src 'none'; script-src 'nonce-{$nonce}'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'",
        ]);
    }

    public static function signIn(string $token): Reply
    {
        if (preg_match('/\A[0-9a-f]{48}\z/', $token) !== 1) {
            return self::refuse(400, 'Invalid login link.');
        }
        global $wpdb;
        $name = self::PREFIX . hash('sha256', $token);
        // Straight from the database: the agent wrote the row with wp-cli,
        // whose object cache PHP-FPM never sees.
        $row = $wpdb->get_row($wpdb->prepare("SELECT option_name, option_value FROM {$wpdb->options} WHERE option_name = %s LIMIT 1", $name));
        if (!is_object($row) || !hash_equals($name, (string) $row->option_name)) {
            return self::expired();
        }
        // Consumed before anything else is decided: exactly one request can
        // delete the row.
        $deleted = $wpdb->query($wpdb->prepare("DELETE FROM {$wpdb->options} WHERE option_name = %s", $name));
        wp_cache_delete($name, 'options');
        if ($deleted !== 1) {
            return self::expired();
        }
        $parts = explode(':', (string) $row->option_value);
        $user = (int) ($parts[0] ?? 0);
        $expiry = (int) ($parts[1] ?? 0);
        if (count($parts) !== 2 || $user <= 0 || time() > $expiry) {
            return self::expired();
        }
        $who = get_userdata($user);
        if (!$who instanceof \WP_User || !user_can($who, 'manage_options')) {
            return self::expired();
        }
        return new Reply(302, '', ['Location' => admin_url()], $user);
    }

    private static function expired(): Reply
    {
        return self::refuse(403, 'This login link has expired. Generate a new one from the panel.');
    }

    private static function refuse(int $status, string $message): Reply
    {
        return new Reply($status, '<!doctype html><meta charset="utf-8"><title>Login link</title><p>' . esc_html($message) . '</p>', ['Content-Type' => 'text/html; charset=utf-8']);
    }
}

/** The object cache's numbers, for the panel (SPEC 2.6). */
final class Stats
{
    public static function handle(): void
    {
        if (empty($_GET['cloudground_stats'])) {
            return;
        }
        nocache_headers();
        self::answer((string) ($_SERVER['HTTP_X_CLOUDGROUND_TOKEN'] ?? ''))->emit();
    }

    public static function answer(string $sent): Reply
    {
        $token = Config::token();
        if ($token === '' || $sent === '' || !hash_equals($token, $sent)) {
            return Reply::json(403, ['error' => 'forbidden']);
        }
        if (!function_exists('apcu_enabled') || !apcu_enabled()) {
            return Reply::json(200, ['error' => 'apcu unavailable']);
        }
        $info = apcu_cache_info(true);
        $sma = apcu_sma_info(true);
        if (!is_array($info) || !is_array($sma)) {
            return Reply::json(200, ['error' => 'apcu unavailable']);
        }
        $total = (int) ($sma['seg_size'] ?? 0) * max(1, (int) ($sma['num_seg'] ?? 1));
        return Reply::json(200, [
            'hits' => (int) ($info['num_hits'] ?? 0),
            'misses' => (int) ($info['num_misses'] ?? 0),
            'entries' => (int) ($info['num_entries'] ?? 0),
            'mem_used' => max(0, $total - (int) ($sma['avail_mem'] ?? 0)),
            'mem_total' => $total,
        ]);
    }
}

/** The timing footer for Performance Guard's own requests (SPEC 2.7). */
final class Guard
{
    public static function footer(): void
    {
        echo self::comment((string) ($_SERVER['HTTP_X_CLOUDGROUND_PROBE'] ?? ''));
    }

    public static function comment(string $sent): string
    {
        $token = Config::token();
        if ($token === '' || $sent === '' || is_user_logged_in()) {
            return '';
        }
        if (!hash_equals(hash_hmac('sha256', 'guard-probe', $token), $sent)) {
            return '';
        }
        // Not timer_stop(): it formats for the site's locale, and "0,1234"
        // (Italian, German…) reads back as 0.
        $start = (float) ($GLOBALS['timestart'] ?? $_SERVER['REQUEST_TIME_FLOAT'] ?? microtime(true));
        return sprintf("\n<!-- cloudground queries=%d time=%.4F mem=%d -->\n",
            (int) get_num_queries(), max(0.0, microtime(true) - $start), (int) memory_get_peak_usage(true));
    }
}

/**
 * A failed sign-in's mark for the block list (SPEC 2.10): nginx logs the
 * address of a POST WordPress answered with it, and the agent counts it.
 */
final class AuthFailure
{
    public const HEADER = 'X-CloudGround-Auth';

    /** @var callable(string): void How a header is sent; tests replace it. */
    public static $send = null;

    private static int $count = 0;

    public static function failed(): void
    {
        self::$count++;
        if (self::$send === null && headers_sent()) {
            return;
        }
        $line = self::HEADER . ': failed=' . self::$count;
        if (self::$send !== null) {
            (self::$send)($line);
            return;
        }
        // Replaces the previous mark: one line per request, with the count.
        header($line, true);
    }

    /** Tests: a new request. */
    public static function reset(): void
    {
        self::$count = 0;
    }
}

/** WooCommerce's cart-fragments script, off outside cart and checkout (SPEC 2.8). */
final class CartFragments
{
    public static function dequeue(): void
    {
        if (!function_exists('is_cart') || !function_exists('is_checkout') || is_cart() || is_checkout()) {
            return;
        }
        wp_dequeue_script('wc-cart-fragments');
    }
}

/** RFC 4648 base32, without padding: how authenticator apps take a key. */
final class Base32
{
    private const ALPHABET = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';

    public static function encode(string $bytes): string
    {
        $bits = '';
        foreach (str_split($bytes) as $c) {
            $bits .= str_pad(decbin(ord($c)), 8, '0', STR_PAD_LEFT);
        }
        $out = '';
        foreach (str_split($bits, 5) as $chunk) {
            $out .= self::ALPHABET[bindec(str_pad($chunk, 5, '0'))];
        }
        return $bytes === '' ? '' : $out;
    }

    public static function decode(string $text): ?string
    {
        $text = strtoupper(rtrim($text, '='));
        $bits = '';
        foreach (str_split($text) as $c) {
            $i = strpos(self::ALPHABET, $c);
            if ($i === false) {
                return null;
            }
            $bits .= str_pad(decbin($i), 5, '0', STR_PAD_LEFT);
        }
        $out = '';
        foreach (str_split($bits, 8) as $byte) {
            if (strlen($byte) === 8) {
                $out .= chr(bindec($byte));
            }
        }
        return $text === '' ? '' : $out;
    }
}

/** Time-based one-time passwords, RFC 6238: SHA-1, six digits, 30 s. */
final class Totp
{
    public const PERIOD = 30;

    /** A new key, 160 random bits in base32. */
    public static function secret(): string
    {
        return Base32::encode(random_bytes(20));
    }

    /** The code of a key at a time step. */
    public static function at(string $secret, int $step): string
    {
        $key = Base32::decode($secret) ?? '';
        $mac = hash_hmac('sha1', pack('J', $step), $key, true);
        $o = ord($mac[19]) & 0x0f;
        $n = ((ord($mac[$o]) & 0x7f) << 24 | ord($mac[$o + 1]) << 16 | ord($mac[$o + 2]) << 8 | ord($mac[$o + 3])) % 1000000;
        return str_pad((string) $n, 6, '0', STR_PAD_LEFT);
    }

    /**
     * The time step code matches, one step of clock drift either way, and
     * only past $after: a code already used never works again. 0 for none.
     */
    public static function verify(string $secret, string $code, int $after, ?int $now = null): int
    {
        if (preg_match('/\A\d{6}\z/', $code) !== 1) {
            return 0;
        }
        $step = intdiv($now ?? time(), self::PERIOD);
        $found = 0;
        // Every candidate is compared, so the time taken does not say which matched.
        foreach ([$step - 1, $step, $step + 1] as $s) {
            if (hash_equals(self::at($secret, $s), $code) && $s > $after) {
                $found = $s;
            }
        }
        return $found;
    }
}

/**
 * Secrets kept in the database, sealed with a key derived from the site's
 * salts in wp-config.php: a copy of the database alone does not give them
 * away. New salts make them unreadable, which fails closed (SPEC 2.11).
 */
final class Seal
{
    private static function key(): string
    {
        return hash_hmac('sha256', 'cloudground-2fa', wp_salt('secure_auth'), true);
    }

    public static function close(string $plain): string
    {
        $nonce = random_bytes(SODIUM_CRYPTO_SECRETBOX_NONCEBYTES);
        return 'v1:' . base64_encode($nonce . sodium_crypto_secretbox($plain, $nonce, self::key()));
    }

    public static function open($sealed): ?string
    {
        if (!is_string($sealed) || !str_starts_with($sealed, 'v1:')) {
            return null;
        }
        $raw = base64_decode(substr($sealed, 3), true);
        if ($raw === false || strlen($raw) <= SODIUM_CRYPTO_SECRETBOX_NONCEBYTES) {
            return null;
        }
        try {
            $plain = sodium_crypto_secretbox_open(substr($raw, SODIUM_CRYPTO_SECRETBOX_NONCEBYTES),
                substr($raw, 0, SODIUM_CRYPTO_SECRETBOX_NONCEBYTES), self::key());
        } catch (\Throwable) {
            return null;
        }
        return is_string($plain) ? $plain : null;
    }
}

/**
 * Two-factor sign-in (SPEC 2.11). The panel turns it on per site with
 * CLOUDGROUND_2FA in the environment of the site's PHP master: "optional"
 * lets each account set it up from its profile, "required" also makes it
 * compulsory for administrators and shop managers. A correct password
 * then signs nobody in: it opens a second step on wp-login.php that asks
 * for the code, and only that step sets the auth cookie.
 */
final class TwoFactor
{
    public const SECRET = 'cloudground_2fa_secret';
    public const RECOVERY = 'cloudground_2fa_recovery';
    public const LAST = 'cloudground_2fa_last';
    public const FAILS = 'cloudground_2fa_fails';
    public const PENDING = 'cloudground_2fa_pending';
    public const SETUP = 'cloudground_2fa_setup';
    public const SHOW = 'cloudground_2fa_show';
    public const COOKIE = 'cloudground_2fa';
    public const ACTION = 'cloudground_2fa';
    public const ROLES = ['administrator', 'shop_manager'];
    /** How long the second step waits, and how many codes it takes. */
    public const TTL = 600;
    public const TRIES = 5;
    public const CODES = 10;

    /** A profile save's refusal, reported through user_profile_update_errors. */
    private static string $error = '';

    public static function mode(): string
    {
        $v = getenv('CLOUDGROUND_2FA');
        return $v === 'optional' || $v === 'required' ? $v : '';
    }

    public static function register(): void
    {
        if (self::mode() === '') {
            return;
        }
        // Last, so it sees what every other authentication plugin decided.
        add_filter('authenticate', [self::class, 'authenticate'], 99999, 3);
        add_action('login_form_' . self::ACTION, [self::class, 'screen']);
        add_action('show_user_profile', [self::class, 'profile']);
        add_action('edit_user_profile', [self::class, 'profile']);
        add_action('personal_options_update', [self::class, 'save']);
        add_action('edit_user_profile_update', [self::class, 'save']);
        add_action('user_profile_update_errors', [self::class, 'errors'], 10, 3);
        add_action('admin_notices', [self::class, 'notice']);
    }

    public static function required(\WP_User $user): bool
    {
        $required = self::mode() === 'required' && array_intersect((array) $user->roles, self::ROLES) !== [];
        return (bool) apply_filters('cloudground_2fa_required', $required, $user);
    }

    public static function enrolled(int $user): bool
    {
        $secret = get_user_meta($user, self::SECRET, true);
        return is_string($secret) && $secret !== '';
    }

    // ------------------------------------------------------ sign-in --

    /** The authenticate filter: a password alone never signs in an account that has, or must have, a second factor. */
    public static function authenticate($user, $username = '', $password = '')
    {
        $out = self::intercept($user, (string) $username, (string) $password, $_POST, self::interactive());
        if ($out instanceof Reply) {
            nocache_headers();
            $out->emit();
        }
        return $out;
    }

    /** @return \WP_User|\WP_Error|Reply|mixed */
    public static function intercept($user, string $username, string $password, array $request, bool $interactive)
    {
        if (!$user instanceof \WP_User || self::mode() === '') {
            return $user;
        }
        // An auth cookie (no credentials typed) was earned with the second factor.
        if ($username === '' && $password === '') {
            return $user;
        }
        // Application passwords are their own credential, made in the profile.
        if (did_action('application_password_did_authenticate')) {
            return $user;
        }
        $enrolled = self::enrolled($user->ID);
        if (!$enrolled && !self::required($user)) {
            return $user;
        }
        if (!$interactive) {
            return new \WP_Error('cloudground_2fa', __('This account signs in with two-factor authentication: use the sign-in page, or an application password for other programs.', 'cloudground'));
        }
        return self::begin($user, $enrolled, wp_unslash($request));
    }

    /** A sign-in form posted by a browser, not XML-RPC, the REST API, Ajax or wp-cli. */
    private static function interactive(): bool
    {
        if ((defined('XMLRPC_REQUEST') && \XMLRPC_REQUEST) || (defined('REST_REQUEST') && \REST_REQUEST)
            || (defined('WP_CLI') && \WP_CLI) || wp_doing_ajax()) {
            return false;
        }
        return strtoupper((string) ($_SERVER['REQUEST_METHOD'] ?? '')) === 'POST';
    }

    /**
     * Holds the sign-in: a pending record in the user's meta (only the
     * token's hash), the token in a short HttpOnly cookie, and a redirect
     * to the second step. An account that must set two-factor up first
     * also gets a code by email: the password alone cannot enrol a new app.
     */
    public static function begin(\WP_User $user, bool $enrolled, array $request): Reply
    {
        $token = bin2hex(random_bytes(32));
        $pending = [
            'hash' => hash('sha256', $token),
            'expires' => time() + self::TTL,
            'tries' => 0,
            'remember' => !empty($request['rememberme']),
            'redirect' => self::requested($request),
            'interim' => !empty($request['interim-login']),
        ];
        if (!$enrolled) {
            $code = sprintf('%08d', random_int(0, 99999999));
            // Keyed with the token, so the stored value cannot be reversed by trying every code.
            $pending['mail'] = hash_hmac('sha256', $code, $token);
            $pending['secret'] = Seal::close(Totp::secret());
            $pending['sent'] = self::mailCode($user, $code);
        }
        update_user_meta($user->ID, self::PENDING, $pending);
        $reply = new Reply(303, '', ['Location' => self::url($pending['interim'])]);
        $reply->cookies[self::COOKIE] = [$user->ID . '.' . $token, $pending['expires']];
        return $reply;
    }

    private static function mailCode(\WP_User $user, string $code): bool
    {
        $site = wp_specialchars_decode((string) get_bloginfo('name'), ENT_QUOTES);
        $body = sprintf(__("Someone signed in to %1\$s as %2\$s with the right password. Two-factor sign-in is required for this account and is not set up yet.\n\nTo set it up, enter this confirmation code on the sign-in page within 10 minutes:\n\n%3\$s\n\nIf this was not you, change your password now: someone else knows it.", 'cloudground'),
            home_url('/'), $user->user_login, substr($code, 0, 4) . ' ' . substr($code, 4));
        return (bool) wp_mail($user->user_email, sprintf(__('[%s] Your confirmation code', 'cloudground'), $site), $body);
    }

    /** Where the sign-in asked to go, if it is this site. */
    private static function requested(array $request): string
    {
        $to = $request['redirect_to'] ?? $request['redirect'] ?? '';
        return is_string($to) ? (string) wp_validate_redirect($to, '') : '';
    }

    private static function url(bool $interim): string
    {
        return site_url('wp-login.php?action=' . self::ACTION . ($interim ? '&interim-login=1' : ''), 'login_post');
    }

    /** login_form_cloudground_2fa: the second step. */
    public static function screen(): void
    {
        nocache_headers();
        $reply = self::step(strtoupper((string) ($_SERVER['REQUEST_METHOD'] ?? 'GET')),
            is_string($_COOKIE[self::COOKIE] ?? null) ? $_COOKIE[self::COOKIE] : '', wp_unslash($_POST));
        $reply->headers += ['Referrer-Policy' => 'no-referrer', 'X-Robots-Tag' => 'noindex'];
        $reply->emit();
    }

    /** @return array{0: \WP_User, 1: array, 2: string}|null the sign-in the cookie holds */
    public static function pending(string $cookie): ?array
    {
        if (preg_match('/\A([1-9]\d{0,19})\.([0-9a-f]{64})\z/', $cookie, $m) !== 1) {
            return null;
        }
        $user = get_userdata((int) $m[1]);
        $rec = $user instanceof \WP_User ? get_user_meta($user->ID, self::PENDING, true) : null;
        if (!is_array($rec) || !hash_equals((string) ($rec['hash'] ?? ''), hash('sha256', $m[2])) || time() > (int) ($rec['expires'] ?? 0)) {
            return null;
        }
        return [$user, $rec, $m[2]];
    }

    public static function step(string $method, string $cookie, array $post): Reply
    {
        $p = self::pending($cookie);
        if ($p === null) {
            return self::over(__('This sign-in has expired. Please sign in again.', 'cloudground'));
        }
        [$user, $rec, $token] = $p;
        // Bound to this sign-in: another page cannot post the form.
        $form = hash_hmac('sha256', 'form', $token);
        if ($method !== 'POST') {
            return self::page($user, $rec, $form, '');
        }
        if (!hash_equals($form, is_string($post['cg2fa_form'] ?? null) ? $post['cg2fa_form'] : '')) {
            return self::page($user, $rec, $form, '');
        }
        $wait = self::throttled($user->ID);
        if ($wait > 0) {
            return self::page($user, $rec, $form, sprintf(__('Too many wrong codes. Try again in %d minutes.', 'cloudground'), (int) ceil($wait / 60)));
        }
        $rec['tries'] = (int) ($rec['tries'] ?? 0) + 1;
        if ($rec['tries'] > self::TRIES) {
            delete_user_meta($user->ID, self::PENDING);
            return self::over(__('Too many wrong codes. Please sign in again.', 'cloudground'));
        }
        update_user_meta($user->ID, self::PENDING, $rec);
        $code = is_string($post['cg2fa_code'] ?? null) ? $post['cg2fa_code'] : '';
        $codes = null;
        if (isset($rec['secret'])) {
            $codes = self::confirmSetup($user->ID, $rec, $token, is_string($post['cg2fa_mail'] ?? null) ? $post['cg2fa_mail'] : '', $code);
            $ok = $codes !== null;
        } else {
            $ok = self::check($user->ID, $code);
        }
        if (!$ok) {
            self::failed($user->ID);
            // Counted for the block list like a wrong password (SPEC 2.10).
            AuthFailure::failed();
            return self::page($user, $rec, $form, isset($rec['secret'])
                ? __('A code is not right. Check the confirmation code from the email and the code your app shows now.', 'cloudground')
                : __('That code is not right. Enter the code your app shows now, or a recovery code.', 'cloudground'));
        }
        // Used once: a second post of the same form finds nothing.
        if (!delete_user_meta($user->ID, self::PENDING)) {
            return self::over(__('This sign-in has expired. Please sign in again.', 'cloudground'));
        }
        delete_user_meta($user->ID, self::FAILS);
        return self::finish($user, $rec, $codes);
    }

    /**
     * Whether code is the account's current app code or one of its
     * recovery codes (which then stops working). A key that no longer
     * opens (new salts) is a wrong code, never a skipped check.
     */
    public static function check(int $user, string $code): bool
    {
        $code = strtolower(preg_replace('/[\s-]+/', '', $code) ?? '');
        if (preg_match('/\A\d{6}\z/', $code) === 1) {
            $secret = Seal::open(get_user_meta($user, self::SECRET, true));
            if ($secret === null) {
                return false;
            }
            $step = Totp::verify($secret, $code, (int) get_user_meta($user, self::LAST, true));
            if ($step === 0) {
                return false;
            }
            update_user_meta($user, self::LAST, $step);
            return true;
        }
        if (preg_match('/\A[0-9a-f]{16}\z/', $code) !== 1) {
            return false;
        }
        $hash = hash('sha256', $code);
        $left = [];
        $used = false;
        $list = get_user_meta($user, self::RECOVERY, true);
        foreach (is_array($list) ? $list : [] as $h) {
            if (!$used && is_string($h) && hash_equals($h, $hash)) {
                $used = true;
                continue;
            }
            $left[] = $h;
        }
        if ($used) {
            update_user_meta($user, self::RECOVERY, $left);
        }
        return $used;
    }

    /** @return list<string>|null the new recovery codes once the email code and the app's code both match */
    private static function confirmSetup(int $user, array $rec, string $token, string $mail, string $code): ?array
    {
        $mail = preg_replace('/\D+/', '', $mail) ?? '';
        $secret = Seal::open($rec['secret']);
        $mailOK = hash_equals((string) ($rec['mail'] ?? ''), hash_hmac('sha256', $mail, $token));
        $step = $secret === null ? 0 : Totp::verify($secret, preg_replace('/\s+/', '', $code) ?? '', 0);
        if (!$mailOK || $step === 0) {
            return null;
        }
        return self::enrol($user, (string) $rec['secret'], $step);
    }

    /** @return list<string> */
    private static function enrol(int $user, string $sealed, int $step): array
    {
        update_user_meta($user, self::SECRET, $sealed);
        update_user_meta($user, self::LAST, $step);
        delete_user_meta($user, self::SETUP);
        return self::newCodes($user);
    }

    /** @return list<string> ten new recovery codes; the old ones stop working */
    public static function newCodes(int $user): array
    {
        $codes = [];
        $hashes = [];
        for ($i = 0; $i < self::CODES; $i++) {
            $hex = bin2hex(random_bytes(8));
            $codes[] = implode('-', str_split($hex, 4));
            $hashes[] = hash('sha256', $hex);
        }
        update_user_meta($user, self::RECOVERY, $hashes);
        return $codes;
    }

    /** Seconds before this account may try a code again: after five wrong codes, 1 minute, doubling up to an hour. */
    public static function throttled(int $user, ?int $now = null): int
    {
        [$n, $at] = array_map('intval', explode(':', (string) get_user_meta($user, self::FAILS, true)) + [0, 0]);
        if ($n < self::TRIES) {
            return 0;
        }
        $delay = min(3600, 60 << min(6, $n - self::TRIES));
        return max(0, $at + $delay - ($now ?? time()));
    }

    private static function failed(int $user): void
    {
        [$n] = array_map('intval', explode(':', (string) get_user_meta($user, self::FAILS, true)) + [0]);
        update_user_meta($user, self::FAILS, ($n + 1) . ':' . time());
    }

    /** Signs in, as wp_signon would have: the cookie, wp_login, the redirect. */
    private static function finish(\WP_User $user, array $rec, ?array $codes): Reply
    {
        $requested = (string) ($rec['redirect'] ?? '');
        $to = (string) apply_filters('login_redirect', $requested !== '' ? $requested : admin_url(), $requested, $user);
        $to = (string) wp_validate_redirect($to, admin_url());
        if ($to === admin_url() && !$user->has_cap('edit_posts')) {
            $to = $user->has_cap('read') ? admin_url('profile.php') : home_url('/');
        }
        if ($codes !== null) {
            $reply = self::codesPage($codes, $to);
        } elseif (!empty($rec['interim'])) {
            $reply = self::interimDone();
        } else {
            $reply = new Reply(303, '', ['Location' => $to]);
        }
        $reply->user = $user->ID;
        $reply->remember = !empty($rec['remember']);
        $reply->cookies[self::COOKIE] = ['', 1];
        $reply->then = static function () use ($user): void {
            do_action('wp_login', $user->user_login, $user);
        };
        return $reply;
    }

    // ------------------------------------------------------- pages --

    private static function page(\WP_User $user, array $rec, string $form, string $error): Reply
    {
        $setup = isset($rec['secret']);
        // wp-login.php reads it only after this action has run.
        $GLOBALS['interim_login'] = !empty($rec['interim']);
        ob_start();
        self::header(__('Two-factor sign-in', 'cloudground'), $error);
        echo '<form name="cg2fa" id="cg2fa" method="post" action="' . esc_url(self::url(!empty($rec['interim']))) . '">';
        if ($setup) {
            $secret = (string) Seal::open($rec['secret']);
            echo '<p>' . esc_html__('Two-factor sign-in is required for this account. Set it up now:', 'cloudground') . '</p>';
            echo '<p><label for="cg2fa-mail">' . esc_html(!empty($rec['sent'])
                ? sprintf(__('1. The confirmation code emailed to %s', 'cloudground'), self::mask($user->user_email))
                : __('1. The confirmation code: the email could not be sent. Ask an administrator, or sign in from the CloudGround panel.', 'cloudground')) . '</label>';
            echo '<input type="text" name="cg2fa_mail" id="cg2fa-mail" class="input" autocomplete="off" inputmode="numeric" spellcheck="false" required></p>';
            echo '<p>' . esc_html__('2. Add this key to an authenticator app:', 'cloudground') . '<br><code style="font-size:1.1em;word-spacing:.2em">'
                . esc_html(trim(chunk_split($secret, 4, ' '))) . '</code><br><a href="' . esc_attr(self::otpauth($user, $secret)) . '">'
                . esc_html__('Open in an authenticator app on this device', 'cloudground') . '</a></p>';
            echo '<p><label for="cg2fa-code">' . esc_html__('3. The six-digit code the app shows', 'cloudground') . '</label>';
        } else {
            echo '<p><label for="cg2fa-code">' . esc_html__('Authentication code', 'cloudground') . '</label>';
        }
        echo '<input type="text" name="cg2fa_code" id="cg2fa-code" class="input" autocomplete="one-time-code" spellcheck="false" required autofocus></p>';
        if (!$setup) {
            echo '<p class="description">' . esc_html__('The six-digit code from your authenticator app, or one of your recovery codes.', 'cloudground') . '</p>';
        }
        echo '<input type="hidden" name="cg2fa_form" value="' . esc_attr($form) . '">';
        echo '<p class="submit"><input type="submit" class="button button-primary button-large" value="' . esc_attr__('Continue', 'cloudground') . '"></p></form>';
        self::footer($setup ? 'cg2fa-mail' : 'cg2fa-code');
        return new Reply(200, (string) ob_get_clean(), ['Content-Type' => 'text/html; charset=utf-8']);
    }

    /** @param list<string> $codes */
    private static function codesPage(array $codes, string $to): Reply
    {
        ob_start();
        self::header(__('Two-factor sign-in is on', 'cloudground'), '');
        echo '<p>' . esc_html__('Keep these recovery codes somewhere safe. Each one signs you in once if you lose your phone; this is the only time they are shown.', 'cloudground') . '</p>';
        echo '<pre style="font-size:1.1em;line-height:1.6">' . esc_html(implode("\n", $codes)) . '</pre>';
        echo '<p class="submit"><a class="button button-primary button-large" href="' . esc_url($to) . '">' . esc_html__('I have saved them, continue', 'cloudground') . '</a></p>';
        self::footer('');
        return new Reply(200, (string) ob_get_clean(), ['Content-Type' => 'text/html; charset=utf-8']);
    }

    /** What wp-login.php shows inside the session-expired dialog after a sign-in: the dialog closes on this body class. */
    private static function interimDone(): Reply
    {
        ob_start();
        if (function_exists('login_header')) {
            $GLOBALS['interim_login'] = 'success';
            login_header('', '<p class="message">' . __('You have logged in successfully.') . '</p>');
            echo '</div>';
            do_action('login_footer');
            echo '</body></html>';
        } else {
            echo '<!doctype html><body class="interim-login-success"><p>You have logged in successfully.</p></body>';
        }
        return new Reply(200, (string) ob_get_clean(), ['Content-Type' => 'text/html; charset=utf-8']);
    }

    private static function over(string $message): Reply
    {
        ob_start();
        self::header(__('Two-factor sign-in', 'cloudground'), $message);
        echo '<p><a href="' . esc_url(wp_login_url()) . '">' . esc_html__('Sign in', 'cloudground') . '</a></p>';
        self::footer('');
        $reply = new Reply(200, (string) ob_get_clean(), ['Content-Type' => 'text/html; charset=utf-8']);
        $reply->cookies[self::COOKIE] = ['', 1];
        return $reply;
    }

    /** wp-login.php's own frame when it is there (it is, on the second step). */
    private static function header(string $title, string $error): void
    {
        if (function_exists('login_header')) {
            login_header($title, '', $error !== '' ? new \WP_Error('cloudground_2fa', esc_html($error)) : null);
            return;
        }
        echo '<!doctype html><html><head><meta charset="utf-8"><meta name="robots" content="noindex"><title>' . esc_html($title) . '</title></head><body>';
        if ($error !== '') {
            echo '<div id="login_error">' . esc_html($error) . '</div>';
        }
    }

    private static function footer(string $focus): void
    {
        if (function_exists('login_footer')) {
            login_footer($focus);
            return;
        }
        echo '</body></html>';
    }

    private static function mask(string $email): string
    {
        [$local, $domain] = array_pad(explode('@', $email, 2), 2, '');
        return substr($local, 0, 1) . str_repeat('*', max(1, strlen($local) - 1)) . '@' . $domain;
    }

    /** The otpauth:// link authenticator apps open (Key Uri Format). */
    public static function otpauth(\WP_User $user, string $secret): string
    {
        $issuer = (string) wp_parse_url(home_url('/'), PHP_URL_HOST);
        return 'otpauth://totp/' . rawurlencode($issuer . ':' . $user->user_login)
            . '?secret=' . $secret . '&issuer=' . rawurlencode($issuer) . '&algorithm=SHA1&digits=6&period=' . Totp::PERIOD;
    }

    // ----------------------------------------------------- profile --

    private static function nonceAction(int $user): string
    {
        return 'cloudground_2fa_profile_' . $user;
    }

    /** The section of a profile: set up, new recovery codes, turn off; an administrator can reset another account. */
    public static function profile($user): void
    {
        if (!$user instanceof \WP_User) {
            return;
        }
        $self = get_current_user_id() === $user->ID;
        if (!$self && !current_user_can('manage_options')) {
            return;
        }
        $enrolled = self::enrolled($user->ID);
        $required = self::required($user);
        echo '<h2 id="cloudground-2fa">' . esc_html__('Two-factor sign-in', 'cloudground') . '</h2><table class="form-table" role="presentation"><tr><th scope="row">'
            . esc_html__('Authenticator app', 'cloudground') . '</th><td>';
        wp_nonce_field(self::nonceAction($user->ID), '_cg2fa_nonce');
        if ($enrolled) {
            $list = get_user_meta($user->ID, self::RECOVERY, true);
            $left = is_array($list) ? count($list) : 0;
            echo '<p>' . esc_html(sprintf(_n('On. %d recovery code left.', 'On. %d recovery codes left.', $left, 'cloudground'), $left)) . '</p>';
            $show = $self ? Seal::open(get_user_meta($user->ID, self::SHOW, true)) : null;
            if ($show !== null) {
                delete_user_meta($user->ID, self::SHOW);
                echo '<p>' . esc_html__('Your new recovery codes. Keep them somewhere safe; this is the only time they are shown.', 'cloudground') . '</p><pre>' . esc_html($show) . '</pre>';
            }
            if ($self) {
                echo '<p><label><input type="checkbox" name="cg2fa_new_codes" value="1"> ' . esc_html__('Make new recovery codes (the old ones stop working)', 'cloudground') . '</label></p>';
            }
            $label = !$self ? __('Reset two-factor sign-in for this account', 'cloudground')
                : ($required ? __('Remove this app (you will set up a new one at your next sign-in)', 'cloudground') : __('Turn two-factor sign-in off', 'cloudground'));
            echo '<p><label><input type="checkbox" name="cg2fa_reset" value="1"> ' . esc_html($label) . '</label></p>';
        } elseif ($self) {
            $secret = self::setupSecret($user->ID);
            echo '<p>' . esc_html($required ? __('Required for this account. Add this key to an authenticator app, then enter the code it shows and update your profile.', 'cloudground')
                : __('Off. To turn it on, add this key to an authenticator app, then enter the code it shows and update your profile.', 'cloudground')) . '</p>';
            echo '<p><code style="font-size:1.1em;word-spacing:.2em">' . esc_html(trim(chunk_split($secret, 4, ' '))) . '</code> <a href="' . esc_attr(self::otpauth($user, $secret)) . '">'
                . esc_html__('Open in an authenticator app on this device', 'cloudground') . '</a></p>';
            echo '<p><label for="cg2fa-setup">' . esc_html__('Code from the app', 'cloudground') . '</label> <input type="text" name="cg2fa_setup_code" id="cg2fa-setup" class="regular-text" autocomplete="one-time-code" inputmode="numeric" spellcheck="false"></p>';
        } else {
            echo '<p>' . esc_html($required ? __('Not set up yet. It is required: this account will be asked to set it up at its next sign-in.', 'cloudground') : __('Off.', 'cloudground')) . '</p>';
        }
        echo '</td></tr></table>';
    }

    /** The key being set up from the profile: kept (sealed) for an hour so a reload does not change it. */
    private static function setupSecret(int $user): string
    {
        $setup = get_user_meta($user, self::SETUP, true);
        if (is_array($setup) && time() - (int) ($setup['at'] ?? 0) < 3600) {
            $secret = Seal::open($setup['secret'] ?? null);
            if ($secret !== null) {
                return $secret;
            }
        }
        $secret = Totp::secret();
        update_user_meta($user, self::SETUP, ['secret' => Seal::close($secret), 'at' => time()]);
        return $secret;
    }

    /** personal_options_update / edit_user_profile_update. */
    public static function save($user_id): void
    {
        $user_id = (int) $user_id;
        $nonce = is_string($_POST['_cg2fa_nonce'] ?? null) ? wp_unslash($_POST['_cg2fa_nonce']) : '';
        if ($nonce === '' || !wp_verify_nonce($nonce, self::nonceAction($user_id)) || !current_user_can('edit_user', $user_id)) {
            return;
        }
        $self = get_current_user_id() === $user_id;
        if (!$self && !current_user_can('manage_options')) {
            return;
        }
        if (!empty($_POST['cg2fa_reset'])) {
            foreach ([self::SECRET, self::RECOVERY, self::LAST, self::SHOW, self::PENDING, self::SETUP, self::FAILS] as $key) {
                delete_user_meta($user_id, $key);
            }
            return;
        }
        if (!$self) {
            return;
        }
        if (self::enrolled($user_id)) {
            if (!empty($_POST['cg2fa_new_codes'])) {
                update_user_meta($user_id, self::SHOW, Seal::close(implode("\n", self::newCodes($user_id))));
            }
            return;
        }
        $code = is_string($_POST['cg2fa_setup_code'] ?? null) ? preg_replace('/\s+/', '', wp_unslash($_POST['cg2fa_setup_code'])) : '';
        if ($code === '') {
            return;
        }
        $setup = get_user_meta($user_id, self::SETUP, true);
        $secret = is_array($setup) ? Seal::open($setup['secret'] ?? null) : null;
        $step = $secret === null ? 0 : Totp::verify($secret, (string) $code, 0);
        if ($step === 0) {
            self::$error = __('Two-factor sign-in was not turned on: the code from the app is not right.', 'cloudground');
            return;
        }
        $codes = self::enrol($user_id, (string) $setup['secret'], $step);
        update_user_meta($user_id, self::SHOW, Seal::close(implode("\n", $codes)));
    }

    public static function errors($errors, $update = false, $user = null): void
    {
        if (self::$error !== '' && $errors instanceof \WP_Error) {
            $errors->add('cloudground_2fa', esc_html(self::$error));
            self::$error = '';
        }
    }

    /** A required account that is signed in without the app (from the panel, or from before): a reminder on every admin page. */
    public static function notice(): void
    {
        $user = wp_get_current_user();
        if (!$user instanceof \WP_User || $user->ID === 0 || !self::required($user) || self::enrolled($user->ID)) {
            return;
        }
        echo '<div class="notice notice-warning"><p>' . esc_html__('Two-factor sign-in is required for your account.', 'cloudground')
            . ' <a href="' . esc_url(admin_url('profile.php#cloudground-2fa')) . '">' . esc_html__('Set it up now', 'cloudground') . '</a></p></div>';
    }
}

// --------------------------------------------------------------- wiring --

add_action('muplugins_loaded', [Stats::class, 'handle'], 0);
add_action('init', [MagicLogin::class, 'handle'], 1);
add_action('wp_footer', [Guard::class, 'footer'], PHP_INT_MAX);
add_action('wp_login_failed', [AuthFailure::class, 'failed'], 10, 0);
add_action('application_password_failed_authentication', [AuthFailure::class, 'failed'], 10, 0);
Events::register();
TwoFactor::register();

if (defined('CLOUDGROUND_DISABLE_CART_FRAGMENTS') && \CLOUDGROUND_DISABLE_CART_FRAGMENTS) {
    add_action('wp_enqueue_scripts', [CartFragments::class, 'dequeue'], 100);
    add_action('wp_print_footer_scripts', [CartFragments::class, 'dequeue'], 1);
}

if (function_exists('wp_get_environment_type') && wp_get_environment_type() === 'staging') {
    add_filter('pre_wp_mail', '__return_true');
}
