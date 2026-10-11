# ops/wp — WordPress toolkit, object cache and cache warmer

What the agent does inside a WordPress site on the operator's behalf: a
one-shot login link, plugin/theme/core updates, search-replace, the APCu
object cache, making an out-of-band change visible to the live site, the
object-cache numbers, and warming the full-page cache of any site.

Sources: the e2e suite (`e2e/features_test.go`: object cache, magic login,
R-0.5 concurrent object-cache changes, the page-cache checks), the API
callers (`internal/api/wp.go`), the connector and the APCu drop-in
(`connector/SPEC.md`: magic login, stats, the generation that carries a
flush across processes), `docs/caching.md`.

Every wp-cli call goes through `host.WPCLI` (the site's user, `--path=<root>/current`,
the minimal environment). Every file touched in a site tree goes through a
`sitefs.Root`. RPC names, parameters and results are those of
`internal/agentrpc/methods.go`.

## 1. Locks

The RPC server takes them (method table):

- `WPObjectCache` and `WPUpdate`: `LockLiveSite`. Both change the live
  release's files (the drop-in link; plugin and core files), so they are
  ordered against deploys, restores and deletion, and a site deleted while the
  call waited is refused with `conflict`. The old `WPUpdate` took no lock.
- `WPMagicLogin`, `WPPlugins`, `CacheStats`, `WarmCache`: no site lock. They
  write nothing in the tree (the login writes one option row).

`InstallObjectCache` (provisioning) runs under CreateSite, before the site
can be reached.

## 2. WPMagicLogin

1. The administrator is the one with the **lowest ID**
   (`user list --role=administrator --field=ID --orderby=ID --order=ASC --number=1`).
   None → `conflict` "no administrator user found".
2. Earlier links are removed: `option list --search=cloudground_login_*
   --field=option_name`, then `option delete` of every name that is the prefix
   and 64 hex characters. One live link per site.
3. A token of 24 random bytes, hex (48 characters, what the connector accepts).
4. `option add cloudground_login_<sha256(token)> <admin id>:<unix expiry>
   --autoload=no`, expiry now + 5 minutes. Only the hash reaches argv and the
   database. The option name is the `options` table's unique index, so the
   connector finds the row with one index lookup (the old row was a user-meta
   value matched with `LIKE`).
5. Result `{url: "https://<domain>/?cloudground_login=1#<token>"}`. The token
   is in the fragment, which browsers do not send; the connector's page posts
   it back, reads the row **by its name** from the database, consumes it with
   a DELETE that must affect one row, checks the expiry and that the user can
   still `manage_options`, and signs in once (`connector/SPEC.md` §2.5).

## 3. WPPlugins

`plugin list` and `theme list`, `--format=json --fields=name,status,version,update`.
A failing wp-cli or output that is not that JSON is an error (the old code
returned empty lists, which read as "no plugins"). `update` is a string
(`available`, `none`, `unavailable`, `version higher than expected`) for an
item wp-cli can check and the boolean `false` for one it cannot (must-use
plugins and drop-ins, so every CloudGround WordPress site has one): `false`,
absent or empty read as `none`, `true` as `available`; any other JSON type is
an error. A numeric `version` is kept as its text.

## 4. WPUpdate

`what`: `core` (`core update`, then `core update-db`), `plugins`
(`plugin update --all`), `themes` (`theme update --all`), `all` (the four
commands, each run even if an earlier one failed, failures joined). Anything
else → `invalid`. Afterwards (also after a partial `all`) the site's state is
refreshed (§7); a failed refresh is logged, not returned. Result
`{updated: <what>}`.

## 5. SearchReplace (agent-internal, migration)

`search-replace <from> <to> --all-tables-with-prefix --precise --skip-columns=guid`.
Empty values, equal values, or a value starting with `-` → `invalid`.

### Table prefix (agent-internal, migration)

The configuration is `shared/wp-config.php`, which every release links. It
is read as text, never run, through the site's root (`O_NOFOLLOW`, at most
1 MiB):

- `ParseTablePrefix(text)`: exactly one statement may start a line by
  assigning `$table_prefix` (a commented-out line does not count), and it
  must be a quoted literal alone (`$table_prefix = 'wp_';`, either quote),
  whose value matches `^[A-Za-z0-9_]{1,64}$`, the characters WordPress
  accepts (an empty prefix is refused). Anything else, a computed value or
  two assignments, is not read (`ErrNoTablePrefix`, or `invalid`).
- `TablePrefix(ctx, site)` is that, on the site's file.
- `SetTablePrefix(ctx, site, p)`: `p` must match the same pattern, else
  `invalid` before the file is touched (the value is written into PHP); the
  one literal is replaced and the file written to a temporary name and
  renamed over (`sitefs.WriteFile`, owner and mode kept), so PHP reads the
  old file or the new one, never a half-written one. (It used to be `wp
  config set`, which rewrites the file in place.)

The migration also reads the archive's own `wp-config.php` with
`ParseTablePrefix` to choose among a dump's installations.

