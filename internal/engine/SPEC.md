# `internal/engine` — the web-serving engine (specification)

An engine turns one site's description (`engine.Input`) into the files the
web server reads. nginx + PHP-FPM is the only engine (`internal/engine/nginx`);
the agent writes and loads what it returns (`internal/agent/ops/sites`
SPEC §4). The engine is pure: no file system, no processes, so every byte
it produces is pinned by tests.

Sources: the render matrix (`nginx/testdata/matrix`, taken from the
templates before the Phase 5 rewrite, R-5.1), the e2e suite (headers,
cache HIT/BYPASS, HTTP/3, staging, `max_execution`), the measurements quoted
in the rendered comments (they are part of the output and stay with it) and
`internal/install` (the default servers, the rejected tunings: its SPEC §4).

## 1. Input

| Field | Meaning |
|---|---|
| `Site` | the site record (`state.Site`): ID, domain, aliases, type, profile, `Config` |
| `Policy` | the page-cache policy (`velocity.PolicyFor`, §5) |
| `DocRoot` | what nginx serves: `<root>/current[/<public_root>]` |
| `SymlinkFrom` | where `disable_symlinks` starts checking: `<root>/current` for a site with a public root (a link inside the public root is checked too, R-4.3); empty is `$document_root` |
| `PHPSocket` | the site's FPM socket; empty for static and proxy sites |
| `CacheDir`, `LogDir` | page-cache directory, log directory |
| `CertAvailable`, `CertFullchain`, `CertKey` | the site's certificate, and whether it exists |
| `FallbackCert`, `FallbackKey` | the self-signed certificate used until it does |
| `ACMEWebroot` | where HTTP-01 challenges are served from |
| `ClientMaxBody` | request body limit; empty is `128m` |
| `HTTP3` | this nginx has `ngx_http_v3_module` (QUIC on 443/udp) |
| `HTTP2Directive` | this nginx is ≥ 1.25.1: `http2 on;` instead of `listen … http2` |
| `BasicAuthFile`, `InternalKey` | staging: the password file, and the header value that passes it |
| `ProtectedPaths` | the site's protected paths (#26), each `{Path, File}`: the path and its password file (§4.5) |
| `AuthLogSocket` | the agent's datagram socket for failed sign-ins (#21, §4.6); empty logs none |
| `Blocking` | the visitors the site refuses (#25, §4.7): addresses, user agents, countries already resolved to their prefixes, and this machine's addresses (never refused); nil refuses nobody |

`Renderer` has `SiteFiles(Input) (path → content)`, `GlobalFiles()` and
`Routing(Input) (RoutingView)` (§7.5).

## 2. Files

| File | When |
|---|---|
| `<SitesDir>/<domain>.conf` (default `/etc/nginx/sites-enabled`) | always: the vhost (§4) |
| `<ConfDir>/cloudground-cache-<id>.conf` (default `/etc/nginx/conf.d`) | the policy caches: the cache zone (§3.2) |
| `<ConfDir>/cloudground-block-<id>.conf` | `Blocking` set: the site's refused visitors (§4.7) |
| `<ConfDir>/cloudground-global.conf` | `GlobalFiles`: http-level maps and tuning (§3.1) |

Refused before anything is rendered:

- a domain or alias that is not a plain lowercase hostname (`site.ValidateDomain`):
  it is pasted into the configuration;
- a proxy upstream that does not normalise (`site.NormalizeProxyUpstream`);
  the normalised form is what is rendered;
- a custom routing block that does not pass §7.

## 3. http level

### 3.1 Global file

Fixed text: the `cloudground` log format; the gzip tuning (`gzip_vary`,
level 5, 1024 bytes, the types list; `gzip on` itself is the distro's);
`open_file_cache max=20000 inactive=60s`, valid 60s, min_uses 2, errors on;
the maps `$cg_path` (the request path), `$cg_args_bypass` (0 when the query
string is empty or made only of tracking parameters: `utm_*`, `fbclid`,
`gclid`, `gbraid`, `wbraid`, `msclkid`, `dclid`, `mc_cid`, `mc_eid`, `_ga`,
`_gl`, `igshid`, `yclid`, `twclid`, `ttclid`), `$cg_key` (`$cg_path` when the
arguments do not bypass, else `$request_uri`), `$cg_no_store` (upstream
`Cache-Control` with `no-store` or `private`) and `$cg_private_req`
(`Authorization` or `X-CloudGround-Probe` present); for the block list
(§4.6), the map `$cg_auth_failed` (the upstream's `X-CloudGround-Auth` on a
POST, empty for any other method) and the log format `cloudground_auth`
(`$remote_addr $cg_auth_failed`). Every comment is part of the file.

### 3.2 Cache zone

```
fastcgi_cache_path <CacheDir> levels=1:2 keys_zone=cg_<id>:<ZoneSizeMB>m
                   max_size=<MaxSizeMB>m inactive=<inactive>s use_temp_path=off;
```

`inactive` is a week, or TTL + stale when that is longer.

## 4. The vhost

In this order. "Security headers" is the set of §4.3.

1. Header comment: managed, and `# Site: <domain> (id <id>, type <type>, profile <profile>)`.
2. Protected paths (§4.5): the two maps `$cg_auth_realm_<id>` and
   `$cg_auth_file_<id>`. WordPress hardening with XML-RPC off (§4.8): the
   geo block `$cg_xmlrpc_from_<id>` and the map `$cg_xmlrpc_deny_<id>`.
3. Staging (`BasicAuthFile` set): a map from `$http_x_cloudground_internal`
   to `$cg_staging_realm_<id>`: the internal key (when there is one) gives
   `off`, anything else `"Staging"`.
4. Port 80: `listen 80` and `[::]:80`, the server names (domain then
   aliases), the ACME location (`root <ACMEWebroot>`), and everything else
   `301` to `https://$host$request_uri` (with a canonical host, to `https://<canonical>$request_uri`, §4.10).
5. Port 443:
   - listen: `443 ssl http2` (both families), or with `HTTP2Directive`
     `443 ssl` and `http2 on;`; with `HTTP3` also `443 quic` (no
     `reuseport`: the installer's default server holds the only one);
   - server names, `server_tokens off`;
   - with `Blocking`, `if ($cg_block_<id>) { return 403; }` (§4.7);
   - staging: `auth_basic $cg_staging_realm_<id>` and the password file;
   - protected paths: `auth_basic $cg_auth_realm_<id>` and
     `auth_basic_user_file $cg_auth_file_<id>` (§4.5);
   - `tcp_nodelay on; tcp_nopush off;` (measured: 850 → 7800 req/s);
   - certificate: the site's when `CertAvailable`, else the fallback;
     TLS 1.2 and 1.3, `ssl_prefer_server_ciphers off`, session timeout 1d,
     `shared:cg_ssl:10m`;
   - logs: `access.log` in the `cloudground` format, buffered 64k / 1s;
     `error.log` at `warn`; on a WordPress or WooCommerce site with a PHP
     socket and `AuthLogSocket`, the failed sign-ins' log (§4.6);
   - `client_max_body_size`;
   - `http3 on;` with `HTTP3`;
   - the security headers, at server level;
   - WordPress hardening (§4.8): `if ($cg_xmlrpc_deny_<id>) { return 403; }`
     with XML-RPC off, `if ($uri = /wp-login.php) { return 404; }` with a
     login path;
   - **proxy site**: the routing block (§7), whose default is
     `location /` proxying to the upstream (HTTP/1.1, Host, X-Real-IP,
     X-Forwarded-For, X-Forwarded-Proto, Upgrade/Connection for websockets,
     60s read timeout);
   - **any other site**: `root <DocRoot>`, `index index.php index.html
     index.htm`, `disable_symlinks if_not_owner from=<SymlinkFrom>` (or
     `from=$document_root` when it is empty); the
     hidden-files location (`~ /\.(?!well-known)` → `deny all`); the
     leftovers location (§4.4, `return 404`); for a
     WordPress or WooCommerce site, the uploads location
     (`~* ^/wp-content/uploads/.*\.php$` → `deny all`, SR-T9: a `.php` a
     plugin let into the uploads is never run; a regex location, so it
     must come before the routing block's and the PHP location, since the
     first regex that matches wins; requests outside that prefix see one
     more regex test, measured with the benchmarks); with hardening
     (§4.8), the readme location and the login path's exact location; the static
     assets location (css, js, mjs, images, fonts, mp4, webm, pdf: `expires
     30d`, `Cache-Control: public, immutable`, the security headers, no
     access log, `gzip_static on`, `try_files $uri =404`); then
     - static site: the routing block, default `try_files $uri $uri/
       /index.html =404`;
     - PHP site (a socket, and neither static nor proxy): the routing block,
       default `try_files $uri $uri/ /index.php?$args`; with a caching
       policy the bypass rules (§5.1); the PHP location (§4.2);
     - a PHP-type site without a socket gets no routing block at all.

### 4.1 Whitespace

The output is byte-identical to the matrix; in particular the security
headers repeated inside a location keep the four-space indent of the
server level, and blocks are separated by one empty line exactly where the
matrix has one.

### 4.2 PHP location

`location ~ \.php$`: `try_files $uri =404`, `fastcgi_params`,
`SCRIPT_FILENAME $document_root$fastcgi_script_name`, `HTTPS on`, the socket,
`fastcgi_read_timeout` = `Config.PHP.RequestTimeoutSeconds()`, buffers
`16 32k` and `64k`. With a caching policy: PHP always sees
`Accept-Encoding: gzip` and nginx gunzips for clients that do not take it;
`open_file_cache_valid 1s` (a purge is seen within a second); the zone; the
key `$scheme$request_method$host$cg_key` (with `IgnoreTrackingArgs`) or
`…$request_uri`; valid `200 301` for the TTL and `404` for 60s; GET and HEAD;
bypass on `$cg_skip_<id>` and `$cg_private_req`, no-store on those and
`$cg_no_store`; the lock with the policy's timeout; stale on `updating error
timeout invalid_header http_500 http_503` with background update;
`Cache-Control Expires Vary` ignored — **never `Set-Cookie`**; the
`X-CloudGround-Cache` header. On a site that logs failed sign-ins (§4.6),
`fastcgi_hide_header X-CloudGround-Auth` after the buffers. Then the
security headers.

### 4.3 Security headers

`X-Content-Type-Options nosniff`, `X-Frame-Options SAMEORIGIN`,
`Referrer-Policy strict-origin-when-cross-origin`; with `HTTP3`
`Alt-Svc 'h3=":443"; ma=86400'`; with `HSTS` **and** a real certificate
`Strict-Transport-Security "max-age=31536000"` (never on the fallback). All
`always`. nginx inherits `add_header` only into a level that declares none,
so the set is repeated in every location that adds a header of its own:
the static assets location, the PHP location, and any block of a custom
routing that does (§7.4).

### 4.4 Leftovers

Files an operator or a plugin leaves in a public root are never served:
a case-insensitive regex location answers 404 for a path ending in
`.bak`, `.old`, `.orig`, `.save`, `.swp`, `.swo` or `~`; an SQL dump
(`.sql`, `.sql.gz`, `.sql.bz2`, `.sql.xz`, `.sql.zst`, `.sql.zip`); a copy of
`wp-config.php` under a longer name (`wp-config.php.bak`,
`wp-config.php-old`; `wp-config.php` itself goes to the PHP location); and an
archive at the top of the document root (`.tar`, `.tar.gz`, `.tgz`,
`.tar.bz2`, `.tbz2`, `.tar.xz`, `.txz`, `.tar.zst`, `.zip`, `.7z`, `.rar`),
where dumps are made. An archive deeper down (a plugin's download) is
served. 404 rather than 403, so the answer does not confirm the file
exists. A regex location: it comes right after the hidden-files one and
before the uploads, static, routing and PHP locations, since the first
regex that matches wins; a request that matches none of them pays one more
regex test (a cached page is served from the PHP location, so it pays it
too: measured with `bench/`).

### 4.5 Protected paths (#26)

A path such as `/wp-admin` or a private folder, with its own username and
password. Refused before anything is rendered: a path that is not
`site.ValidProtectedPath` (absolute, segments of letters, digits and
`. _ ~ -`, no `.` or `..` segment, not `/`), a file name that is not an
absolute path free of white space, `;`, braces, quotes and `$`, and
protected paths on a staging copy (`BasicAuthFile` set: the copy asks for
its own password everywhere already).

At http level, two maps from `$uri` (the normalised path: decoded, with dot
segments and double slashes resolved, so no other spelling of a path gets
past them):

```
map $uri $cg_auth_realm_<id> {
    default  off;
    "~^/wp-admin/admin-ajax\.php$"  off;      # WordPress and WooCommerce only
    "~^<quoted path>(?:/|$)"  "Restricted";    # one per path, longest first
}
map $uri $cg_auth_file_<id> {
    default  <the first path's file>;
    "~^<quoted path>(?:/|$)"  <its file>;
}
```

The path matches itself and everything below it, never a longer name
(`/private` is not `/privateer`). The longest path comes first, so a path
inside another takes its own credentials. On a WordPress site
`/wp-admin/admin-ajax.php` stays open: themes and plugins call it from the
public pages, and protecting `/wp-admin` must not break them.

The 443 server then says `auth_basic $cg_auth_realm_<id>;
auth_basic_user_file $cg_auth_file_<id>;` at server level, so every location
inherits it. A prefix location per path would not do: `/wp-admin/x.php`
matches the PHP regex location, not the prefix one, and would reach PHP
without the password. The access phase runs before the page cache is read,
so a cached page of a protected path is not served without the password.
Port 80 only redirects.

### 4.6 Failed sign-ins (#21)

A WordPress or WooCommerce site with a PHP socket, rendered with an
`AuthLogSocket`, sends each failed sign-in to the agent, which counts it
towards blocking the address (internal/blocklist SPEC §3, ops/blocklist
SPEC §1.2). At server level of the 443 server, after the access log:

```
access_log syslog:server=unix:<AuthLogSocket>,nohostname,tag=cloudground cloudground_auth if=$cg_auth_failed;
```

The connector marks WordPress's answer to a failed sign-in (wp-login.php,
xmlrpc.php, application passwords) with `X-CloudGround-Auth: failed=<n>`
(connector SPEC §2.10); `$cg_auth_failed` is that value on a POST and empty
otherwise, so only those requests write a line. Only a POST: a response is
stored in the page cache with its headers, and a GET served from it would
carry a mark to every visitor. The PHP location hides the header from the
client. Unbuffered (one datagram per failure, and failures are rare next to
the traffic); every other request pays the map lookup on the method when it
is logged. Measured in `bench/blocklist.md`.

A site can mark any of its own responses, so a site's code can count
failures against the addresses that actually sent it a POST, and against no
other: nginx writes the address, not the site.

### 4.7 Refused visitors (#25)

A site may refuse addresses or networks, user agents, and countries (or
serve only some countries). The rules are the panel's (`config.blocking`,
internal/api SPEC §3.13); the agent resolves the countries to their
prefixes from DB-IP's data (ops/blocklist SPEC §4) and fills `Blocking`.

Refused before anything is rendered: a user agent pattern that is not
`site.ValidUserAgentPattern` (2–100 printable ASCII characters, none of `"
\ $ { }`), a country that is not two capital letters, an invalid prefix,
and serving only some countries with none named.

The rules go to one http-level file per site, `cloudground-block-<id>.conf`
(beside the cache zone; the vhost stays readable in the routing editor):

```
# Written by CloudGround: the visitors <domain> (site <id>) refuses (#25).
# Country data: IP Geolocation by DB-IP (https://db-ip.com), CC BY 4.0.   (with countries)
geo $cg_block_ip_<id> {
    default  0;
    127.0.0.0/8  x;   ::1/128  x;   <each of this machine's addresses>  x;
    <each refused prefix>  1;
}
geo $cg_block_cc_<id> {          (with countries)
    default  0;                  (1 when serving only the countries named)
    <each prefix of the countries>  1;   (0 when serving only them)
}
map $http_user_agent $cg_block_ua_<id> {    (with user agents)
    default  0;
    "~*(?:<pattern>|<pattern>)"  1;          (each regex-quoted: a substring, any case)
}
map "$cg_block_ip_<id>[$cg_block_cc_<id>][$cg_block_ua_<id>]" $cg_block_<id> {
    default  1;
    "0…0"  0;
    "x…"  0;          (every combination after an x)
}
```

- `$cg_block_<id>` is 1 when any rule refuses the request and the address is
  not this machine's: loopback and the machine's own addresses (`x`) are
  never refused, so the panel's own requests to the site (Performance
  Guard, the warmer, the uptime check from the public address) always get
  through. The longest prefix wins in a `geo`, so an exemption holds inside
  a refused network.
- Prefixes are written once each, sorted, without those another covers.
  Lookups: one radix-tree match per `geo`, one case-insensitive regex for all
  the user agents, and one hash lookup for the combination, all evaluated
  only for this site's requests.
- In the 443 server, after `server_tokens`: `if ($cg_block_<id>) { return
  403; }`. The server rewrite phase runs before locations, so before the
  page cache is read: a refused visitor never gets a cached page. Port 80
  only redirects and serves ACME challenges (Let's Encrypt's validation may
  come from a refused country), so it refuses nobody.
- Without rules there is no file and no `if`: such a site pays nothing.
  Rules taken away remove the file before the reload (ops/sites SPEC §4).

The cost is in `bench/blocklist.md` (to measure on the VPS).

### 4.8 WordPress hardening (#75)

`Site.Config.WPHardening`, on a WordPress or WooCommerce site with a PHP
socket; every other site ignores it. Each item is on or off on its own.
Refused before anything is rendered: a login path that is not
`site.ValidLoginPath` (3–64 of `a-z 0-9 -`, starting with a letter or
digit, not one of WordPress's or WooCommerce's first path segments), and an
exception that is not an IP address or network in canonical form
(`site.NormalizeXMLRPCAllow`).

- **XML-RPC off** (`xmlrpc_off`): at http level a geo block
  `$cg_xmlrpc_from_<id>` (`default 0`, each exception `1`) and a map
  `"$uri|$cg_xmlrpc_from_<id>|$arg_for" → $cg_xmlrpc_deny_<id>`, `1` for
  `~^/xmlrpc\.php\|0\|(?!jetpack$)`: `/xmlrpc.php` (the normalised path) from
  an address that is not an exception, unless the query's first `for` is
  `jetpack`. At server level, before any location is chosen,
  `if ($cg_xmlrpc_deny_<id>) { return 403; }`; the PHP location passes
  `fastcgi_param CLOUDGROUND_XMLRPC_FROM $cg_xmlrpc_from_<id>`. nginx and PHP
  can read a query string differently (`for=jetpack&for=x`): the connector
  decides again with PHP's reading and lets `for=jetpack` through only when
  Jetpack is active (connector/SPEC.md §2.12), so nginx's test only saves
  the PHP work for everything else. Jetpack's servers call XML-RPC with
  `for=jetpack` and sign the request; with it Jetpack serves only its own
  methods.
- **Readme and licence files** (`block_readme`): a case-insensitive regex
  location, after the uploads one, answering 404 for
  `/readme.html`, `/license.txt` and a plugin's or theme's top-level
  `readme`, `changelog` or `license` `.txt`/`.md`/`.html`: they give the
  versions away.
- **Login path** (`login_path`, e.g. `my-door`): `location = /my-door {
  rewrite ^ /wp-login.php last; }` after the readme location, and at
  server level `if ($uri = /wp-login.php) { return 404; }`. The server-level
  `if` runs once, on the request as it came (normalised, so no spelling of
  the path slips past); the rewrite is an internal redirect to the PHP
  location, which the server level does not see again. With a caching
  policy, `$uri = /my-door` also skips the cache (§5.1). The connector
  rewrites WordPress's links and redirects to the new path.
- The file editor and the version (`file_edit_off`, `hide_version`) are the
  connector's alone, through the PHP master's environment (internal/phpfpm
  SPEC §5.3).

### 4.9 Maintenance page (#79)

A site may answer visitors with a maintenance page: a 503 with `Retry-After`. The settings are the
panel's (`config.maintenance`, internal/api SPEC §3.16); the vhost file holds everything, no file of
its own. Refused before anything is rendered: `site.CheckMaintenance` (title at most 100 and message
at most 2000 characters of plain text, `retry_after_sec` 0 or 60–604800, at most 50 addresses, each
an address or network in its canonical form, and a 32-hex-digit key while the mode is on).

Before the `server` blocks:

```
geo $cg_mnt_ip_<id> {
    default  0;
    127.0.0.0/8  1;   ::1/128  1;   <each of this machine's addresses>  1;   <each address of the list>  1;
}
map "$cg_mnt_ip_<id>|$cookie_cg_maint|$uri" $cg_mnt_<id> {
    default  1;
    "~^1\|"  0;                                          # an address let through
    "~^0\|<key>\|"  0;                                   # a browser holding the key
    "~^0\|[^|]*\|/\.cg-maintenance/<key>$"  0;           # the address that sets the cookie
}
```

In the 443 server, after the refused visitors' `if` (so a refused address is still 403):
`if ($cg_mnt_<id>) { return 599; }` and `error_page 599 =503 @cg_maintenance;`. 599 is no status a
backend gives, so only this `if` reaches the page; PHP's own 503 for a visitor let through is not
replaced. The named location `@cg_maintenance` has `default_type text/html`, `charset utf-8`, the
security headers (§4.3), `Retry-After` (the setting, an hour by default), `Cache-Control: no-store`,
and `return 503 '<page>'`. The server rewrite phase runs before the page cache is read, and the
policy has no cache while the mode is on (internal/velocity SPEC §1.1), so nothing of the site is
cached meanwhile.

The page is a fixed HTML document with the title and the message. Both are encoded for the quoted
string and for HTML at once: letters, digits, spaces and `.,:!?()-_/@+%` as they are, a line break
as `<br>`, every other character as a numeric reference (`${x}` shows as text, `<b>` is not run, a
quote does not end the string). Empty ones take the defaults ("Down for maintenance", "We are
making some changes. Please come back in a little while.").

`location = /.cg-maintenance/<key>` sets `cg_maint=<key>` (Secure, HttpOnly, SameSite=Lax, eight
hours) and redirects to `/`: opening it lets a browser through. The key is a bearer secret kept in
the root-readable vhost and never in the API's view of the site (`Site.Public`); port 80, which
only redirects and serves ACME challenges, is not affected.

### 4.10 Redirects and canonical host (#83)

A site may send some paths elsewhere and choose one of its names as the canonical one. The settings
are the panel's (`config.redirects`, internal/api SPEC §3.18); the vhost file holds everything.
Refused at every render: `site.CheckRedirects`.

**What is accepted.** Nothing is escaped: paths and targets are kept to characters nginx gives no
meaning (no space, quote, `;`, brace, backslash, `$` or `#`, no control character), and each is
written inside double quotes all the same.

- A rule is `match` (`exact` or `prefix`), `from`, `to` and `status` (301 or 302); at most 200 rules.
- `from` is a path of the site, at most 512 characters of letters, digits and `. _ ~ - / + , : @ ! ( ) = *`:
  no percent escape (nginx matches the decoded path, so it would never match), no empty, `.` or `..`
  segment. A prefix rule's path has no trailing slash and is not the root.
- `to` is a path (`/new`, with a `?query` for an exact rule) or an `http(s)://host[:port]/path`; at
  most 1024 characters of the same set plus `%XX` escapes and `&`, `?`; a host name `ValidateDomain`
  accepts, no credentials; never `//`, which would be another site. A rule that redirects to its own
  path (a prefix rule to something below it) is a loop and is refused, and so are two rules for one
  path, an exact and a prefix rule included (the prefix rule has the exact location too).
- The canonical host is `""` or the site's domain or one of its aliases.

**Locations**, in the 443 server after the refused visitors, the canonical host and the maintenance
page and before the document root: an exact rule is `location = <from> { return <status> "<to>$is_args$args"; }`
(the query string is kept unless `to` has its own); a prefix rule is that location for its own path
and `location ^~ <from>/ { rewrite "^<from>/(.*)$" "<to>/$1" permanent|redirect; }`, so
`/blog/a/b?x=1` goes to `/news/a/b?x=1`. The regex is the path with `regexp.QuoteMeta`; nginx gives
the rewrite's query string on its own. Exact and `^~` locations are chosen before any regex
location, so neither the static-file locations nor the routing block's regexes can answer a redirected
path. The routing block cannot have the same location (nginx would refuse the file as a duplicate):
`prepare` refuses with the location named. The rules are not in the cache: a location is not the
PHP location, which alone reads and fills the page cache.

**Canonical host.** In the 443 server, after the refused visitors' `if` and before the maintenance
page: `if ($host != "<canonical>") { return 301 https://<canonical>$request_uri; }`. `$host` is
lower case and has no port; `$request_uri` keeps the path and query as they came. Port 80 sends
every name straight to `https://<canonical>$request_uri` (one hop, ACME location untouched). The
maintenance page's key address belongs to the domain: opened on a name that is not canonical, it
goes to the canonical one first.

A staging copy has no redirects (its names are not the original's).

## 5. Page-cache policy (`velocity.PolicyFor`)

Off for static and proxy sites and when the site's cache is off. Profiles:
balanced 600s / 7200s stale, commerce 300s / 3600s, maximum 86400s / 3 days
and 4096 MB; tracking arguments ignored in all three; 32 MB zone, 1024 MB,
lock 5s; `CacheTTLSec` overrides the TTL.

### 5.1 Bypass

At server level, before the PHP location: `$cg_skip_<id>` is 1 for a POST,
for a query string that bypasses (`$cg_args_bypass`, or any query string
without `IgnoreTrackingArgs`), for a `Cookie` header matching a bypass
cookie (case-insensitive substring, regex-quoted) and for a request URI
under a bypass prefix (anchored at the start, and at the end of a path
segment unless the prefix ends in `/` or `=`: `/feed` does not catch
`/feedback`).

Each site type has its own set (R-5.2: Laravel used to get WordPress's,
so a Laravel visitor's session cookie never bypassed):

| Type | Cookies | URI prefixes |
|---|---|---|
| wordpress | `wordpress_logged_in_`, `wp-postpass_`, `comment_author_`, `woocommerce_cart_hash`, `woocommerce_items_in_cart`, `wp_woocommerce_session_` | `/wp-admin`, `/wp-login.php`, `/wp-cron.php`, `/xmlrpc.php`, `/wp-json/`, `/feed` |
| woocommerce | wordpress's | wordpress's + commerce's |
| php | wordpress's (a PHP site may well be a hand-installed WordPress) + `PHPSESSID` (PHP's own session) + the PHP frameworks' `_session=`, `CAKEPHP=`, `XSRF-TOKEN=`, `remember_web_` (R-10.5) | wordpress's |
| laravel | `_session=` (the session cookie: `<APP_NAME slug>_session`, `laravel_session` by default), `XSRF-TOKEN=`, `remember_web_` (the remember-me cookie) | `/login`, `/logout`, `/register`, `/password/`, `/forgot-password`, `/reset-password`, `/email/verify`, `/admin`, `/nova`, `/horizon`, `/telescope`, `/sanctum/`, `/livewire/`, `/broadcasting/` |

Every type with a cache also bypasses on the site's own `bypass_cookies`, each as `<name>=`
(velocity SPEC §1.4).

Commerce's prefixes (`/cart`, `/checkout`, `/my-account`, `/addons`,
`/wc-api`, `/?wc-ajax=`) are WooCommerce's: added for the woocommerce type,
and for the commerce profile on wordpress and php sites; not for Laravel.

A response that sets a cookie is never stored whatever the type (Set-Cookie
is not ignored), so a Laravel page rendered with a session is not cached;
the cookie bypass is what keeps a visitor holding a session from being
served someone else's cached page.

## 6. The gate

- `TestRenderMatrix`: every type × profile × cache switch × build (1.24,
  1.26 without QUIC, 1.28 with it), and every option (HSTS with and
  without a certificate, aliases, staging with and without key, staging
  proxy, public root, PHP timeout, body limit, TTL override and a TTL over
  a week, query-string bypass, empty bypass lists, PHP without a socket,
  HTTPS upstream, maximum profile on a store, custom routings), compared
  byte for byte with `nginx/testdata/matrix`. A changed byte is a change to
  the request path: regenerate (`-update-matrix`) only deliberately and say
  why in the commit.
- `make docker-nginx-test`: the matrix dumped (`CG_NGINX_DUMP`) and
  `nginx -t` run on all of it at once, with the installer's default servers,
  by the distro nginx of Ubuntu 24.04 (1.24) and 26.04 (1.28).

## 7. Routing (R-5.4)

A site's routing is the block of locations that decides what serves a URI.
It is the only part of the vhost a user edits; everything else (TLS,
certificate, logs, limits, the cache and its bypass, the security headers,
the PHP link, the hidden-files and static-asset locations) stays the
panel's.

### 7.0 Threat model

The block is written by an administrator of the panel, trusted with the
site: they may make it serve anything of the site's own release, proxy to
its own app, redirect anywhere. The block must not reach beyond the site:

- **other tenants**: no path outside the site's release tree, no symlink
  followed out of it (the server's `disable_symlinks` stays in force), no
  proxy to nginx itself (another site by Host), no file test or listing
  outside the public part;
- **system services**: no proxy to a port a system service listens on, on
  this machine or a private network, nor to a Unix socket (the panel's API,
  the agent, every site's PHP), nor through a raw-protocol `*_pass`;
- **the panel-managed parts**: TLS, logs, the cache and its bypass
  (`$cg_*`), the staging password, the security headers, the PHP link,
  limits and measured tuning cannot be overridden or switched off from it;
- **the machine**: nothing nginx reads or loads as root at start-up
  (`include`, modules, certificates, databases), no scripting module.

An administrator's mistakes are their site's: a routing that breaks the
site is refused by `nginx -t` or can be restored to the default.

What is judged must be what nginx later uses (R-10.5): a proxy target is
an address, never a name, since nginx resolves names again at every reload
(DNS rebinding: a name checked public at render could later lead to
loopback); and the ports reserved are judged per site, so another tenant's
listener is out of reach like a system service's. Writing the resolved
address into the vhost instead of the name was considered and not done:
TLS to the upstream would then lose its name (SNI, certificate checks), and
an address the administrator types is just as explicit.

### 7.1 Storage and rendering

`Site.Config.Routing` holds the block as the user wrote it; empty is the
type's default (§4), so "Restore default" is storing "". The default is
rendered from the same text a user starts editing from (`DefaultRouting`),
so the matrix is unchanged. The block is rendered where the default is,
after one empty line, each line indented four spaces except lines that
continue a quoted string, and always followed by a line break (a trailing
comment cannot swallow the next line). It is re-validated at every render:
a stored block that no longer passes is refused, not rendered. Being in
the vhost, it is in the managed file's hash, so drift covers it.

Lines `\r\n` are stored as `\n`; any other control character than tab
and line feed, or more than 32 KiB, or blocks nested deeper than 8, are
refused.

### 7.2 Tokenizer

The block is read the way nginx reads its configuration
(`ngx_conf_read_token`), so the directives checked are the directives nginx
will see:

- whitespace (space, tab, CR, LF) separates words; `;` ends a directive,
  `{` opens a block, `}` at the start of a word closes one;
- `#` at the start of a word comments to the end of the line;
- `"…"` and `'…'` are quoted words; after the closing quote the next
  character must be whitespace, `;`, `{` or `)`;
- outside quotes, `\` makes the next character part of the word;
- `${` inside a word does not open a block (a variable in braces), and `}`
  inside a word does not close one;
- inside a word `\"`, `\'` and `\\` stand for the character, `\t`, `\r`,
  `\n` for tab, CR and LF;
- the directive name is a word like any other: `"include"` is `include`.

The text must end outside any quote, comment-free of an open directive
(every directive ended by `;` or a block), with every block closed, and with
no `}` closing a block it did not open.

### 7.3 Rules

Every directive anywhere in the block, at any depth, is checked by name
(after unquoting, lower case) and arguments, in two passes over the same
tree: the **allowlist** below, then the **denylist** (defence in depth: a
name later added to the allowlist by mistake still meets it). A refusal
names the line.

#### Allowlist

| Directive | Rule |
|---|---|
| `location` (block) | a path, or `=` / `~` / `~*` / `^~` and a path; named `@x` locations |
| `if` (block) | `($var)`, `($var = x)`, `!=`, `~`, `~*`, `!~`, `!~*` on a variable, or a file test (`-f -d -e -x`, `!`) of `$request_filename` only: testing any other path tells what exists outside the site |
| `try_files` | files and a fallback that is a path, a named location or `=<code>` |
| `rewrite` | regex, replacement, flag `last` / `break` / `redirect` / `permanent` |
| `return` | a code (100–599) and a text or URL, or a URL |
| `error_page` | error codes (300–599), optionally `=` or `=<code>`, and a path of this site or a named location (never another host) |
| `root`, `alias` | the release tree (below) |
| `autoindex` | `off` only (R-10.5): nginx's listing `stat`s each entry through symlinks (`ngx_de_info` is `stat(2)` on Linux; `disable_symlinks` covers the files a request opens, not a listing), so a link the tenant planted showed the size and date of another site's file. Until R-10.5, `on` was allowed in the public directory |
| `index`, `default_type`, `expires`, `add_header` (§7.4), `charset`, `internal` | — |
| `types` (block) | media types and their extensions only |
| `set` | a variable not named `cg_*` |
| `break`, `allow`, `deny`, `limit_except` (block) | added to the review's list: the rewrite module's companion of `rewrite … break`, and read-only access control |
| `proxy_pass` | the target rule (below) |
| `proxy_set_header` | towards this machine, `Host` only `$host` or one of the site's names |
| `proxy_http_version`, `proxy_read_timeout`, `proxy_connect_timeout`, `proxy_send_timeout`, `proxy_buffering`, `proxy_redirect`, `proxy_hide_header` | `proxy_hide_header` added: it only removes an upstream header |

Not on it, among others: every other `*_pass` (`fastcgi_pass` is the
panel's PHP link; `grpc_pass`, `uwsgi_pass`, `scgi_pass`, `memcached_pass`
reach raw protocols), caching, logging, TLS, tuning, scripting, `include`.

**Proxy targets.** `http://` or `https://` with an **IP address** (v4, or
v6 in brackets) or `localhost` (`*.localhost`), taken as 127.0.0.1; no
variable, no user, no `unix:`. A host name is refused (R-10.5): it was
checked at render, but nginx resolves it again at every start and reload,
when it may point at this machine (a system port, another tenant by Host),
and the Host-header rule below never ran for it. A target on this machine or a private
network — `localhost`, 127.0.0.0/8, 0.0.0.0/8, ::1, ::, 10/8, 172.16/12,
192.168/16, 169.254/16, 100.64/10, fc00::/7, fe80::/10, or one of this
machine's own addresses — may not use a system service's port: the fixed
ones (80, 443, 8443: nginx itself, so any other tenant by Host; 3306
MariaDB; 6379 Redis; 5252 the panel's API; 9080 its local listener, the
connector's) and every TCP port in LISTEN whose owner is not **this
site's** user (its shell login has the same uid), read from
`/proc/net/tcp{,6}` at every render — a service started later is covered at
the next render, and the render fails rather than reach it. Another site's
listener (its app server, a process in its shell) is another tenant's and
as reserved as a system service's (R-10.5: before, any `cg-site-*` or
`cg-shell-*` owner was exempt, so site A could proxy to site B's app). An
owner that cannot be looked up reserves every listening port. A site's own
app (a Node server run by its user on 3000) is reachable.

**The release tree.** `root` and `alias` take one clean absolute path,
without variables, in `<root>/current` or `<root>/releases/<name>`, or
beneath, with no hidden segment (`.git`, `.ssh`…) below. Never the site
root itself, `shared/`, `logs/`, `tmp/`, `src/`, `.ssh/`. An `alias` ending
in `/` needs a prefix location ending in `/` (else `/x../` leaves the
directory). The server-level `disable_symlinks if_not_owner
from=…` (§4) is not editable and is inherited by every location of
the block, whatever root or alias it sets.

#### Denylist (second pass)

Refused names:

| Why | Names |
|---|---|
| reads or loads files as root at start-up | `include`, `load_module`, every `ssl_*`, `proxy_ssl_*`, `grpc_ssl_*`, `uwsgi_ssl_*`, `xslt_*`, `geoip*` (any name containing `ssl_`, or starting `geoip` or `xslt`) |
| logging (files written as root) | every name ending in `_log`, `log_format`, `log_subrequest`, `open_log_file_cache` |
| server identity and sockets | `server`, `listen`, `server_name`, `http`, `stream`, `mail`, `events`, `upstream`, `map`, `geo`, `split_clients`, `http2`, `http3`, `quic_*`, `user`, `env`, `daemon`, `pid` |
| staging password | `auth_basic`, `auth_basic_user_file`, `auth_request`, `auth_request_set`, `satisfy`, every `auth_*` |
| security headers and server tokens | `more_set_headers`, `more_clear_headers`, `more_set_input_headers`, `more_clear_input_headers`, `add_header_inherit`, `server_tokens`, `disable_symlinks`, `merge_slashes` |
| the PHP-FPM link and the page cache | every `fastcgi_*`; every `proxy_*`, `uwsgi_*` or `scgi_*` name containing `cache` or `store`; every `*_temp_path`; `client_body_in_file_only` |
| limits | `client_max_body_size`, `client_body_timeout`, `client_header_timeout`, `client_body_buffer_size`, `large_client_header_buffers` |
| measured tuning (AGENTS.md rules 5–6) | `tcp_nodelay`, `tcp_nopush`, `sendfile*`, `aio*`, `directio*`, `output_buffers`, `postpone_output`, `open_file_cache*`, `gzip*`, `gunzip*`, `brotli*`, `zstd*`, `keepalive_*`, `reset_timedout_connection`, `lingering_*` |
| scripting modules | names starting `perl`, `js_`, `njs`, `lua`, `mruby`, `wasm`, `python`, any name containing `_by_` (`content_by_lua_block`, `set_by_*`…) |
| server-wide state, writes to the tree | `stub_status`, `status`, `api`, `vhost_traffic_status*`, `dav_methods`, `dav_access`, `dav_ext_*`, `create_full_put_path`, `min_delete_depth`, `ssi*`, `post_action` |

Arguments:

- **`*_pass`** (`proxy_pass`, `grpc_pass`, `uwsgi_pass`, `scgi_pass`,
  `memcached_pass`): no `unix:` target and no variable (a variable target
  is parsed at request time, `unix:` included);
- **`root`, `alias`**: one argument, absolute, clean, without variables,
  the site's root or under it;
- **paths**: in `root`, `alias`, `try_files`, `index`, `error_page`,
  `rewrite` (its replacement), `mirror`, `add_before_body`,
  `add_after_body`, the part before `?` may use only `$uri`,
  `$document_uri`, `$document_root`, `$realpath_root` (`$is_args` starts the
  query) and, in `rewrite`'s replacement only, its own captures (`$1`…`$9`):
  everything else (`$arg_*`, `$args`, `$http_*`, `$cookie_*`,
  `$request_uri`, a `set` variable, a capture of an earlier `if`) is the
  client's, unnormalised, and could walk out of the tree with `..`;
- **no `..` path segment** in any word;
- **the panel's variables**: `set` of a variable named `cg_*`, and a regex
  capture named `cg_*` (`(?<cg_…>`, `(?P<cg_…>`, `(?'cg_…'`), anywhere: the
  cache bypass and keys are `$cg_*`.

What passes both is nginx's to judge: a directive in the wrong context or a
bad value fails `nginx -t` (§7.5).

### 7.4 Security headers in custom blocks

A `location` or `if` block of the custom routing whose own directives include
`add_header` gets the security headers (§4.3) added before its closing
brace, so the headers it declares do not silently drop the panel's.

### 7.5 Saving (agent `SetSiteRouting`, `SiteRouting`)

- `SiteRouting` returns the default block, the stored one, and the
  rendered vhost around the block (`Before`, `After`), read-only in the UI.
- `SetSiteRouting` validates (§7.2–7.3: refused with the rule's message),
  then renders the site with the candidate block through the normal render
  (ops/sites SPEC §4, only if changed): files written, `nginx -t`, reload.
  When `nginx -t` fails every file goes back and nginx is reloaded on the
  old ones: the site keeps serving as before, and the error carries nginx's
  message. When `nginx -t` passes but the reload fails, the same: the
  files go back and the error is returned. The panel stores the block only
  once the agent has applied it.
- The default shown and compared with is the type's, read without the
  stored block, so a stored block that no longer passes (a service took its
  port) can still be replaced or restored.
- Two saves of one site are serialised: the panel holds the site's lock
  from reading the site to storing the result, and the agent's row takes
  `LockLiveSite`; the second save sees the first's block.

### 7.6 The panel (`internal/api/routing.go`) and the tab

- `GET /api/sites/{id}/routing`, `PUT /api/sites/{id}/routing` are
  **admin, session only** rows (an API token, an admin's included, gets
  403; an operator of the site gets 403). PUT takes `{"routing": "…"}`
  (≤ 65 KiB of JSON); `""` restores the default. The panel checks the rules
  itself first (a refusal never reaches the agent), then calls
  `SetSiteRouting` under the site's lock with the site as stored now, and
  stores what the agent returns (`""` when the text was the default) with
  `MutateSiteApplied`, recording the rendered files' hashes for drift. A
  refusal stores nothing. Each change is audited as `site.routing`
  (`custom` / `default`).
- A staging copy takes its production's routing with the `root` and
  `alias` paths in the production's tree moved to its own
  (`RebaseRouting`: read with the tokenizer, only those arguments
  rewritten; the old name elsewhere, in a comment, a header or a longer
  word, is left; a block that does not parse is copied as it is and
  refused at the copy's render).
- The UI's "Vhost" tab (admins only; not in the create dialog) shows the
  panel's part of the vhost read-only above and below the editor, the block
  in a code editor, Save and "Restore default"; a refusal is shown with its
  line and the site keeps serving.
