<?php
/**
 * Plugin Name: CloudGround Connector
 * Description: Purges exactly the cached pages a change affects, signs the panel in, and reports the object cache. Managed by CloudGround.
 * Version: 2.1.0
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
        if ($this->user > 0) {
            wp_set_auth_cookie($this->user, false, true);
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

// --------------------------------------------------------------- wiring --

add_action('muplugins_loaded', [Stats::class, 'handle'], 0);
add_action('init', [MagicLogin::class, 'handle'], 1);
add_action('wp_footer', [Guard::class, 'footer'], PHP_INT_MAX);
add_action('wp_login_failed', [AuthFailure::class, 'failed'], 10, 0);
add_action('application_password_failed_authentication', [AuthFailure::class, 'failed'], 10, 0);
Events::register();

if (defined('CLOUDGROUND_DISABLE_CART_FRAGMENTS') && \CLOUDGROUND_DISABLE_CART_FRAGMENTS) {
    add_action('wp_enqueue_scripts', [CartFragments::class, 'dequeue'], 100);
    add_action('wp_print_footer_scripts', [CartFragments::class, 'dequeue'], 1);
}

if (function_exists('wp_get_environment_type') && wp_get_environment_type() === 'staging') {
    add_filter('pre_wp_mail', '__return_true');
}
