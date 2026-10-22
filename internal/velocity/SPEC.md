# velocity — page-cache policy and purge

Two jobs: decide how a site's pages are cached (`PolicyFor`, read by the nginx renderer in
`internal/engine/nginx`), and remove cached pages from disk (`PurgeURLs`, `PurgeAll`, run by the
agent as root through `internal/agent/ops/cache`). The values are pinned by `testdata/*.golden`
(`golden_test.go`); caching defaults change only with benchmark numbers (AGENTS.md rules 5–6).

## 1. Policy — `PolicyFor(site) CachePolicy`

### 1.1 When there is no page cache

`Config.CacheEnabled` off, development mode on (`Config.DevModeUntil` set, §1.5), a maintenance page on (`Config.Maintenance.Enabled`, internal/engine SPEC §4.9), or a static or proxy site (static files are served from disk; proxied
sites are not cached): `CachePolicy{Enabled: false}` and nothing else set.

### 1.2 Every cached site

`Enabled`, `LockTimeoutSec = 5` (request coalescing: concurrent misses for a URL wait for one
regeneration up to 5 s), `ZoneSizeMB = 32` (key zone), `MaxSizeMB = 1024` (on disk).

### 1.3 By profile

| Profile | `TTLSec` (fresh) | `StaleSec` (stale-while-revalidate, stale-if-error) | `MaxSizeMB` | `IgnoreTrackingArgs` |
| --- | --- | --- | --- | --- |
| Balanced (also unset or unknown) | 600 | 7200 | 1024 | yes |
| Commerce | 300 | 3600 | 1024 | yes |
| Maximum | 86400 | 259200 | 4096 | yes |

`IgnoreTrackingArgs`: a query string made only of tracking parameters is cached under the bare
path instead of bypassing the cache (a campaign must not send every click to PHP; safe because a
response that sets a cookie is never cached). Any other query string bypasses.

`Config.CacheTTLSec > 0` replaces `TTLSec` (not `StaleSec`).

### 1.4 Bypass rules

Each site type has its own set (each list a fresh slice, in this order; how the renderer matches
them is internal/engine SPEC §5.1).

WordPress and WooCommerce sites, WordPress's set:

- cookies (a name that starts with any of these): `wordpress_logged_in_`, `wp-postpass_`,
  `comment_author_`, `woocommerce_cart_hash`, `woocommerce_items_in_cart`, `wp_woocommerce_session_`;
- path prefixes: `/wp-admin`, `/wp-login.php`, `/wp-cron.php`, `/xmlrpc.php`, `/wp-json/`, `/feed`.

PHP sites: WordPress's set (a PHP site may be a hand-installed WordPress), with `PHPSESSID` (PHP's
own session) appended to the cookies, then the PHP frameworks' default session cookies (R-10.5):
`_session=` (any `*_session` cookie: CodeIgniter's `ci_session`, Laravel's `laravel_session`),
`CAKEPHP=` (CakePHP), `XSRF-TOKEN=` (set with a session by Laravel and Angular apps),
`remember_web_` (Laravel's remember-me). Symfony, Yii, Laminas and Slim keep `PHPSESSID` by
default. Chosen to bypass every visitor holding a framework session and no anonymous one: a
generic `session` was left out because analytics and consent cookies carry it, and Drupal's
`SESS<hash>` because it is its host's own name (a Drupal site adds it below).

**A site's own bypass cookies** (`SiteConfig.BypassCookies`, R-10.5), on every type with a page
cache: each name valid by `ValidBypassCookie` (`^[A-Za-z0-9_.-]{1,64}$`, nothing nginx's
configuration reads as syntax), at most `MaxBypassCookies` (20), appended after the type's cookies
as `<name>=` (the name followed by its value, so `sid` does not catch `sidebar`); an invalid one is
dropped here, and refused by the API's PATCH and by `site.Validate`. This changes the request path
of PHP sites (more cookies matched): the integrator benchmarks it (AGENTS.md rule 5).

Laravel sites (R-5.2; until then they got WordPress's set, so a visitor holding a Laravel session
was served cached pages), Laravel's set only:

- cookies: `_session=` (any `*_session` cookie: Laravel's is `<APP_NAME slug>_session`,
  `laravel_session` by default), `XSRF-TOKEN=`, `remember_web_`;
- path prefixes: `/login`, `/logout`, `/register`, `/password/`, `/forgot-password`,
  `/reset-password`, `/email/verify`, `/admin`, `/nova`, `/horizon`, `/telescope`, `/sanctum/`,
  `/livewire/`, `/broadcasting/`.

A WooCommerce site, or a WordPress, WooCommerce or PHP site on the Commerce profile, also bypasses
`/cart`, `/checkout`, `/my-account`, `/addons`, `/wc-api`, `/?wc-ajax=` (appended after the paths
above). A Laravel site never gets them, whatever its profile.

A type with no rules has nil lists.

### 1.5 Development mode (#80)

`Config.DevModeUntil` is the end of a site's development mode. While it is set the policy is the
empty one of §1.1: nginx renders no cache for the site, so every request reaches PHP. The policy
reads whether the field is set, never the clock, so a render does not change with the time; the
panel ends the mode by clearing the field and rendering the site again (internal/api SPEC §3.15).

## 2. Cache entries on disk

- The key is `fastcgi_cache_key "$scheme$request_method$host$cg_key"` in the vhost:
  `CacheKey(scheme, method, host, requestURI)` is the concatenation of the four. nginx's `$host`
  is the request's host name, lower case and **without a port**; `$cg_key` is the request URI (the
  bare path for a tracking-only query).
- `CacheFilePath(dir, key)`: the entry is the file named by the key's MD5 in hex, under
  `levels=1:2`: `<dir>/<last hex digit>/<the two before it>/<md5>`.
- `SanitizeCacheDirName(domain)`: a site's cache directory name: `a-z 0-9 . -` kept, `A-Z` lowered,
  every other character (also each byte of a multi-byte one is one rune → one `_`) becomes `_`.

Deleting the file removes the entry; nginx needs no purge module, and the agent (root) reaches
every site's cache directory.

## 3. `PurgeURLs(dir, urls, extraHosts...) (removed int, error)`

For each absolute URL, in order: the entries of its request URI (path and query, `/` for none)
for GET and HEAD, under `https` and `http`, and under the URL's host followed by every extra host
(the site's domain and aliases, since `$host` is part of the key). Every host is taken as `$host`
holds it: lower case and without a port (`https://example.com:443/p/` purges the entries of
`example.com`), each host once. Until R-5.2 the URL's host was used as written, port and case
included: a key nginx never writes, so such a URL purged nothing unless the same host also came
as an extra host. **The fix changes nothing in production today:** the only caller (ops/cache)
always passes the site's domain and aliases as extra hosts, which already covered every URL the
API or the connector accepts. It is kept as a correctness fix, so a caller without extra hosts
gets the same result. A URL that does not parse or
has no host stops the purge with `invalid purge url "<url>"`, after what earlier URLs removed.
A missing entry is not an error; any other removal failure is (`purge <url>: <err>`).
`removed` counts the entries that existed.

## 4. `PurgeAll(dir) (removed int, error)`

Removes every entry under `dir` (each top-level level directory in turn) and counts the non-directory
files removed. A missing `dir` is 0 and no error.