## 6. Object cache (WPObjectCache, InstallObjectCache)

APCu only. `what: "redis"` → `invalid`: the API never selects it, the code
path had no test, and Redis on a single server measured slower than APCu
(`docs/caching.md`).

**Enable:**
1. `shared/object-cache.php` ← the embedded drop-in (`connector.ObjectCacheAPCu`),
   0640, site user, atomic replace (a planted symlink at the name is replaced,
   never followed).
2. When the live release had **no** drop-in (the cache was off, or the site is
   new): `shared/.cloudground-cache-epoch` ← a **fresh random epoch** (16 hex),
   0644, site user. The drop-in prefixes every APCu key with it. Without this,
   turning the cache off, changing the database (wp-cli, an import) and turning
   it back on served the entries APCu still held from before: the FPM master,
   and its APCu segment, survive a reload. The new epoch abandons them. When
   the cache was already on, the epoch, and the warm cache, are kept.
3. The live release's `wp-content/object-cache.php` → `../../../shared/object-cache.php`.
4. PHP reload (OPcache).

**Disable:** the live release's link is removed (a missing one is fine), then a
PHP reload. `shared/object-cache.php` stays, so deploys keep agreeing with the
stored setting through `LinkDropin`, which release builds call.

**Connector:** `InstallConnector(r, release)` writes the embedded connector
(`connector.PHP`) to `<release>/wp-content/mu-plugins/cloudground-connector.php`
(atomic replace, 0644, the `Root`'s owner), creating `mu-plugins` if needed
and leaving the tree's other mu-plugins alone. Provisioning and every release
build (`ops/release`, migration included) call it: a tree without it serves
and caches, but nothing ever purges.

Result `{object_cache: "enabled", backend: "apcu"}` or `{object_cache: "disabled"}`.

## 7. Refresh (refreshSiteState)

Make an out-of-band change (wp-cli, import, restore, update) visible to the
live site, touching only that site:

- no PHP version → nothing to do;
- WordPress: `cache flush --skip-plugins --skip-themes`, then reload PHP
  (OPcache). wp-cli has its own APCu segment, so the flush cannot delete
  PHP-FPM's entries; the drop-in writes a new generation to
  `shared/.cloudground-cache-epoch` instead, and PHP-FPM's next request reads
  it and none of the old entries (`connector/SPEC.md` §3.3–3.5). Plugins and
  theme are skipped so one that breaks wp-cli does not stop the flush; the
  drop-in is loaded either way. When wp-cli fails (the generation file cannot
  be written, so the drop-in's flush returns false; or WordPress does not
  boot):
- otherwise: restart the site's PHP master, which clears its APCu and OPcache
  and nobody else's.

The old refresh asked the connector to flush through an FPM worker over
HTTPS (`?cloudground_flush=1`): it needed a wp-cli run anyway to read the
token, then nginx, TLS and a full WordPress boot with every plugin, and any of
a staging password, a maintenance file or a plugin's fatal turned it into a
restart. Measured on 2026-10-09 the two are equally correct and the generation
is the one that works from both sides; the endpoint is gone
(`connector/SPEC.md` §3.4).

## 8. CacheStats

- Backend: `apcu` when the live release has the drop-in link, else `none`.
  (Redis is not detected any more: nothing enables it.)
- `apcu`: the numbers come from the site's connector, through its FPM pool
  (§9, `GET /?cloudground_stats=1`, header `X-CloudGround-Token: <token>`):
  `{hits, misses, entries, mem_used, mem_total}`. A failed or refused request
  is an error. **No file is written into the docroot**: the old code wrote
  `current/cloudground-cache-stats.php` on every call, guarded only by
  `REMOTE_ADDR`, and it then travelled with every release, backup and staging
  copy.
- `hit_rate_pct` = hits × 100 / (hits + misses), 0 when both are 0.

## 9. Loopback

Requests to a site's own pages always dial `127.0.0.1:443` (configurable for
tests), whatever the domain resolves to, with the site's domain as SNI and
Host. TLS is not verified (the vhost may still be on the bootstrap
certificate; the token or the cache is what matters). Redirects are not
followed. A staging site's internal key goes in `X-CloudGround-Internal`.
Responses are read up to a bound and drained.

## 10. WarmCache

Params `{site, max_urls}`; result `{warmed, cached}`.

1. `max_urls` ≤ 0 or > 500 → 200.
2. URL list:
   - WordPress: `post list --post_type=post,page,product --post_status=publish
     --field=url --posts_per_page=<max>`. `product` is WooCommerce's: the old
     list had only posts and pages, so no shop page ever got warmed. A wp-cli
     failure falls back to the sitemap;
   - otherwise, or when wp-cli gave nothing: the sitemap (§10.1).
3. Paths: `/` first, then each URL that names **this site** (§10.2), path and
   query kept, duplicates dropped, at most `max` after `/`. A URL naming
   anything else is **skipped**: the old code turned it into `/`, so a sitemap
   of 200 foreign URLs warmed the home page 200 times.
