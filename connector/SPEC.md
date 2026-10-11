# connector — the WordPress side of CloudGround

Two PHP files the agent installs into every WordPress site, and the contract
between them, the agent (`internal/agent/ops/wp`) and the panel API
(`internal/api/connector_purge.go`, internal/api/SPEC.md §8).

| File | Installed as | Embedded as |
| --- | --- | --- |
| `php/cloudground-connector.php` | `<release>/wp-content/mu-plugins/cloudground-connector.php` | `connector.PHP` |
| `php/object-cache.php` | `shared/object-cache.php`, linked from `<release>/wp-content/object-cache.php` | `connector.ObjectCacheAPCu` |

Both are **GPL-2.0-or-later** (`LICENSE`): code that runs inside WordPress,
on its hooks, is GPL-derivative in the WordPress Foundation's reading,
whatever licence the panel itself carries. They target **WordPress 6.6+** and
**PHP 8.2+** (sites run 8.4 and 8.5).

Sources of this specification: the e2e suite (`e2e/features_test.go`: cache,
magic login, object cache, template-part purge, R-3.3/R-3.4/R-3.8 checks;
`e2e/e2e_test.go`: WooCommerce), `internal/agent/ops/wp/SPEC.md`,
`internal/api/connector.go`, `internal/velocity/purge.go`, the vhost template
(`internal/engine/nginx`), `internal/guard`, `docs/caching.md` and
WooCommerce 11.2.1 and Storefront 4.6.2 as published on wordpress.org on
2026-10-09.

## 1. Configuration

Written into `wp-config.php` by the agent (`ops/wp` §11, `ops/staging`):

| Constant | Meaning |
| --- | --- |
| `CLOUDGROUND_SITE_TOKEN` | The site's secret. Authenticates purges to the panel, the agent to the stats endpoint, and keys the Guard probe. Without it the connector sends nothing and answers no endpoint. |
| `CLOUDGROUND_ENDPOINT` | The panel's loopback listener, default `http://127.0.0.1:9080`. |
| `CLOUDGROUND_DISABLE_CART_FRAGMENTS` | `true` on WooCommerce sites (§2.8). |
| `CLOUDGROUND_PURGE_PAGES` | Optional. How many pages of an archive a change purges, default 10, clamped to 0–50 (§2.3). |
| `WP_ENVIRONMENT_TYPE` | `staging` on a staging copy (§2.9). |

The connector stores nothing of its own in the database except the
one-shot login rows the agent writes (§2.5).

## 2. The mu-plugin

### 2.1 Purge transport

Hooks (§2.2) never talk to the panel. They add URLs, or "everything", to a
per-request **queue**. The first addition arms one `shutdown` action at the
last priority, which:

