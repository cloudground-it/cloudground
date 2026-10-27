# CloudGround HTTP API

<!-- Generated from internal/api/routes.go and internal/api/apidoc.md.tmpl by `go generate ./internal/api`. Do not edit by hand. -->

Everything the panel does goes through this HTTP API; the web interface uses it like any other
client. The API listens on the Unix socket `/run/cloudground/api.sock`, and nginx carries outside
requests to it. On the server, root can call it directly with
`curl --unix-socket /run/cloudground/api.sock http://localhost/api/...`. A request that changes
something is sent as `application/json`, and from a browser only from the panel's own origin.

## Signing in

A session starts with `POST /api/login`, whose body holds the account's email and password; the
answer sets a cookie, which every later request sends back:

```sh
# sign in once, keeping the cookie in ./cookies
curl -sk --cookie-jar cookies https://panel.example.com/api/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"admin@example.com","password":"…"}'
# then any call
curl -sk --cookie cookies https://panel.example.com/api/sites
```

The cookie is `HttpOnly`, `Secure` and `SameSite=Strict`. Its value is a random token that the
panel looks up in its database, so a session can be ended from the server side; it expires after
24 hours.

An account with two-factor authentication gets `401` and `{"totp_required": true}` at first: send
the same request again with `"totp"` set to the six-digit code. A code is accepted only once.

### API tokens

Scripts and `cgctl` authenticate with an API token instead: `Authorization: Bearer cgt_…`. An admin
creates one from a signed-in session (`POST /api/tokens`); the secret is in that response only, and
the panel keeps just its SHA-256. A token acts as the admin who made it, only while that account is
an enabled admin, and under the same two-factor requirement. A request with a `Bearer cgt_…` token
is judged by it alone, even when it also carries a session cookie: a bad or revoked token is `401`.
Any other `Authorization` header (a proxy's Basic auth, another system's bearer token) is ignored.

A token is for automating sites, not for governing the panel. It never reaches the routes marked
**session only** below, which answer it `403`: `/api/logout`, every `/api/account/*` route,
creating, changing, deleting and resetting the password of users, every `/api/tokens` route, a
site's shell and routing, the firewall rules, the panel's certificate and its self-update. It may
restore drifted files but not accept drift (`403`). Of the
settings (`PUT /api/settings`) it may change only `acme_email` and `probe_target`; any other key it
changes is `403` (one sent back exactly as `GET` showed it passes, so a script can send back the
map it read). So a leaked token cannot mint another token, create an admin, reset a password,
turn the 2FA requirement off, send the backups or the alerts somewhere else, or replace the panel. Changing or resetting a password, turning 2FA off, disabling, demoting or
deleting an account, and `panel-api recover-admin` revoke the account's tokens along with its
sessions.

Creating and revoking tokens are audited, and so is every audited action made with one, with the
token named in the actor. `GET /api/events` accepts a token like every other route.

```sh
curl -sk --cookie cookies https://panel.example.com/api/tokens -H 'Content-Type: application/json' \
  -d '{"name":"deploy script","expires_in_days":90}'
curl -sk https://panel.example.com/api/sites -H "Authorization: Bearer $CLOUDGROUND_TOKEN"
```

## Who may call what

The endpoint tables give each route an access marker. Without a valid session or token the answer
is `401`; with one whose role the marker does not allow, `403`.

| Marker | Who may call |
| :-- | :-- |
| **open** | anyone; no session needed |
| **site token** | the site's connector token, or the signature of a git webhook, in place of a session |
| **auth** | every signed-in role |
| **global** | `admin` and `readonly`; server-wide data is closed to `operator` |
| **manage** | `admin`, or an `operator` on a site assigned to it |
| **admin** | `admin` alone |
| **dbstudio** | a Quarry session (see Quarry below), never a panel session or an API token |
| **dbstudio data** | a Quarry session that may read the rows: a database user's, or one opened from the panel by an `admin` or `operator` |
| **mcp** |  |

An `operator` sees and touches only assigned sites (and their staging copies); requests for any
other site, or for another site's backups, releases or cron jobs, return `404`, not `403`, so the
API does not confirm that they exist.

When **Require 2FA for admins** is on, an `admin` account without two-factor authentication gets
`403` on every endpoint except signing out and the two enrolment steps
(`POST /api/account/2fa/begin` and `/confirm`), until it enrols.

## Requests and answers

- Bodies are JSON both ways.
- Work that takes time answers **`202`** and a task: follow it with `GET /api/tasks/{id}`, or read
  the resource again.
- A failure is a status code with `{"error":"..."}` explaining it.
- Unknown fields are rejected. Request bodies are capped at 4 MiB, except:
  - `POST /api/setup`: 8 KiB
  - `POST /api/login`: 8 KiB
  - `POST /api/login/passkey/begin`: 8 KiB
  - `POST /api/login/passkey`: 8 KiB
  - `POST /api/account/password`: 8 KiB
  - `POST /api/account/2fa/begin`: 8 KiB
  - `POST /api/account/2fa/disable`: 8 KiB
  - `POST /api/account/passkeys/begin`: 8 KiB
  - `POST /api/account/passkeys/finish`: 64 KiB
  - `POST /api/account/passkeys/challenge`: 8 KiB
  - `POST /api/account/passkeys/{id}/remove`: 8 KiB
  - `PUT /api/account/password-signin`: 8 KiB
  - `POST /api/account/recovery-codes`: 8 KiB
  - `PUT /api/account/profile`: 8 KiB
  - `POST /api/account/whats-new`: 8 KiB
  - `POST /api/users`: 8 KiB
  - `POST /api/users/{id}/password`: 8 KiB
  - `POST /oauth/register`: 64 KiB
  - `POST /oauth/token`: 8 KiB
  - `POST /oauth/revoke`: 8 KiB
  - `POST /mcp`: 64 KiB
  - `POST /api/mcp/requests/{id}/approve`: 8 KiB
  - `POST /api/mcp/requests/{id}/deny`: 8 KiB
  - `POST /api/mcp/actions/{id}/confirm`: 8 KiB
  - `POST /api/mcp/actions/{id}/reject`: 8 KiB
  - `POST /api/tokens`: 8 KiB
  - `POST /api/sites/{id}/protected-paths`: 8 KiB
  - `DELETE /api/sites/{id}/protected-paths`: 8 KiB
  - `PUT /api/sites/{id}/blocking`: 64 KiB
  - `PUT /api/sites/{id}/dev-mode`: 8 KiB
  - `PUT /api/sites/{id}/maintenance`: 8 KiB
  - `PUT /api/sites/{id}/routing`: 65 KiB
  - `PUT /api/sites/{id}/certificate/custom`: 256 KiB
  - `PUT /api/sites/{id}/wp/cleanup`: 8 KiB
  - `POST /api/sites/{id}/migration/pull/estimate`: 64 KiB
  - `POST /api/sites/{id}/migration/pull`: 64 KiB
  - `POST /api/db-servers`: 256 KiB
  - `PUT /api/db-servers/{id}`: 256 KiB
  - `PUT /api/sites/{id}/git-deploy`: 8 KiB
  - `POST /hooks/git/{hook}`: 1 MiB
  - `POST /api/backups/{id}/archive`: 8 KiB
  - `PUT /api/sites/{id}/uptime`: 8 KiB
  - `PUT /api/sites/{id}/mail`: 8 KiB
  - `POST /api/sites/{id}/mail/test`: 8 KiB
  - `PUT /api/sites/{id}/backups/policy`: 8 KiB
  - `POST /api/sites/{id}/files/upload`: 16 MiB
  - `POST /api/quarry/login`: 8 KiB
  - `POST /api/sites/{id}/quarry/handoff`: 8 KiB
  - `POST /api/quarry/handoff`: 8 KiB
  - `POST /api/quarry/db/{db}/tables/{table}/edit`: 16 MiB
  - `POST /api/quarry/import/upload`: 8 MiB
  - `PUT /api/sites/{id}/wp/updates`: 8 KiB
  - `PUT /api/sites/{id}/wp/hardening`: 8 KiB
  - `PUT /api/sites/{id}/wp/two-factor`: 8 KiB
  - `POST /api/system/updates/apply`: 8 KiB
  - `POST /api/blocklist/blocks`: 8 KiB
  - `DELETE /api/blocklist/blocks`: 8 KiB
  - `POST /api/blocklist/allowed`: 8 KiB
  - `DELETE /api/blocklist/allowed`: 8 KiB
  - `PUT /api/blocklist/settings`: 8 KiB
  - `POST /api/settings/mail/test`: 8 KiB