4. **One GET per path**, `Accept-Encoding: gzip`, user agent
   `CloudGround-CacheWarmer/1.0`. The page cache stores one gzip variant keyed
   without the encoding and gunzips for clients without gzip, so a single
   request fills the entry every client is served from. The old warmer made
   up to three requests per URL (a "plain" pass for a second cache entry that
   no longer exists, then one or two gzip passes).
5. A worker pool: 2 × CPUs, at least 2, at most 8 (`Ops.WarmWorkers` overrides),
   sharing keep-alive connections. Each request has a 30 s timeout; the whole
   run is bounded by the method's deadline (30 minutes) and stops early when
   it is cancelled.
6. `warmed` = requests that got a response. `cached` = responses the page cache
   took: `X-CloudGround-Cache` `HIT`, or `MISS`/`EXPIRED` with status 200, 301
   or 404, no `Set-Cookie`, and no `Cache-Control` `no-store`/`private` (the
   template's `fastcgi_no_cache` rules). The old `cached` counted HITs on a
   second request.

### 10.1 Sitemap

Tried in order until one yields pages: `/wp-sitemap.xml`, `/sitemap_index.xml`,
`/sitemap.xml`, `/?sitemap=index`. Parsed with `encoding/xml` (namespaces
ignored, entities decoded by the parser): `<urlset><url><loc>` are pages,
`<sitemapindex><sitemap><loc>` are sitemaps, followed one level deep, at most
50 of them, each only if it names this site (§10.2). A response other than
200 or bigger than 8 MiB is skipped.

### 10.2 This site

A `<loc>` or a wp-cli URL names this site when it is:
- a path (`/x`, not `//x`), or
- `https://<domain>[:443]/…` with the domain matched case-insensitively, no
  user info.

Anything else (another host, a subdomain, `http:`, another port, `user@`) is
not this site. The request is still made to 127.0.0.1:443 with the site's own
Host, but the sitemap is written by the site, so nothing in it may decide what
the warmer, which runs as root, requests.

## 11. Installing WordPress and WooCommerce (site creation)

Called by `ops/sites` while it provisions a site, under the site's lock and
before the site has a vhost. Every wp-cli run of this package is the site's
user in the site's slice (`host.Invocation.Slice`, `cg-site-<id>.slice`).

`InstallWordPress(ctx, r, site, release, Install)` runs, on
`<root>/<release>` (the live link already points there):

1. `core download --locale=en_US`;
2. `config create --dbname=<db> --dbuser=<user> --prompt=dbpass
   --dbhost=<host>:<port> --skip-check`, the database password on stdin;
3. with a connector token: `config set CLOUDGROUND_SITE_TOKEN --prompt` (the
   token on stdin), `config set CLOUDGROUND_ENDPOINT http://127.0.0.1:9080`,
   `config set DISABLE_WP_CRON true --raw` (WordPress's events run from the
   crontab, `ops/cron`);
4. `core install --url=https://<domain> --title=<title or domain>
   --admin_user=<u> --prompt=admin_password --admin_email=<e> --skip-email`,
   the password on stdin;
5. `rewrite structure /%postname%/ --hard`; a failure is logged, the site
   works without pretty permalinks;
6. `wp-config.php`, when wp-cli wrote a regular file, moves to
   `shared/wp-config.php`, mode 0600, and the release gets the relative link
   `wp-config.php -> ../../shared/wp-config.php`;
7. `wp-content/uploads` becomes `-> ../../../shared/uploads`;
8. the connector is installed (`InstallConnector`).

A failing step is an error naming it; no secret reaches argv.

`InstallWooCommerce(ctx, site)` runs on the live release: `plugin install
woocommerce --activate`, `rewrite flush` (its pages' rules), `config set
CLOUDGROUND_DISABLE_CART_FRAGMENTS true --raw`. Any failure is an error: a
"woocommerce" site without WooCommerce serves 404 on /shop. No PHP reload
follows: the site's master is started fresh by the render that comes next.

## 12. Writing into a release's wp-content (R-10.5)

A release's `wp-content` belongs to the tenant, who may make it, or
`wp-content/mu-plugins`, a symlink to another folder of the tree (the
site's `.ssh` among them), while root writes there. So:

- `InstallConnector(r, release)`: `wp-content` and then `mu-plugins` are
  each made (0750) when missing and opened with `sitefs.Root.SubStrict`
  (`RESOLVE_NO_SYMLINKS`: no link anywhere on the path); a link at either is
  an error and nothing is written. The plugin is `Replace`d through the
  `mu-plugins` descriptor.
- `LinkDropin(r, release, on)`: `wp-content` is opened once with
  `SubStrict` (missing: nothing to do; a link: an error), and the drop-in
  link is removed and made through that descriptor. The check that it is a
  real directory and the write are the same open: there is no window to
  swap a link in between.

`release` is a real path (`releases/<id>`, or a build directory), never
through `current`.