1. drops everything when nothing was queued or there is no token;
2. builds the payload: `{"all":true}` when everything was asked for or more
   than **2000** URLs were queued; otherwise `{"urls":[…]}`, unique absolute
   URLs, after the `cloudground_purge_urls` filter (a plugin may add its own
   URLs, e.g. an SEO plugin's sitemaps). `cloudground_purge_all` (bool) can
   turn any purge into a full one;
3. ends the response first when PHP-FPM can (`fastcgi_finish_request`), so
   the person saving a post never waits for the purge;
4. sends **one** `POST <endpoint>/api/connector/purge` with
   `Authorization: Bearer <token>`, `Content-Type: application/json`, the
   payload as the body, **blocking**, 3 s timeout, no redirects.

The API contract is unchanged: the body has exactly the fields `urls`
(array of strings) and `all` (bool), because the handler refuses unknown
fields. The API maps the token to its site in constant time, drops URLs
whose host is not the site's domain or an alias, and asks the agent to
remove the page-cache entries for each URL (GET and HEAD, http and https, on
the domain and every alias), or the whole site's cache for `all`.

Why blocking: a non-blocking request is abandoned when the process exits,
which is what every wp-cli command does at once: the previous connector's
purges from wp-cli (a post created, a plugin activated) never reached the
panel. After `fastcgi_finish_request` the wait costs no visitor anything; in
wp-cli it costs at most the timeout.

### 2.2 Events

"Post set", "product page", "term set" and "comment set" are defined in §2.3.
A post type is **public** when `is_post_type_viewable()`; a taxonomy when
`is_taxonomy_viewable()`. Revisions and autosaves are ignored everywhere.

| Event (hook) | Purge |
| --- | --- |
| `transition_post_status`, new or old status `publish`, public type | post set of the post |
| `post_updated`, the post was `publish` before | post set of the **old** post (old permalink, old date and author archives); a hierarchical post whose path changed and that has children → everything |
| `before_delete_post`, the post is `publish` | post set (deleting without the trash fires no transition) |
| `set_object_terms`, published public post, public taxonomy, terms changed | the post's permalink and the term set of every term added **or removed** |
| post types `wp_template`, `wp_template_part`, `wp_global_styles`, `wp_navigation`, `wp_block` changing (any of the above) | everything: they render on any URL — except a `wp_navigation` going from `new` during a visitor's page view (GET or HEAD of the front end; not the admin, REST, Ajax, cron or wp-cli): WordPress's fallback menu, which a Navigation block with no menu saves while it renders (`WP_Navigation_Fallback`, the first view of a fresh site), holding what the page already shows |
| `comment_post` approved; `transition_comment_status` to or from `approved`; `edit_comment` of an approved comment | comment set |
| `edited_term`, `delete_term` on a public taxonomy | everything (a term's name shows on every post that has it) |
| `switch_theme`, `activated_plugin`, `deactivated_plugin`, `upgrader_process_complete`, `wp_update_nav_menu`, `wp_delete_nav_menu`, `customize_save_after` | everything |
| `added_option` / `updated_option` / `deleted_option` of a site-wide option (below) | everything |
| WooCommerce `woocommerce_product_object_updated_props` | post set of the product (of the parent for a variation) when a prop shown in listings changed (`regular_price`, `sale_price`, `date_on_sale_from`, `date_on_sale_to`, `stock_status`, `product_type`, `image_id`, `average_rating`, `rating_counts`, `review_count`); otherwise the product page, except when only `total_sales` changed (nothing) |
| WooCommerce `woocommerce_product_set_stock`, `woocommerce_variation_set_stock` | product page |
| WooCommerce `woocommerce_product_set_stock_status`, `woocommerce_variation_set_stock_status` | post set of the product |
| `added_post_meta` / `updated_post_meta` / `deleted_post_meta` of `_price`, `_regular_price`, `_sale_price`, `_stock_status` on a product or variation | post set of the product; `_stock` → product page |
| WooCommerce `woocommerce_attribute_added`, `…_updated`, `…_deleted` | everything |

Site-wide options: `blogname`, `blogdescription`, `permalink_structure`,
`category_base`, `tag_base`, `show_on_front`, `page_on_front`,
`page_for_posts`, `posts_per_page`, `date_format`, `time_format`,
`start_of_week`, `WPLANG`, `site_icon`, `blog_public`, `sidebars_widgets`,
any `widget_*`, any `theme_mods_*`, and WooCommerce's `woocommerce_currency`,
`woocommerce_currency_pos`, `woocommerce_price_thousand_sep`,
`woocommerce_price_decimal_sep`, `woocommerce_price_num_decimals`,
`woocommerce_shop_page_id`, `woocommerce_catalog_columns`,
`woocommerce_catalog_rows`, `woocommerce_default_catalog_orderby`,
`woocommerce_shop_page_display`, `woocommerce_category_archive_display`,
`woocommerce_hide_out_of_stock_items`, `woocommerce_tax_display_shop`,
`woocommerce_prices_include_tax`.

One write of `theme_mods_*` is not a change: the one that only **adds** the
key `custom_css_post_id` (every other mod equal before and after). WordPress
looks the Custom CSS post up while it renders a page
(`wp_get_custom_css_post`, from `wp_head`) and stores the answer, `-1` for
none, there; the first view of a fresh site does it, and purging everything
then emptied the cache that very view had filled (measured on the e2e box:
MISS, HIT, HIT, then MISS; the same view also saves the fallback menu, the
exception in the table above). A Custom CSS edit is `customize_save_after`, or
changes an id already stored, and purges everything as before.

Why WooCommerce needs its own hooks: a product saved through WooCommerce's
data layer (REST API, bulk edit, stock change at checkout, import) calls
`wp_update_post` only when a post field changed. A price-only change updates
post meta and the `post_modified` column directly, and fires no
`transition_post_status`: the previous connector never purged it.

### 2.3 Target sets

Absolute URLs from WordPress's own link functions (`home_url`,
`get_permalink`, `get_term_link`, …), so they carry the site's host.
"Pages of X" means X itself and, when the site has pretty permalinks,
`X` + `<pagination_base>/<n>/` for n = 2 … min(cap, ceil(count / per_page)),
cap = `CLOUDGROUND_PURGE_PAGES` (default 10). A new post shifts every listing
page it appears on, so page 1 alone is not enough; beyond the cap, pages
expire with the TTL. `per_page` is `posts_per_page`, or WooCommerce's
`loop_shop_per_page` for products and product taxonomies. Without pretty
permalinks, pagination and sitemaps are query strings, which the page cache
never stores, so they are skipped.

**Post set** of a public post:
- its permalink, and its comments feed;
- the home page; the front page (`page_on_front`) and the posts page
  (`page_for_posts`) when set; pages of the posts page or, when the front
  page lists posts, of the home page, for type `post`;
- the type's archive (`get_post_type_archive_link`), with its pages and feed;
  for products also WooCommerce's shop page, with its pages;
- the main feed;
- for every public taxonomy of the type (built-in, custom, WooCommerce's
  `product_cat`, `product_tag` and attribute taxonomies with archives), the
  **term set** of each of the post's terms;
- for type `post`: the author archive (pages by the author's post count) and
  feed, and the year, month and day archives (pages by the site's post
  count);
- core sitemaps: `/wp-sitemap.xml`, `/wp-sitemap-posts-<type>-<n>.xml` for
  every page of the type's sitemap (2000 URLs a page), the taxonomy sitemaps
  `/wp-sitemap-taxonomies-<tax>-1.xml` of its public taxonomies, and
  `/wp-sitemap-users-1.xml`.

**Term set** of a term: its archive with pages (by the term's count) and its
feed, and the same for every ancestor (a parent category lists its
children's posts).

**Product page**: the product's permalink (the parent's for a variation).

**Comment set**: the post's permalink and comments feed, and the site's
comments feed, when the post is published and public.

Not purged, deliberately: attachment pages, comment pages
(`/comment-page-n/`), SEO plugins' sitemaps (add them with
`cloudground_purge_urls`), pages past the cap.

### 2.4 Cache-hit consequences (for benchmarking)

Compared with the previous connector, a content change purges more URLs, and
a few changes that purged nothing now purge:

- a post save also purges: the posts page and up to 9 more pages of each
  listing it is on (home, its post-type archive, each term archive and its
  ancestors, author, year/month/day), the term and author feeds, the post's
  comments feed and the core sitemaps. Typical blog post with one category and
  a few tags, 10+ pages of posts: about 60–90 URLs instead of about 10;
- a product's price or stock status change (any path, checkout included)
  purges the product's post set; a stock quantity change purges the product
  page only; an order that changes neither (no stock management) purges
  nothing;
- term edits, widget, permalink, reading and WooCommerce catalogue settings,
  synced patterns (`wp_block`) and plugin/theme/core updates purge the whole
  site;
- purges from wp-cli now arrive (they were dropped).

Read traffic is unchanged; the extra cost is cache misses on listing pages
after a change. The number to compare is the hit ratio under the `uncached`
and `browsers` workloads while content changes at a steady rate.

### 2.5 Magic login

The agent issues the link (ops/wp §2): a token of 24 random bytes as 48 hex
characters, and an option row

    option_name  = cloudground_login_<sha256(token) in hex>
    option_value = <user id>:<unix expiry>
    autoload     = no

`option_name` is the table's unique index, so the lookup is one index probe.
The agent deletes any other `cloudground_login_*` row first, so a site has at
most one live link.

The link is `https://<domain>/?cloudground_login=1#<token>`. The fragment is
never sent to the server, so the token never reaches an access log.

- **GET** (any method but POST) returns a small page, `noindex`, no cache,
  `Referrer-Policy: no-referrer`, and a CSP that allows only its own inline
  script (by nonce) and a form post to the same origin. The script takes the
  token from the fragment, removes it from the address bar and posts it as
  `cloudground_token`. A fragment that is not 48 hex characters shows
  "Invalid login link." and posts nothing.
- **POST**: the token must be 48 lowercase hex characters (else 400). The row
  is read **from the database**, bypassing the object cache (the agent wrote
  it from wp-cli, whose cache PHP-FPM cannot see), by its exact name; its name
  is compared with the expected one with `hash_equals`. It is consumed by a
  `DELETE … WHERE option_name = …` that must affect exactly one row, so two
  concurrent posts cannot both sign in. Then the expiry is checked and the user
  must still exist and be able to `manage_options`. Any failure is 403 "This
  login link has expired"; a wrong token never consumes the right one.
  Success: `wp_set_auth_cookie(user, remember=false, secure=true)` and a 302 to
  `admin_url()`.

### 2.6 Object-cache statistics

`GET /?cloudground_stats=1` with header `X-CloudGround-Token: <token>`,
handled when the mu-plugins have loaded (`muplugins_loaded`), before any
regular plugin can interfere. Wrong or missing token, or no token configured:
403 `{"error":"forbidden"}`. APCu unavailable in the worker: 200
`{"error":"apcu unavailable"}`. Otherwise 200
`{"hits","misses","entries","mem_used","mem_total"}` from `apcu_cache_info`
and `apcu_sma_info` (`mem_total` = segment size × segments). Always
`Cache-Control: no-store`, JSON. The query string keeps it out of the page
cache. Nothing is written anywhere.

There is **no flush endpoint** any more (§3.4).

### 2.7 Performance Guard footer

On `wp_footer`, last: when no user is logged in and the request carries
`X-CloudGround-Probe` equal to `hex(hmac_sha256(key=token, "guard-probe"))`
(compared with `hash_equals`), print

    <!-- cloudground queries=<n> time=<seconds, 4 decimals> mem=<peak bytes> -->

Every other request, anonymous visitors included, gets nothing: the numbers
are internal, and a page cached with them would serve them to everyone. (The
vhost also never caches a response to a request with the probe header.)

### 2.8 WooCommerce cart fragments

With `CLOUDGROUND_DISABLE_CART_FRAGMENTS`, the `wc-cart-fragments` script is
dequeued on every page but the cart and checkout, both at
`wp_enqueue_scripts` (priority 100) and just before footer scripts print (a
widget enqueues it while rendering). Guarded by `function_exists('is_cart')`.

Still needed, verified on 2026-10-09: WooCommerce 11.2.1 no longer enqueues
the script itself, but registers it, its legacy Cart widget enqueues it, and
Storefront 4.6.2 (WooCommerce's own theme) enqueues it on every page. When it
loads in a browser with no stored fragments (every first visit), it posts
`/?wc-ajax=get_refreshed_fragments`, which the page cache never stores: one
PHP request per new visitor, on top of the cached page. Without it, the
mini-cart is rendered by the page; visitors with a cart bypass the page cache
(cookies `woocommerce_items_in_cart`, `woocommerce_cart_hash`), so it is
correct for them, and empty for everyone else, which is also correct.

### 2.9 Staging mail

When `wp_get_environment_type()` is `staging`, `pre_wp_mail` returns true:
`wp_mail()` reports success and sends nothing. Keyed on the environment, so
the same file is safe after a push to production.

### 2.10 Failed sign-ins (#21)

On `wp_login_failed` (wp-login.php, XML-RPC, any `wp_authenticate`) and
`application_password_failed_authentication` (the REST API), while headers
can still be sent: `X-CloudGround-Auth: failed=<n>`, `n` the failures so far
in this request, replacing the previous mark. A successful sign-in sends
nothing. nginx hides the header from the client and logs the address of a
POST that carries it to the agent (internal/engine SPEC §4.6), which counts
it towards blocking the address (internal/blocklist SPEC §3). WordPress's
XML-RPC stops checking credentials after the first failure of a request
(`system.multicall` included), so a request counts once.

## 3. The object-cache drop-in

### 3.1 When it shares

`CLOUDGROUND_OC_APCU` is defined to true when APCu is enabled and, in a CLI
process, `apc.enable_cli` is on too. CloudGround's FPM pools have APCu on and
CLI off, so: true in PHP-FPM, false in wp-cli. When false, the cache works
inside the request only (WordPress's non-persistent behaviour), and every
operation still answers correctly, `add` included (WordPress's locks use it).

### 3.2 Keys

`cg:<site>:<generation>:<blog>:<group>:<key>`, where `<site>` is the first
12 hex characters of sha256(`DB_NAME|$table_prefix|AUTH_KEY`), `<blog>` is 0
for global groups and single sites, and `<group>` is `default` when empty.
Every value read or written is also kept in the request; objects are cloned
in and out. Non-persistent groups stay in the request. `add` is `apcu_add`
(atomic), `incr`/`decr` compare-and-swap and floor at 0, the `*_multiple`
calls make one APCu call each for reads and writes. `wp_cache_supports` is
true for `add_multiple`, `set_multiple`, `get_multiple`, `delete_multiple`,
`flush_runtime`, `flush_group`.

### 3.3 Generation

`<generation>` is 16 hex characters read from `.cloudground-cache-epoch` next
to the drop-in's real path (`shared/`, because the drop-in is reached through
a link and PHP resolves it). It is read once per request. Missing or invalid:
a new random one is written. `flush()` writes a new random one (temporary
file, chmod 0644, rename) and switches to it, so every key changes in every
process at once. Random rather than a counter: a lost file would restart a
counter and revive old keys. The agent writes a fresh one when it turns the
cache on, so nothing APCu kept from before is served (ops/wp §6).

When the file cannot be written: the generation is `0000000000000000`, and
`flush()` returns false from wp-cli (it could not tell PHP-FPM), true in
PHP-FPM after deleting the site's keys directly.

`flush_group()` in PHP-FPM deletes the group's keys of the current
generation (every blog); in wp-cli it can reach nothing, so it flushes.

### 3.4 One way to flush across processes: the generation

wp-cli and PHP-FPM have separate APCu segments. A change made by wp-cli
(a plugin update, an import, a restore, a push from staging) reaches the
database but not the web tier's cache. The previous connector had two answers:
the generation file, and a flush endpoint in the mu-plugin
(`?cloudground_flush=1`) that the agent called over HTTPS so `wp_cache_flush()`
ran in an FPM worker. Only the generation is kept. Measured with
`connector/test/flush/run.sh` (php:8.4-fpm, APCu 5.1.28, the drop-in in the
agent's layout, an options "database" both processes read), 2026-10-09, on a
laptop's Docker VM; the wp-cli timings are 10-run averages in
`wordpress:cli-php8.3` against MariaDB 11.8, no OPcache in the CLI:

| | wp-cli flush (generation) | FPM flush (endpoint) |
| --- | --- | --- |
| control: wp-cli writes, no flush, FPM reads | stale (expected) | stale |
| FPM reads after the flush | new value | new value |
| shared/ not writable | `flushed:false` (agent restarts the master) | n/a |
| cost of the flush itself | 11–16 ms per process (drop-in only) | 1–2 ms per request with 50 000 entries |
| what production adds | WordPress core boot in wp-cli: `wp cache flush --skip-plugins --skip-themes` took 280 ms on WordPress 7.1.3 + WooCommerce 11.2.1 (`wp cache flush` with plugins: 710 ms) | a wp-cli run to read the token (`wp config get`: 59 ms), then TLS, nginx and a full WordPress boot with every plugin in FPM |
| per-request cost | one 16-byte file read, 2.7 µs, and one APCu fetch, 0.02 µs | none |
| reclaiming the old keys | after the response of the next FPM request: 7.8 ms for 50 000 entries | in the request |

Both are correct; the generation works from **both** sides (a flush in wp-cli
or in FPM moves it), needs no web server, no TLS, no token, no working theme
or plugin set, and no vhost that answers (a staging password, a maintenance
file, a security plugin or a fatal in any plugin stopped the endpoint, and
the agent then restarted the site's PHP master). The endpoint needed a
wp-cli run anyway, to read the token. So the agent now runs
`wp cache flush --skip-plugins --skip-themes` and reloads PHP (ops/wp §7).

### 3.5 Reclaim

A new generation leaves the old keys in memory. With `apc.ttl` 0 they would
stay until the segment filled, when APCu empties the whole segment at an
unpredictable time. So each FPM request compares the generation with the
one last seen (`cg:<site>:generation`, one fetch); on a change, one worker
(lock `cg:<site>:reclaiming`, 60 s) deletes every key of the site that is
not of the current generation, from a shutdown function registered last,
after `fastcgi_finish_request`.

### 3.6 API

`wp_cache_init`, `wp_cache_add`, `wp_cache_add_multiple`, `wp_cache_set`,
`wp_cache_set_multiple`, `wp_cache_replace`, `wp_cache_get`,
`wp_cache_get_multiple`, `wp_cache_delete`, `wp_cache_delete_multiple`,
`wp_cache_incr`, `wp_cache_decr`, `wp_cache_flush`, `wp_cache_flush_runtime`,
`wp_cache_flush_group`, `wp_cache_supports`, `wp_cache_close`,
`wp_cache_add_global_groups`, `wp_cache_add_non_persistent_groups`,
`wp_cache_switch_to_blog`, `wp_cache_reset` (deprecated, false), and the
class `WP_Object_Cache` with public `cache_hits`, `cache_misses` and
`stats()`.

## 4. Tests

- `php/tests/connector.php`: the mu-plugin against a small WordPress
  stand-in (`php/tests/wp.php`): purge targets per event, the shutdown queue
  and its payload, magic login, stats, the Guard footer, cart fragments,
  staging mail, the failed sign-in mark. `php/tests/object-cache.php`: the drop-in, run with
  `apc.enable_cli` 0 and 1. `go test ./connector` runs both with `php` when
  it is installed, else in `php:8.4-cli` with docker (as an unprivileged
  user), else skips. Without the APCu extension (php:8.4-cli has none) the
  shared-segment half is skipped; `test/flush/run.sh` runs both suites with
  APCu first.
- `test/flush/run.sh`: the measurement in §3.4.
- Checked by hand on 2026-10-09 against WordPress 7.1.3 and WooCommerce
  11.2.1 in docker, intercepting the purge request: the previous connector
  sent nothing for a price change made with `WC_Product::save()`, nothing for
  a stock change, only the new URL on a slug change, `/?p=ID` when a post was
  unpublished, nothing when one was deleted outright, and never a custom
  taxonomy, a product category or a sitemap; this one sends each.
- e2e (`e2e/features_test.go`, `e2e/e2e_test.go`, tag `e2e`): a price change
  made through WooCommerce purges the product page and its category archive;
  a slug change purges the old URL; a custom taxonomy's term archive is
  purged; the posts sitemap is purged. Each fails with the previous
  connector.