- The endpoints that run a database statement while the request waits take at most
  2 at once per account and 4 per site; beyond, **`429`**
  `too many database statements are running: wait for one to finish`:
  - `POST /api/sites/{id}/database/query`
  - `POST /api/quarry/db/{db}/tables/{table}/rows`
  - `POST /api/quarry/db/{db}/tables/{table}/edit`
  - `POST /api/quarry/db/{db}/tables/{table}/alter`
  - `POST /api/quarry/db/{db}/query`
  - `POST /api/quarry/db/{db}/simulate`
  - `POST /api/quarry/db/{db}/search`
- A browser request that changes something must come from the panel's own origin (`Sec-Fetch-Site`
  and `Origin` are checked when sent), and its body must be `application/json`; the two upload
  endpoints also take the raw file as `application/octet-stream`. Otherwise the answer is `403` or
  `415`. Clients that send neither header, such as `curl` and `cgctl`, are unaffected.

## Endpoints

### First setup and sessions

| Method | Path | Access | What it does |
| :-- | :-- | :-- | :-- |
| `GET` | `/api/bootstrap` | open | has the first admin been created yet |
| `POST` | `/api/setup` | open | create the first admin with the one-time setup token |
| `POST` | `/api/login` | open | start a session: `email`, `password`, and one second factor when the account has two-factor (`totp`, `recovery_code` or `passkey`, an assertion); a refusal for the second factor lists the account's `factors` and carries `passkey` request options |
| `POST` | `/api/login/passkey/begin` | open | start signing in with a passkey alone: WebAuthn request options |
| `POST` | `/api/login/passkey` | open | sign in with a passkey alone (`credential`, an assertion with user verification) and start a session |
| `POST` | `/api/logout` | auth, session only | sign out and delete the session |
| `GET` | `/api/session` | open | the signed-in account, or `{"user": null}` |
| `GET` | `/healthz` | open | liveness: `200` when the agent answers, `503` when it does not |
| any | `/` | open | any other path: the web interface |

### Your account

| Method | Path | Access | What it does |
| :-- | :-- | :-- | :-- |
| `POST` | `/api/account/password` | auth, session only | change your own password |
| `POST` | `/api/account/2fa/begin` | auth, session only | start TOTP enrolment (a proof: your password, and a current factor when you have two-factor); returns the secret |
| `POST` | `/api/account/2fa/confirm` | auth, session only | confirm with a code and switch 2FA on |
| `POST` | `/api/account/2fa/disable` | auth, session only | turn TOTP off (a proof: your password and a current factor) |
| `GET` | `/api/account/passkeys` | auth, session only | your passkeys |
| `POST` | `/api/account/passkeys/begin` | auth, session only | start adding a passkey (a proof); returns WebAuthn creation options |
| `POST` | `/api/account/passkeys/finish` | auth, session only | store the passkey (`name`, `credential`); with your first factor, returns your recovery codes |
| `POST` | `/api/account/passkeys/challenge` | auth, session only | WebAuthn request options for confirming a change with one of your passkeys |
| `POST` | `/api/account/passkeys/{id}/remove` | auth, session only | remove one of your passkeys (a proof) |
| `PUT` | `/api/account/password-signin` | auth, session only | turn your password sign-in on or off (`enabled` and a proof; off takes a verified passkey) |
| `POST` | `/api/account/recovery-codes` | auth, session only | new recovery codes in place of the old ones (a proof) |
| `GET` | `/api/account/sessions` | auth, session only | your active sessions |
| `DELETE` | `/api/account/sessions/{id}` | auth, session only | revoke one |
| `PUT` | `/api/account/profile` | auth, session only | your first and last name and interface language |
| `GET` | `/api/changelog` | auth | what changed in each release, from the CHANGELOG.md inside the panel: `current` (the running release), `seen` (the release your account last saw), `unseen` (the versions to tell you about, newest first) and `entries` (`version`, `body` in Markdown), newest first |
| `POST` | `/api/account/whats-new` | auth, session only | mark the running release's changelog as seen by your account |

### Accounts

| Method | Path | Access | What it does |
| :-- | :-- | :-- | :-- |
| `GET` | `/api/users` | global | every account |
| `POST` | `/api/users` | admin, session only | create one — `role` is `admin`, `operator` or `readonly`; an operator needs `site_ids` |
| `PUT` | `/api/users/{id}` | admin, session only | change an account's role, sites or name, or disable it |
| `POST` | `/api/users/{id}/password` | admin, session only | set a new password for another account (turns its password sign-in back on) |
| `POST` | `/api/users/{id}/2fa/reset` | admin, session only | remove another account's second factors (TOTP, passkeys, recovery codes) when they are lost |
| `DELETE` | `/api/users/{id}` | admin, session only | delete an account |

### Tokens

| Method | Path | Access | What it does |
| :-- | :-- | :-- | :-- |
| `GET` | `/api/tokens` | admin, session only | every token: name, owner, hint, created, expires, last used — never the secret |
| `POST` | `/api/tokens` | admin, session only | create one: `name` (1–64 characters), `expires_in_days` (0 = never, at most 365); answers `201` with the secret in `token`, shown once |
| `DELETE` | `/api/tokens/{id}` | admin, session only | revoke it; it is refused from the next request |

### Sites and their settings

| Method | Path | Access | What it does |
| :-- | :-- | :-- | :-- |
| `GET` | `/api/sites` | auth | the sites the caller may see |
| `POST` | `/api/sites` | admin | create (the database name, user and password may be chosen) — returns `202`, a task and the summary, the only answer with the database password |
| `GET` | `/api/sites/dns-check?domain=` | manage | whether a host name already points at this server (the certificate's readiness): `status` `here`, `partial`, `elsewhere`, `proxied` (a CDN hides the origin), `unresolved` or `unknown`, its A and AAAA `addresses` (a private one unnamed) and the server's public addresses; a lookup only, 3 s at most, 30 a minute per account |
| `GET` | `/api/sites/{id}` | auth | a site and its settings |
| `DELETE` | `/api/sites/{id}` | admin | delete — returns `202` |
| `PATCH` | `/api/sites/{id}` | manage | change any of the site's settings in one request (fields below) — returns `202`, the site and one task |
| `POST` | `/api/sites/{id}/protected-paths` | manage | protect a path with a username and password, or change them (`{"path": "/wp-admin", "username": …, "password": …}`); answers the site's protected paths |
| `DELETE` | `/api/sites/{id}/protected-paths` | manage | stop protecting a path (`{"path": "/wp-admin"}`); answers the site's protected paths |
| `PUT` | `/api/sites/{id}/blocking` | manage | the visitors the site refuses with 403 (`{"addresses": ["203.0.113.0/24"], "user_agents": ["GPTBot"], "countries": ["CN"], "allow_countries_only": false}`; empty lists refuse nobody); live at once, answers the rules |
| `PUT` | `/api/sites/{id}/dev-mode` | manage | development mode: the page cache is bypassed for `{"minutes": 15}`, 30 or 60 (again while it runs: the time starts over), then switched back on by the panel, which empties the cache and warms it again; `0` ends it now; answers `dev_mode_until` (null when off); 409 on a site with no page cache |
| `GET` | `/api/sites/{id}/maintenance` | manage | the maintenance page: `enabled`, `title`, `message`, `retry_after_sec`, `allow` and, while it is on, the `preview_url` that lets a browser through |
| `PUT` | `/api/sites/{id}/maintenance` | manage | change the maintenance page (a field left out keeps its value; `{"enabled": true, "title": …, "message": …, "retry_after_sec": 3600, "allow": ["203.0.113.0/24"], "allow_my_address": false}`): on, visitors get a 503 with Retry-After and the page cache is bypassed; the addresses in `allow`, this server and a browser that opened `preview_url` still see the site; live at once, answers the settings |
| `GET` | `/api/sites/{id}/routing` | admin, session only | the site's routing block (`routing`, `default`, `custom`, `editable`) and the panel's vhost around it (`before`, `after`) |
| `PUT` | `/api/sites/{id}/routing` | admin, session only | replace the routing block (`{"routing": "…"}`, `""` restores the default): checked, then live only if `nginx -t` passes |
| `GET` | `/api/sites/{id}/certificate` | auth | whether the site has a certificate, its issuer and expiry, and the last renewal error |
| `POST` | `/api/sites/{id}/certificate` | manage | request a Let's Encrypt certificate now |
| `PUT` | `/api/sites/{id}/certificate/custom` | manage | install your own certificate: `{fullchain, key}` in PEM |
| `DELETE` | `/api/sites/{id}/certificate/custom` | manage | remove it and go back to Let's Encrypt: the certificate for the site's current names is requested (`{status, task}`; `{status, warning}` without an ACME e-mail) |
| `POST` | `/api/sites/{id}/purge` | manage | empty the site's page cache |
| `POST` | `/api/sites/{id}/warm` | manage | fill the page cache from the sitemap |
| `GET` | `/api/sites/{id}/wp/cleanup` | auth | the scheduled database cleanup of a WordPress site: `enabled`, `keep_revisions`, `last_run`, `next_run` and whether `backups_ready` |
| `PUT` | `/api/sites/{id}/wp/cleanup` | manage | turn the weekly cleanup (expired transients deleted, revisions trimmed to `keep_revisions` per post, 1–100) on or off (`{"enabled": true, "keep_revisions": 5}`); 412 turning it on without backups configured |
| `POST` | `/api/sites/{id}/wp/cleanup/preview` | manage | a dry run: how many expired transients and surplus revisions a cleanup would delete, and the autoloaded options; changes nothing |
| `POST` | `/api/sites/{id}/wp/cleanup/run` | manage | clean the database now (a dry run is logged first, a tested snapshot is taken before a site's first cleanup) — returns `202` and a task; 412 without backups configured |
| `GET` | `/api/sites/{id}/cache-stats` | auth | APCu hits, misses and memory in use |
| `GET` | `/api/sites/{id}/logs?source=` | auth | tail one of the site's own logs: `access`, `error`, `php` or `cleanup` |
| `POST` | `/api/sites/{id}/migration` | manage | bring in a site from a files archive and a SQL dump |
| `POST` | `/api/sites/{id}/migration/pull/estimate` | admin | dry run of a pull from another host over SSH/SFTP: files, bytes, WordPress or not, the host's key, whether it fits |
| `POST` | `/api/sites/{id}/migration/pull` | admin | pull a site from another host over SSH/SFTP (a password or key, single-use) and import it behind a safety snapshot |
| `GET` | `/api/sites/{id}/usage` | auth | the site's resource usage for `range` `24h` (five-minute samples) or `30d` (hourly averages, with the hour's CPU peak): CPU as a share of its allowance, memory, IO and disk; its effective `limits`; the `latest` sample |
| `GET` | `/api/sites/{id}/vulnerabilities` | auth | a WordPress site's last vulnerability check: `enabled` (the panel's switch), `checked`, `checked_at`, `attempted_at`, `error`, `core`, `components` and the `findings` (`component`, `slug`, `version`, `title`, `severity`, `cve`, `fixed_in`, `unfixed`, `link`); `source` names the database |
| `POST` | `/api/sites/{id}/vulnerabilities/check` | manage | check a WordPress site's core, plugins and themes against the vulnerability database now; 202 with the task |
| `GET` | `/api/sites/{id}/malware` | auth | the site's malware scans: `enabled` (the panel's switch), `scanned`, `next_scan`, the last `scan` (`scanned_at`, `attempted_at`, `error`, `files`, `signatures`, `duration_ms`, `engine`, `database`) and the `findings` (`id`, `path`, `signature`, `status` open, quarantined or gone, `first_seen`, `last_seen`, `quarantined_at`) |
| `POST` | `/api/sites/{id}/malware/scan` | manage | scan the site's files for malware now (needs `malware_scan` on); 202 with the task |
| `POST` | `/api/sites/{id}/malware/findings/{finding}/quarantine` | manage | move an open finding's file out of the site into the agent's quarantine; 200 with the finding |
| `GET` | `/api/sites/{id}/uptime` | auth | the site's uptime check: `enabled`, `path`, `url`, `interval_seconds` and the `last` result (`up`, `status`, `latency_ms`, `error`, `down_since`) |
| `PUT` | `/api/sites/{id}/uptime` | manage | turn the uptime check on or off (`enabled`) and choose the path it requests (`path`, default `/`) |
| `GET` | `/api/sites/{id}/mail` | auth | how the site's mail leaves: `off`, `from`, `dkim_selector`, its own `relay` (no password) or null, the `effective` relay (`site`, `server`, `none`) and the `sender` |
| `PUT` | `/api/sites/{id}/mail` | admin | change any of `off`, `from`, `dkim_selector`, `relay` (`{host, port, security, username, from, password}`, null for the server's); answers as GET |
| `GET` | `/api/sites/{id}/mail/checks` | auth | the sender domain's SPF, DMARC and DKIM records, as DNS has them |
| `POST` | `/api/sites/{id}/mail/test` | manage | send a test message through the site's relay: `{"to"}` |

`PATCH /api/sites/{id}` takes any of these fields; only the fields sent change, and one task applies
them all:

| Field | Applies to | Valid values |
| --- | --- | --- |
| `aliases` | production sites (admin only) | host names, at most 50, none used by another site; `[]` removes them all |
| `profile` | every site | `balanced`, `commerce`, `maximum` |
| `cache_enabled` | every site | `true` / `false` |
| `cache_ttl_sec` | every site | 0 (the profile's default) to 31536000 |
| `bypass_cookies` | sites with a page cache | cookie names (letters, digits, `.`, `_`, `-`, at most 64 each), at most 20, that skip the page cache beyond the site type's own; `[]` clears them |
| `php_workers` | every site | 0 (sized automatically) to 64 |
| `hsts` | every site | `true` needs a certificate valid for at least 14 more days (else `409`) |
| `php_version` | PHP sites | a version installed on the server |
| `memory_limit_mb` | PHP sites | 64–4096 |
| `upload_max_mb` | PHP sites | 1–2048 |
| `max_execution_seconds` | PHP sites | 10–600 |
| `public_root` | every type but `proxy` | a folder inside the release such as `public`; empty for the release itself |
| `proxy_upstream` | `proxy` sites (admin only) | an `http(s)://host[:port][/path]` URL |
| `object_cache` | WordPress and WooCommerce | `true` / `false` |

A refused field answers `400` with the first problem in `error` and every one in `fields`
(`{"error": "...", "fields": {"memory_limit_mb": "..."}}`); an admin-only field from an operator
answers `403`. When `aliases` changes on a site with a Let's Encrypt certificate, the certificate is
reissued for the new names after the change is applied (a separate task; if it fails the site keeps
its old certificate). A custom certificate is not touched: the answer carries
`"warnings": ["this site serves a custom certificate, which may not cover the new aliases: …"]`.
A `public_root` that passes through a symbolic link is refused. PHP workers whose memory limits add
up to more than the site's memory budget are refused with the budget in the message; an
administrator may send `"allow_over_budget": true`.

```sh
curl -sk --cookie cookies -X PATCH https://panel.example.com/api/sites/7 \
  -H 'Content-Type: application/json' \
  -d '{"php_version":"8.4","memory_limit_mb":512,"cache_ttl_sec":600}'
```

To bring a site in, `POST /api/sites/{id}/migration` gets `archive`, `sql`, `old_domain` and
`confirm`. The archive and the SQL dump must already be in the receiving site's folder (send them
over SFTP), and both paths are relative to it. Every occurrence of `old_domain` in the imported
database is replaced with the site's domain, and the request is refused unless `confirm` repeats
that domain. With a backup repository configured, the job first takes a `pre-migration` safety
snapshot of the site and restore-tests it, and names it in the task: restoring it puts back the
site as it was before the import. Without one, the task says that the import cannot be undone.
An administrator may add `"max_ratio": <1–1000>` to raise the archive's expansion limit (200 times
its compressed size by default); anyone else sending it gets 403. A refused archive's task error
says the limit and how to raise it. A task error that carries a translation key (`error_key`,
`error_args` in the task) is written in the reader's language when the task is read
(`Accept-Language`, else the user's saved language).

A new WooCommerce store, with its object cache:

```sh
curl -sk --cookie cookies https://panel.example.com/api/sites \
  -H 'Content-Type: application/json' \
  -d '{"domain":"shop.example.com","type":"woocommerce","profile":"commerce","object_cache":true,
       "admin_email":"owner@example.com","admin_user":"owner","admin_password":"…"}'
```

### Releases, staging and Safe Push

| Method | Path | Access | What it does |
| :-- | :-- | :-- | :-- |
| `GET` | `/api/sites/{id}/deployments` | auth | past releases and the guard's verdict on each |
| `POST` | `/api/sites/{id}/deployments` | manage | create a release from `source_dir`, which must be inside the site's own directory |
| `POST` | `/api/deployments/{id}/promote` | manage | promote a built release (`created`, `superseded`, `rolled_back` or the live one); 409 for any other status |
| `POST` | `/api/sites/{id}/rollback` | manage | roll back to the release that was live before the current one, or to `deployment_id` (one that was live); 409 when there is none |
| `GET` | `/api/sites/{id}/git-deploy` | manage | the site's git deploy: `enabled`, `repo`, `branch`, `build_command`, the webhook's `webhook_path` and `secret`, the deploy key's `public_key` and the latest `deliveries` |
| `PUT` | `/api/sites/{id}/git-deploy` | manage | turn git deploy on or off and set the repository (`https://`, `ssh://`, `user@host:path` or a `file://` path inside the site), the `branch` (default `main`) and an optional `build_command` run as the site user; the first time on makes the webhook URL, its secret and the deploy key |
| `POST` | `/api/sites/{id}/git-deploy/secret` | manage | make a new webhook secret and a new webhook URL; the old URL stops answering; 409 when git deploy was never turned on |
| `POST` | `/api/sites/{id}/git-deploy/key` | manage | make a new deploy key and return its public half; 409 when git deploy was never turned on |
| `POST` | `/api/sites/{id}/git-deploy/run` | manage | deploy the branch's tip now, as a push would (`202`, `task`, `release_id`, `deployment_id`; `coalesced` when a deploy was already waiting); 409 when git deploy is off |
| `POST` | `/hooks/git/{hook}` | site token | the git webhook of GitHub, GitLab and Gitea: authenticated by `X-Hub-Signature-256`, `X-Gitea-Signature` or `X-Gitlab-Token`, answers `401` to anything else (an unknown hook and a wrong signature alike) and `429` after too many failures; a push to the site's branch queues a release and its promotion, a ping answers `pong`, anything else is `ignored` |
| `POST` | `/api/sites/{id}/staging` | manage | make a staging copy of the site |
| `POST` | `/api/sites/{id}/staging/recreate` | manage | delete the staging copy and clone production again |
| `GET` | `/api/sites/{id}/staging/credentials` | manage | the staging copy's URL and the user and password protecting it |
| `GET` | `/api/sites/{id}/staging/diff` | manage | what differs between staging and production |
| `GET` | `/api/sites/{id}/staging/tables` | auth | what a database sync would copy |
| `POST` | `/api/sites/{id}/staging/database` | manage | copy production's database to staging again |
| `POST` | `/api/sites/{id}/safe-push` | manage | promote staging if its measurements pass |

### Backups

| Method | Path | Access | What it does |
| :-- | :-- | :-- | :-- |
| `GET` | `/api/sites/{id}/backups` | auth | the site's backups, each with the `databases` its snapshot holds (`null` for one from before they were recorded) |
| `POST` | `/api/sites/{id}/backups` | manage | take one, then restore-test it |
| `POST` | `/api/backups/{id}/verify` | manage | test-restore the backup to a scratch place |
| `POST` | `/api/backups/{id}/restore` | manage | restore in place — `mode` is `full`, `files` or `database`; `confirm` is the domain; `databases`, optional, restores only these of the snapshot's (a backup lists them in `databases`); `paths`, optional with `files`, only these files or folders |
| `POST` | `/api/backups/{id}/archive` | manage | prepare a download of the backup — `part` is `full`, `files` or `database`; returns `202` with the task |
| `GET` | `/api/backups/{id}/archive?part=` | manage | download the prepared archive (`.tar.gz`: `files/`, `databases/<name>.sql`), streamed; deleted once sent |
| `GET` | `/api/settings/backup-ssh-key` | admin | the public key the panel signs in to an SFTP backup destination with (`""` before one is made) |
| `POST` | `/api/settings/backup-ssh-key` | admin, session only | make a new key pair for SFTP destinations, replacing the old one; returns the public key |
| `DELETE` | `/api/backups/{id}` | admin | delete the snapshot from the repository (queued under its lock); refused for one whose restore test has not run and for the site's newest verified one |
| `POST` | `/api/backups/test` | admin | try the backup repository's credentials |
| `PUT` | `/api/sites/{id}/backups/policy` | manage | the site's backup policy: any of `enabled`, `schedule` (`hourly`, `daily`, `weekly`), `hour` (0–23, the panel's time zone, for daily and weekly; -1 for none), `retention_days` (0 keeps every snapshot, at most 3650; changing it is an admin's); answers the policy |
| `POST` | `/api/sites/{id}/backups/retention` | admin | apply the site's retention window now (it also runs daily) |

### Files

Jailed to the site root. Every path is resolved by the kernel beneath the site's root
(`openat2(RESOLVE_BENEATH)`): a relative symlink inside the site (`current`, `wp-config.php`) is
followed, one leading outside is refused. Saving writes a new file and renames it over the old one,
so a hard link is never written through. A missing path answers `404`, a name already taken `409`,
and text sent for a binary file `409`.

| Method | Path | Access | What it does |
| :-- | :-- | :-- | :-- |
| `GET` | `/api/sites/{id}/files?path=` | auth | entries of a folder |
| `GET` | `/api/sites/{id}/files/read?path=` | manage | read a text file (2 MB cap) |
| `POST` | `/api/sites/{id}/files/write` | manage | save a text file |
| `GET` | `/api/sites/{id}/files/download?path=` | manage | download (16 MB cap; use SFTP above that) |
| `POST` | `/api/sites/{id}/files/upload?path=` | manage | upload the raw file as `application/octet-stream` (16 MB cap) |
| `POST` | `/api/sites/{id}/files/manage` | manage | remove, rename or create a folder |

### Database

| Method | Path | Access | What it does |
| :-- | :-- | :-- | :-- |
| `GET` | `/api/db-servers` | global | the registered external MariaDB and MySQL servers (the box's own is server 0): `name`, `host`, `port`, `tls`, `ca_cert`, `admin_user`, whether its password is set (never the password) and how many databases are on it |
| `POST` | `/api/db-servers` | admin | register an external server (`name`, `host`, `port` 3306, `tls` `required` or `off`, `ca_cert`, `admin_user`, `admin_password`); the agent connects first and a refusal is 400 with the server's own message |
| `PUT` | `/api/db-servers/{id}` | admin | change an external server, tested again; the password is kept when left out or `••••••••` |
| `POST` | `/api/db-servers/{id}/test` | admin | connect to the server now: `{ok, version}`, or 502 with the error |
| `DELETE` | `/api/db-servers/{id}` | admin | forget an external server and its password; 409 while a database or a database user is on it |
| `GET` | `/api/sites/{id}/databases` | auth | the site's databases (`name`, `primary`, `size_bytes`, `tables`; -1 when unknown) and database users with their `grants` (`database`, `level`: `all` or `read`) |
| `POST` | `/api/sites/{id}/databases` | manage | create a database: `name` (3–48 of `a-z0-9_`, a letter first, never a reserved or system name, unique on the server) — returns `202` and a task |
| `DELETE` | `/api/sites/{id}/databases/{name}?confirm=` | manage | delete a database (not the primary); `confirm` is its name; its backups are kept — returns `202` and a task |
| `POST` | `/api/sites/{id}/database-users` | manage | create a database user: `name`, `password` (12+ characters; empty generates one, returned once in `password`), `grants` |
| `DELETE` | `/api/sites/{id}/database-users/{name}?confirm=` | manage | delete a database user (not the primary); `confirm` is its name |
| `POST` | `/api/sites/{id}/database-users/{name}/password` | manage | set its password (`password`; empty generates one, returned once); the primary user of a WordPress site gets it in `wp-config.php` too |
| `PUT` | `/api/sites/{id}/database-users/{name}/grants` | manage | replace its `grants` on the site's databases; the primary user keeps `all` on the primary database |
| `GET` | `/api/sites/{id}/database?database=` | auth | each table with its rows and size |
| `POST` | `/api/sites/{id}/database/query` | manage | run one SQL statement |
| `POST` | `/api/sites/{id}/database/export` | manage | write a dump into the site's `shared/exports/` |
| `POST` | `/api/sites/{id}/database/import` | manage | load a `.sql` file found in the site |

The console is **read-only unless you pass `"allow_writes": true`** (then the statement is audited).
Every query is a single statement — `select 1; drop table x` is refused — and runs as a throwaway
account granted on that site's database only: read-only mode grants `SELECT` and `SHOW VIEW`, so the
server itself refuses writes and cross-database access, and its error comes back in `message`. A
statement may run 60 seconds; then the server stops it. Results stop at 5000 rows or 8 MiB.

### Quarry

The panel's database manager, a service of its own at `https://<panel>/quarry` with **its own
session**: the cookie `__Host-cg-quarry` (`HttpOnly`, `Secure`, `SameSite=Strict`, `Path=/`),
idle after 30 minutes, ended after 12 hours, on `POST /api/quarry/logout` and when the panel
restarts. A panel session or an API token reaches none of the `/api/quarry/` routes (`401 sign in
to Quarry`), and a Quarry session none of the panel's. There are two ways in:

- **A database user's own name and password** (`POST /api/quarry/login`), without a panel
  account. MariaDB checks them, and every statement then runs as that user: what it sees and changes
  is what the server grants it. Only a site's own database users sign in (never `root` or the
  agent's accounts); the password stays in the agent's memory, sealed, for the session's life only.
  Limits: 20 attempts a minute per network (an IPv4 address, an IPv6 /64), 80 per IPv6 /48, 6 per
  network and user. After 5 refusals in 15 minutes an attempt waits 1 s, then 2 s, 4 s …: up to
  60 s for the refusals from its own network, at most 2 s for those from anywhere. Parallel attempts
  from one network wait one after the other's delay, and one that would wait over 60 s is refused
  (429): one network is no faster in parallel. The wait comes first and holds nothing; then the user's password is checked one attempt at a time, with at most
  4 queued per network. A delay, never a lock: someone failing from one network slows their own
  guesses, not the user signing in from another. A refusal is `401 invalid credentials` whatever
  the reason, and takes at least 300 ms. At most 32 signed-in sessions per site and 256 in all:
  past either, a correct name and password get `429` saying so (sign out of one, or wait until
  one has gone 30 minutes unused).
- **From the panel**: `POST /api/sites/{id}/quarry/handoff` (a signed-in session, never a token)
  returns a one-time `token` valid 60 seconds, which `POST /api/quarry/handoff` exchanges, with
  the same panel session, for a Quarry session on that site. It works through a throwaway account
  granted on one of the site's databases, and only while that panel session lasts and the account
  still reaches the site. A `readonly` account's session shows the tables and their structure, not
  their rows (`403`).

Table and column names in a request are looked up in `information_schema` and must match exactly;
values are always bound. Every write is audited before it is sent, with its action, database, table
and primary key, never the row's contents; a database user's SQL statements other than reads are
audited redacted.

| Method | Path | Access | What it does |
| :-- | :-- | :-- | :-- |
| `POST` | `/api/quarry/login` | open | sign in with a database user's own `user` and `password` (and optionally the `database` to open); the server checks them, and the session sees what that user is granted |
| `POST` | `/api/sites/{id}/quarry/handoff` | auth, session only | a one-time link into Quarry for this site (`{database}` optional): `token`, valid 60 seconds, for this panel session only |
| `POST` | `/api/quarry/handoff` | open | exchange a handoff `token` for a Quarry session, with the panel session that asked for it |
| `GET` | `/api/quarry/session` | open | the Quarry session (`mode` `login` or `panel`, `user`, `site`, `database`, `read_only`), or `{"session": null}` |
| `POST` | `/api/quarry/logout` | dbstudio | sign out of Quarry |
| `GET` | `/api/quarry/databases` | dbstudio | the databases the session may open |
| `GET` | `/api/quarry/db/{db}/tables?columns=` | dbstudio | tables and views (rows, size, engine, collation) and routines (`columns=1` adds every table's columns) |
| `GET` | `/api/quarry/db/{db}/tables/{table}` | dbstudio | structure: columns, indexes, foreign keys, primary key, `CREATE` statement |
| `POST` | `/api/quarry/db/{db}/tables/{table}/rows` | dbstudio data | a page of rows: `{page, page_size, sort: [{column, desc}], filters: [{column, op, value}], search}` |
| `POST` | `/api/quarry/db/{db}/tables/{table}/edit` | dbstudio data | insert, update or delete by primary key in one transaction: `{changes: [{op, key, values}], dry_run}` |
| `POST` | `/api/quarry/db/{db}/tables/{table}/alter` | dbstudio data | add a column or an index, or empty or drop the table: `{kind: add_column \| add_index \| truncate \| drop, column, index, confirm, dry_run}` (`confirm` is the table's name) |
| `POST` | `/api/quarry/db/{db}/query` | dbstudio data | the SQL editor: `{sql, allow_writes, explain}` (`allow_writes` for a session opened from the panel) |
| `POST` | `/api/quarry/db/{db}/simulate` | dbstudio data | simulate one UPDATE, DELETE, INSERT or REPLACE in a transaction that is always rolled back: `{sql}` → `{affected, preview, refused, message}` |
| `POST` | `/api/quarry/db/{db}/search` | dbstudio data | count the rows containing `term` in each table (or `tables`) |
| `POST` | `/api/quarry/db/{db}/export` | dbstudio data | export, as a task: `{format: sql \| csv, tables}` (CSV: one table) |
| `POST` | `/api/quarry/db/{db}/import` | dbstudio data | import an upload, as a task: `{name, format: sql, confirm: <database>}` or `{name, format: csv, table}` |
| `GET` | `/api/quarry/files` | dbstudio data | the exports and uploads the session may see (a database user: its own) |
| `GET` | `/api/quarry/exports/{name}` | dbstudio data | download an export, streamed |
| `DELETE` | `/api/quarry/files/{dir}/{name}` | dbstudio data | delete an export or an upload (`dir`: `exports` or `imports`) |
| `POST` | `/api/quarry/import/upload?name=&offset=` | dbstudio data | one chunk (at most 8 MiB, `application/octet-stream`) of an upload to `shared/imports` |
| `GET` | `/api/quarry/tasks/{id}` | dbstudio | one of the tasks the session started |
| `GET` | `/api/sites/{id}/quarry/wp/autoload` | auth | WordPress: size of the autoloaded options and the largest ones |
| `POST` | `/api/sites/{id}/quarry/wp/search-replace` | manage | WordPress: serialized-safe search and replace through wp-cli: `{from, to, dry_run}` |
| `POST` | `/api/sites/{id}/quarry/wp/transients` | manage | WordPress: delete (or with `dry_run`, count) the expired transients |

- `edit` and `alter` with `"dry_run": true` return the SQL without running it; without, they return
  the same SQL, now executed. An edit that fails, or whose key matches no row, rolls the whole batch
  back.
- Filters: `eq ne lt le gt ge contains starts null notnull`. Long cells come back cut at 4 KiB (their
  indexes are in `cut`), binary ones as hex; `null` is NULL.
- Exports and uploads may reach 16 GiB. A SQL import goes through the sandboxed `mariadb` client:
  from the panel with a rollback point, as `database/import` does; as a database user, signed in as
  that user and without one. A CSV import (header row of column names, `\N` for NULL) inserts every
  row in one transaction. A database user sees, downloads and imports only the files it made.
- `alter` with `truncate` or `drop` needs `confirm`, the table's name.

### WordPress

| Method | Path | Access | What it does |
| :-- | :-- | :-- | :-- |
| `POST` | `/api/sites/{id}/wp/login` | manage | a sign-in link for wp-admin, valid once for 5 minutes |
| `GET` | `/api/sites/{id}/wp/plugins` | auth | installed plugins and themes, and their updates |
| `POST` | `/api/sites/{id}/wp/update` | manage | apply updates to `core`, `plugins`, `themes` or `all`, behind a restore-tested snapshot: the site is measured before and checked after, and restored from the snapshot when the check fails (412 without a backup repository, unless `without_snapshot` asks to update with no rollback) |
| `PUT` | `/api/sites/{id}/wp/updates` | manage | the update schedule: any of `enabled`, `schedule` (`daily`, `weekly`), `hour` (0–23, the panel's time zone; -1 for none), `what` (`core`, `plugins`, `themes`, `all`); answers the schedule |
| `PUT` | `/api/sites/{id}/wp/hardening` | manage | WordPress hardening, each item on its own: `{"xmlrpc_off", "xmlrpc_allow": [addresses or networks], "file_edit_off", "hide_version", "block_readme", "login_path"}`; answers the hardening |
| `PUT` | `/api/sites/{id}/wp/two-factor` | manage | two-factor sign-in for the site's accounts: `{"mode": "off"}`, `optional` (each account can set it up) or `required` (also compulsory for administrators and shop managers); answers the mode |

### SFTP, shell and keys

| Method | Path | Access | What it does |
| :-- | :-- | :-- | :-- |
| `POST` | `/api/sites/{id}/sftp` | manage | turn SFTP on (`keys_only` and an optional `public_key`, or a new `password`) or off (`enable`) |
| `GET` | `/api/sites/{id}/ssh-keys` | auth | the site's SSH keys |
| `POST` | `/api/sites/{id}/ssh-keys` | manage | add a key (`public_key`, `access`: `sftp` or `shell`; a `shell` key only from an admin's session) |
| `DELETE` | `/api/sites/{id}/ssh-keys/{fingerprint}` | manage | remove that key |
| `POST` | `/api/sites/{id}/ssh-shell` | admin, session only | turn the site's SSH shell on or off (`{"enable": bool}`) |

`POST /sftp` with `{"enable": true, "password": "..."}` turns the login on with a password (12
characters at least); with `{"enable": true, "keys_only": true}` it takes keys only: no password,
the one it had locked, and sshd does not offer the site user a password at all. `keys_only` left
out is `true` when the body has a `public_key`, which is added (as an `"sftp"` key) before the
login is turned on: `{"enable": true, "public_key": "ssh-ed25519 ..."}` is a keys-only login with
that key. A keys-only request with a password is refused (400). The site's `config.sftp_keys_only`
says which it is. Keys only is the safer choice: a password login's only defence against guessing
is the password and sshd's own limits on attempts per connection, as CloudGround does not yet
block addresses that keep failing.

A fingerprint is `SHA256:` and the standard base64 `ssh-keygen -l` prints; percent-encode it in the
path (it can contain `/` and `+`). Keys sign in only while SFTP is on.

Every key has a type, `access`: `"sftp"` (the default) or `"shell"`, given when it is added
(`{"public_key": "...", "access": "shell"}`) and listed with each key. Every key signs in to SFTP
as the site user, chrooted in the site's folder. While an administrator has the site's shell on,
the `"shell"` keys also sign in to `ssh <shell_user>@host` (`shell_user` is in the key list,
`cg-shell-<id>`): bash in the site's folder as the site's uid, inside the site's sandbox. A shell
is an administrator's: the shell switch and a `"shell"` key need an admin signed in with a
session (an API token gets 403); an operator adds `"sftp"` keys to its sites.

### Scheduled jobs

| Method | Path | Access | What it does |
| :-- | :-- | :-- | :-- |
| `GET` | `/api/sites/{id}/cron` | auth | this site's jobs; `last_run` and `last_status` are the latest run, scheduled or on demand; for a read-only account `command` is redacted as in the audit log (first word, word count, fingerprint) |
| `POST` | `/api/sites/{id}/cron` | manage | create one (`schedule`, `command`, `description`, `enabled`) |
| `PATCH` | `/api/cron/{id}` | manage | change any of those fields; a disabled job leaves the schedule |
| `DELETE` | `/api/cron/{id}` | manage | remove the job |
| `POST` | `/api/cron/{id}/run` | manage | start the job at once |

### The server

| Method | Path | Access | What it does |
| :-- | :-- | :-- | :-- |
| `GET` | `/api/system/status` | global | free CPU, memory and disk, and the services |
| `GET` | `/api/system/metrics` | global | capacity samples of the last 24 hours |
| `GET` | `/api/system/capabilities` | auth | what the agent reports this server has: installed PHP versions, services, roots |
| `GET` | `/api/system/update` | auth | the running version and the newest release (`?refresh=1`, admins only, checks now) |
| `GET` | `/api/system/alerts` | auth | what needs attention, filtered to the caller's sites |
| `POST` | `/api/system/alerts/{id}/dismiss` | manage | dismiss one the caller can see |
| `GET` | `/api/system/drift` | global | managed files changed outside the panel |
| `POST` | `/api/system/drift/{id}/resolve` | admin | `{"action":"restore"}` puts the file back, `{"action":"accept"}` keeps it (accept: a signed-in session only, an API token gets 403) |
| `POST` | `/api/system/maintenance` | admin | run one maintenance tick now and wait for it |
| `GET` | `/api/php/{version}/extensions` | auth | what a PHP version loads: every module, and the common extensions with `installed` |
| `POST` | `/api/php/{version}/extensions` | admin | install one of the common extensions (`{"extension": "redis"}`) and restart the PHP of the sites on that version; answers the task |
| `GET` | `/api/services` | global | whether nginx, PHP-FPM, MariaDB and Redis run |
| `POST` | `/api/services/{name}/restart` | admin | restart that service |
| `GET` | `/api/system/updates` | global | the server's pending package upgrades, security first, and whether it needs a reboot |
| `POST` | `/api/system/updates/refresh` | admin | update apt's package lists, then list the pending upgrades again (a task) |
| `POST` | `/api/system/updates/apply` | admin, session only | upgrade every pending package, or only the security updates (`{"security_only": true}`) (a task) |
| `GET` | `/api/logs` | global | the last lines of a server log |
| `GET` | `/api/firewall` | global | the firewall's rules |
| `POST` | `/api/firewall/rule` | admin, session only | add a firewall rule or take one away |
| `POST` | `/api/firewall/enable` | admin, session only | turn ufw on: allow the rules `GET /api/firewall` listed as `plan` (`{"expect": ["22/tcp", …]}`), then enable |
| `GET` | `/api/blocklist` | global | the block list: blocked addresses (by hand or for failed sign-ins) with their end, the allowlist, the automatic blocking's settings, whether netfilter enforces it, and the caller's own address |
| `POST` | `/api/blocklist/blocks` | admin, session only | block an address or a network (`{"address": "203.0.113.0/24", "note": …, "minutes": 0}`, 0 is until removed); answers the block list |
| `DELETE` | `/api/blocklist/blocks` | admin, session only | remove a block (`{"address": …}`); answers the block list |
| `POST` | `/api/blocklist/allowed` | admin, session only | never block an address or a network (`{"address": …, "note": …}`); its automatic blocks end; answers the block list |
| `DELETE` | `/api/blocklist/allowed` | admin, session only | take an address off the allowlist (`{"address": …}`); answers the block list |
| `PUT` | `/api/blocklist/settings` | admin, session only | the automatic blocking: `enabled`, `max_failures` (3–100) in `window_minutes` (1–1440) block for `ban_minutes` (1–43200); answers the block list |
| `GET` | `/api/audit` | global | the audit log: actor, action, address |
| `GET` | `/api/settings` | global | the panel's settings |
| `PUT` | `/api/settings` | admin | change the panel's settings (an API token may change only `acme_email` and `probe_target`) |
| `POST` | `/api/settings/mail/test` | admin | send a test message through the server-wide mail relay: `{"to"}` |
| `POST` | `/api/panel/certificate` | admin, session only | give the panel its own certificate |
| `POST` | `/api/panel/update` | admin, session only | record an update request for the panel |

`POST /api/system/maintenance` runs, synchronously, one tick of the scheduler's five-minute work, each
piece a maintenance job: the reconcile after an upgrade or a change in the number of sites (PHP pools
resized), a capacity sample, the backup and retention sweeps (the scheduled backups they queue are
not waited for), the certificate renewals if due, the drift scan, then the alerts. A job of the tick
already queued or running is waited for, not doubled. It answers `{"ran": true, "duration_ms": N}`,
or, when the tick would hold the request more than 10 s (maintenance waiting behind long jobs),
`202 {"ran": false, "jobs": [ids]}`: the tick goes on, and its jobs can be polled with
`GET /api/tasks/{id}`. Each run is recorded in the audit log as `system.maintenance`.

`POST /api/panel/update` takes `{"version": "v0.3.0"}`, which is recorded in the audit log and
nothing more. The request cannot name a source or waive verification: the update origin and the key
releases must be signed with are compiled into the agent. See [Update CloudGround](https://cloudground.it/en/docs/server/update-cloudground).

### Tasks and the event stream

| Method | Path | Access | What it does |
| :-- | :-- | :-- | :-- |
| `GET` | `/api/tasks` | auth | the latest tasks |
| `GET` | `/api/tasks/names` | auth | every task kind in words, in the reader's language (`{"backup.run": "Backup"}`) |
| `GET` | `/api/tasks/{id}` | auth | a task and its log |
| `POST` | `/api/tasks/{id}/cancel` | manage | cancel a queued task, or ask a running one to stop |
| `GET` | `/api/events` | auth | server-sent events stream for live updates, filtered per user |

Tasks are the jobs of the panel's persistent queue. A task waiting for another one on the same site
(or the same backup repository) is `pending`; its `job_status` says `queued`. Cancelling a queued
task ends it at once (`status` `failed`, `job_status` `canceled`, `error` `Canceled`) and undoes
what the request stored ahead of it (a settings change's values, a new site's record, a delete's
status); a running one gets `cancel_requested` and stops before its next change, since a change
already sent to the agent always runs to its end (a restore canceled during its safety snapshot
never restores). A restore test cannot be canceled (`409`). Your tasks go before the scheduler's
in the queue of a site or a repository. After a panel restart nothing stays `running`: an
interrupted task either runs again (settings renders, backups, restore tests, retention, at most
three starts) or ends `failed` with `interrupted by panel restart`; a cancel asked before the
restart is kept. Only an admin cancels a server-wide task.

`GET /api/events` is a `text/event-stream` of the panel's changes, one JSON object per message:
`{"type": "site.changed", "site_id": 12, "at": "…", "data": {…}}`. `site_id` is absent on
server-wide events. Each message has an SSE `id`; reconnecting with `Last-Event-ID` (or
`?last_event_id=`) replays what was missed from a short history, or sends `{"type":"resync"}`
when that is not possible, after which a client should reload what it shows. A
`{"type":"ping"}` message without an ID arrives every 25 seconds.

| Type | About | `data` |
| --- | --- | --- |
| `task.created`, `task.progress`, `task.finished` | a task (site or server) | the task without its log; progress and message; status, error |
| `site.changed`, `site.deleted` | a site | `reason` (`created`, `updated`, `applied`), status, generations |
| `deployment.changed` | a site | release id and status |
| `backup.finished`, `backup.verified`, `backup.removed` | a site | backup id and kind or verification status; count removed by retention |
| `cron.changed`, `cert.changed` | a site | job id; nothing |
| `alert.raised`, `alert.cleared` | a site or the server | the alert (`id`, `level`, `kind`, `message`); its id |
| `drift.changed`, `service.changed`, `system.status`, `metrics.sample`, `settings.changed`, `user.changed` | the server | drift event id; changed service names; what `GET /api/system/status` answers; the sample; nothing; user id |

Who receives what: `admin` and `readonly` accounts receive every event. An `operator` receives only
events about its assigned sites and their staging copies, never server-wide ones. The stream ends
when its session does (logout, expiry, a disabled account), and a request from another origin is
refused with `403`. The full contract is `internal/api/events/SPEC.md`.

### WordPress connector

| Method | Path | Access | What it does |
| :-- | :-- | :-- | :-- |
| `POST` | `/api/connector/purge` | site token | the WordPress connector's purge request |

### Assistants (MCP)

The remote MCP server (SPEC §16), off by default: an administrator turns it on with the setting
`mcp`, and while it is off every route here answers `404`. Assistants register themselves, are
authorised with OAuth 2.1 (authorisation code with PKCE) on the panel's consent page, and call
tools at `/mcp` with a bearer token; destructive tools only record an action you confirm here.

| Method | Path | Access | What it does |
| :-- | :-- | :-- | :-- |
| `GET` | `/.well-known/oauth-protected-resource` | mcp | the MCP resource's metadata (RFC 9728): where to authorise |
| `GET` | `/.well-known/oauth-protected-resource/mcp` | mcp | the same, at the path-specific address |
| `GET` | `/.well-known/oauth-authorization-server` | mcp | the authorisation server's metadata (RFC 8414) |
| `POST` | `/oauth/register` | mcp | register a public client (RFC 7591): `client_name`, `redirect_uris` |
| `GET` | `/oauth/authorize?response_type=code&client_id=&redirect_uri=&code_challenge=&code_challenge_method=S256&state=&resource=` | mcp | start an authorisation: redirects to the panel's consent page |
| `POST` | `/oauth/token` | mcp | exchange a code (with its PKCE verifier) or a refresh token for tokens; form-encoded |
| `POST` | `/oauth/revoke` | mcp | revoke a grant by one of its tokens (RFC 7009) |
| `POST` | `/mcp` | mcp | the MCP endpoint (Streamable HTTP, JSON answers): `Authorization: Bearer cgm_…` |
| `GET` | `/mcp` | mcp | `405`: the server starts no stream |
| `DELETE` | `/mcp` | mcp | `405`: the server keeps no session |
| `GET` | `/api/mcp/requests/{id}` | auth, session only | an authorisation request waiting for your consent |
| `POST` | `/api/mcp/requests/{id}/approve` | auth, session only | consent: `write` (`{area: [site ids]}`) and a proof; returns the `redirect` |
| `POST` | `/api/mcp/requests/{id}/deny` | auth, session only | refuse; returns the `redirect` |
| `GET` | `/api/mcp/grants` | auth, session only | the assistants connected to your account (an admin's: every account's) |
| `DELETE` | `/api/mcp/grants/{id}` | auth, session only | disconnect one |
| `GET` | `/api/mcp/actions` | auth, session only | the destructive actions your assistants asked for |
| `POST` | `/api/mcp/actions/{id}/confirm` | auth, session only | confirm one (a proof): it runs now |
| `POST` | `/api/mcp/actions/{id}/reject` | auth, session only | reject one |

These calls carry the site's own bearer token instead of a session, and a token purges only URLs on
the domains of the site it belongs to.
