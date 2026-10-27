# `internal/agent/ops/sites` — creating, deleting and applying a site (specification)

A site is a Unix user, a directory tree, optionally a database, a PHP-FPM
master of its own, an nginx vhost and (for WordPress) a cron schedule. This package
creates all of it, tears all of it down, and re-renders the configuration
from the API's desired state. It is the last domain of the agent rewrite
because it uses every other one: `ops/db`, `ops/sftp`, `ops/release`,
`ops/wp`, `ops/cron`, `ops/certs`, over `sitefs` and the `host` adapters.

Sources: the e2e suite (`e2e/e2e_test.go` site types, `reconcile_test.go`
R-0.7, `layout_test.go`, `security_test.go`), the API callers
(`internal/api/sites.go`, `scheduler.go`, `php.go`, `system.go`), the
golden renders in `testdata/golden` (taken from the agent before this
rewrite).

## 1. Methods

| Method | Params → Result | Table row |
|---|---|---|
| `CreateSite` | `CreateSiteParams{Site, DBPassword, Admin*, SiteTitle, ConnectorToken}` → `CreateSiteResult{SystemUser, RootPath, DatabaseName, DatabaseUser, Files}` | Sites, **`LockSite`**, 45 min |
| `DeleteSite` | `SiteRef{Site}` → `{"deleted": <domain>}` | Sites, `LockSite`, 15 min |
| `ApplySiteConfig` | `SiteRef{Site, ReloadOnlyIfChanged}` → `ApplyResult{Files}` | Sites, `LockLiveSite`, 15 min |
| `CheckDocRoot` | `SiteRef{Site}` → none | Sites, read-only, 30 s |
| `ReloadWebServer` | none → none | 2 min |
| `PHPExtensions` | `PHPExtensionsParams{Version}` → `PHPExtensionsResult{Version, Modules, Extensions}` | read-only, 1 min (§9) |
| `PHPExtensionInstall` | `PHPExtensionInstallParams{Version, Extension}` → `PHPExtensionsResult` | 15 min (§9) |
| `SetAppEnv` | `AppEnvParams{Site, Env}` → none | Sites, `LockLiveSite`, 2 min (§11) |
| `AppStatus` | `SiteRef{Site}` → `AppStatusResult` | Sites, read-only, 30 s (§11) |
| `AppControl` | `AppControlParams{Site, Action}` → `AppStatusResult` | Sites, `LockLiveSite`, 2 min (§11) |

`CheckDocRoot` (R-4.3) answers `invalid` when a component of the site's
public root beneath the live release (`current`) is a symbolic link or not a
folder, read with `Lstat` through the site's confined root; a component
that does not exist yet ends the check. Every render runs the same check
first and refuses with the same error. In the vhost, a site with a public
root has `disable_symlinks if_not_owner from=<root>/current` instead of
`from=$document_root`, so nginx checks the public root's components too
(a link to another tenant's tree is owned by the site, its target is not);
sites without a public root are rendered as before.

`CreateSite` used to run without the site's lock: an apply or a deletion of
the same domain could interleave with provisioning. It now takes it, like
`DeleteSite` (`LockSite`: the site does not exist yet).

Called inside the agent, by handlers that already hold the site's lock:
`Provision` and `Teardown` (staging's clone and its rollback), `Render`
(certificates, staging), `Verify` (staging), `ReloadPHP` / `RestartPHP`
(releases, restores, the object cache), `ReloadNginx` (restores). None of
them takes the site lock; the lock is not reentrant.

## 2. Names

| What | Name |
|---|---|
| user and group | `cg-site-<id>` (`site.SystemUser`) |
| slice | `cg-site-<id>.slice` (`site.Slice`), a child of `cg-site.slice` |
| PHP master unit | `cloudground-php-<user>.service` (`phpfpm.UnitName`) |
| its slice drop-in | `<SystemdDir>/<unit>.d/cloudground-slice.conf` |
| pool / master config | `<PHPMasterDir>/<user>.conf` |
| vhost | `<NginxSites>/<domain>.conf` |
| page-cache zone | `<NginxConfDir>/cloudground-cache-<id>.conf` |
| refused visitors (#25) | `<NginxConfDir>/cloudground-block-<id>.conf` |
| staging password | `<NginxAuthDir>/<domain>.htpasswd` |
| protected path password (#26) | `<NginxAuthDir>/<domain>.path-<first 6 bytes of the path's SHA-256, hex>.htpasswd` |
| logs / page cache | `<LogRoot>/<domain>`, `<CacheRoot>/<sanitised domain>` |
| tree | `<SitesRoot>/<domain>` |

## 3. Per-site slice

Every process of a site runs in its slice:

- the **PHP master**: its unit is the template's (`phpfpm.RenderUnit`,
  unchanged), and a drop-in written next to it says `Slice=cg-site-<id>.slice`
  (`[Service]`). The drop-in is a managed file like the unit;
- **cron jobs**, scheduled and run now (`ops/cron`): transient services
  with the PHP master's sandbox (R-4.5 review);
- **wp-cli** (`ops/wp`, `ops/staging`, this package): a transient service
  with the PHP master's sandbox (`host.Invocation.Sandbox`);
- **the SSH shell** (`ops/sftp`), the same way.

Not in it: SFTP sessions (sshd's, chrooted). Deletion deals with those
through the user (§6).

### 3.1 The slice unit: accounting and limits (#18, #19)

Every site, PHP or not (a static site's cron jobs and shell are in its slice
too), has a unit file for its slice, `<SystemdDir>/cg-site-<id>.slice`
(`site.SliceUnit`), a managed file like the others (drift: `system` ops'
`managed`, the API's `siteOwnsPath`). `[Slice]`:

- `CPUAccounting`, `MemoryAccounting`, `IOAccounting`, `TasksAccounting`:
  the counters the panel's usage figures are read from (`system` ops'
  `SiteUsage`, api SPEC §11.5);
- `MemoryHigh` (nine tenths of the limit: the kernel reclaims and slows the
  site there) and `MemoryMax` (one of the site's processes is killed there,
  never another site's; the PHP master keeps running, phpfpm SPEC §4
  `OOMPolicy=continue`);
- `CPUQuota` when the site has one (`CPUQuotaPct`, 100 per CPU); none by
  default, so a site may use every CPU;
- `TasksMax`.

`site.EffectiveLimits` gives the values: the site's own (`Config.Resources`),
or the defaults, measured with `bench/` (`bench/site-resources.md`): memory
half the server's (at least 1024 MB; the WooCommerce bench site peaked at
561 MB through the whole suite, its cron and a cache warm), 512 tasks (it
peaked at 8), no CPU quota (it used both CPUs of the 2-CPU box under the
uncached scenario, so any quota would have cut its throughput).

The unit is written before the PHP master's files (render step 4) and, when
it changed, systemd is reloaded: a running slice takes new limits on the
reload, nothing in it is restarted (checked on Ubuntu 26.04: `memory.max`,
`pids.max` and `cpu.max` change at once, and go back to `max` when the unit
is removed). A failed render restores it with the rest and reloads again.

## 4. Render

`Render(ctx, site)` writes everything the site's configuration consists of
and loads it; `Apply` renders with `ReloadOnlyIfChanged`. The caller holds
the site's lock, which covers the site's own files and PHP master (steps
1–4). The agent's nginx lock (shared by every site and the panel's vhost) is
taken only for step 5: nginx's files and the reload after them. **No lock
shared between sites is held across a systemd job**: the PHP master's
restart used to be waited for under the nginx lock, and one job result lost
in the R-3.7 storm stalled every render and deletion on the machine for
22 minutes (host/SPEC.md, systemd "Why").

**Modes are exact, never the umask's.** The agent runs with `UMask=0027`
(`internal/units`), under which `os.MkdirAll(p, 0755)` makes 0750: on a
freshly imaged server `/etc/cloudground-php` came out 0750 and every PHP
site failed its FPM test ("failed to open configuration file …: Permission
denied"). Every file is written with its mode set on the descriptor, and
every directory the agent creates gets exactly its mode
(`rootfile.MkdirAllMode`): the parents of a file it writes (nginx's
directories, the units' drop-in directory, the password directory before it
is narrowed) 0755, the roots above a site's cache and log directories 0755,
the sites' root 0755 (§5). A directory that exists is left as it is, except
the PHP masters' directory, set to 0755 on every PHP render (step 4).

1. **Input** (`engine.Input`): document root `<root>/current[/<public_root>]`;
   the certificate is the operator's (`<CustomCertDir>/<domain>/fullchain.pem`
   when it exists) or Let's Encrypt's (`<CertLiveDir>/<domain>`), and
   `CertAvailable` says the chosen fullchain exists; PHP socket
   `<PHPSocketDir>/<user>/php.sock` except for static and proxy sites; a
   staging site (`StagingAuthHash`) gets its password file and internal key;
   a site with blocking rules (`Config.Blocking`) its addresses, user agents
   and countries, the countries resolved by `CountryPrefixes` (ops/blocklist
   SPEC §4; a failure fails the render, nothing written), and this
   machine's addresses as the exempt ones (internal/engine SPEC §4.7); a
   site without rules has its rules file removed in the same nginx change
   (put back if the change is rolled back);
   every site the socket WordPress vhosts log failed sign-ins to
   (`Paths.AuthLogSocket`, ops/blocklist SPEC §1.2; internal/engine SPEC §4.6);
   each protected path (`Config.ProtectedPaths`) its own password file, one
   line `<username>:<bcrypt hash>`.
   `HTTP3` and `HTTP2Directive` (nginx ≥ 1.25.1) come from `nginx -V`, read
   once per process (`host.Nginx.BuildInfo`, which reads stderr).
2. The renderer's site files and global files. The page-cache directory is
   created (0700) when the policy caches; the log directory is created and
   set to root:<site group> 0750 (nginx's workers, in every site group,
   reopen logs there; the site's PHP writes its error log there). For a PHP
   site, `php-error.log` is created there, owned by the site user, never
   through a link.
3. Each nginx file is written atomically (temp file + rename) with mode 0644;
   the password files are 0640 root:www-data in a 0750 root:www-data
   directory; a vhost carrying the internal key is 0600. Every password hash
   must be bcrypt (`$2a/b/y$NN$` + 53 characters), and a protected path's
   username `site.ValidBasicAuthUser`, or the render is refused. Once nginx
   has the new files, the site's protected-path password files the render
   did not write (a path no longer protected) are removed.
4. **PHP master** (PHP sites): the budget is `MemoryHighMB`, or when unset a
   quarter of the machine's memory shared by the provisioned sites (trees
   with a `releases/` directory), at least 160 MB (and a warning when the
   floor binds). The masters' directory (`PHPMasterDir`,
   `/etc/cloudground-php`) is made 0755, even when it exists: the master and
   its test read the pool as the site's user. Pool (0640, root:<site group>),
   unit (0644) and slice
   drop-in (0644) are written. If the unit or drop-in changed: daemon-reload.
   Then:
   - `ReloadOnlyIfChanged` and nothing of the three changed: enable and
     start (a no-op on a running master: its workers, OPcache and APCu are
     kept);
   - otherwise: `php-fpm<ver> -t --fpm-config <pool>` as the site user, in
     a transient service (`systemd-run --wait --pipe`, `host.ServiceArgs`:
     unit `cg-fpm-test-<user>-<n>.service`, the site's slice,
     `phpfpm.Sandbox` as properties, `RuntimeMaxSec=60`) — the master's
     own confinement, never `runuser` in the agent's mount namespace, where
     the site's code saw the whole machine through the agent's view (R-10.5;
     a cancelled test stops its unit). A failure carries php-fpm's output.
     Then enable, then restart when the unit
     or drop-in changed (the command line holds the cache sizes; the slice
     can only change on a start), reload-or-restart when only the pool did.
5. nginx, under the nginx lock: the files of step 3 are written, then,
   unless `ReloadOnlyIfChanged` and no nginx file changed, `nginx -t` and a
   reload that returns once a new worker generation runs (no sleep). The PHP
   master is loaded before nginx routes to it, as before.
6. **Failure** at any step restores every file written to what it was —
   content, mode and owner — or removes it. nginx's files are restored and
   nginx reloaded before the nginx lock is let go, so no other render sees or
   keeps half of this one; then, outside it, systemd is reloaded and the PHP
   master restarted if its unit existed before; a first render has
   nothing to go back to, so the master it may have started is stopped. A
   failed restore is reported with the cause. The restore runs even when the
   request was cancelled. (The old rollback restored every file 0644: a
   staging vhost, 0600 because it holds the key that passes the password,
   became world-readable.)
7. **Result**: every file written, with its SHA-256, plus, for WordPress,
   `<root>/current/wp-content/mu-plugins/cloudground-connector.php` with the
   embedded connector's hash (drift covers it).

The rendered bytes of every template-produced file are those of the agent
before this rewrite (`testdata/golden`, eight sites under nginx 1.24 and
1.28); the drop-in is the only file added. Two deliberate changes since:
the pool's `process_control_timeout` is min(`request_terminate_timeout`,
30 s) (R-5.2, `internal/phpfpm/SPEC.md` §5.2), so a reload kills only
requests running past 30 s (it was 10 s) and holds the site's other
requests for at most 30 s; and the php site's cache bypass also matches
`PHPSESSID` (R-5.2, internal/engine SPEC §5.1), the only other difference
(one line of `fast.example.com`).

PHP control used by other domains: `ReloadPHP` (reload-or-restart; nothing
to do for a site without PHP or whose unit is not there yet), `RestartPHP`
(restart; nothing for a site without PHP).

## 5. Create

`CreateSite` = `Provision` under the site lock. Refusals before anything is
made, so that a failure can never touch something that was there before:

- a PHP version whose FPM binary (`<PHPBinDir>/php-fpm<ver>`, a regular
  file) is missing, naming the installed ones (none found: not refused);
- an existing user of that name (`conflict`), an existing root
  (`conflict`), an existing database (`conflict`).

Steps, each with its undo, undone in reverse order on any later failure:

| Step | Undo |
|---|---|
| user: `useradd --system --home-dir <root> --no-create-home --shell /usr/sbin/nologin` | delete the user (§6) |
| SFTP off (deny group) | — (goes with the user) |
| tree: `<root>` 0750, `shared/uploads`, `logs`, `tmp/sessions`; the skeleton locked to root:<group> (`release.LockSkeleton`); www-data joins the site group | remove the tree |
| database and account (local only) | drop them |
| `releases/initial` and `current -> releases/initial` | (the tree) |
| application: WordPress (`ops/wp` §11), object cache (a failure is logged), WooCommerce for the woocommerce type or commerce profile; a placeholder `index.html` (static) or `index.php` (php, laravel, under the public root) | (the tree) |
| precompression of the release's assets (a failure is logged), the release chowned to the site user | (the tree) |
| render (§4) | as deletion's steps 1-3 (§6), plus the page cache and logs |
| WordPress: the cron schedule (`ops/cron` Apply with no jobs: WordPress's events) | remove the schedule |
| verify: the user exists, the live release resolves, the vhost exists, the pool exists (PHP), the database exists (local) | — |

The undo runs with a context of its own: a cancelled request still cleans
up. Undo failures are logged; the error returned is the step's.

## 6. Delete

`DeleteSite` = `Teardown` under the site lock. Idempotent: every part is
"remove if there", so a retried or repeated deletion succeeds. The root must
be `<SitesRoot>/<domain>`; nothing else is checked, the site may be half
gone.

1. Routing first: under the nginx lock, remove the vhost, the page-cache
   zone, the blocking rules and the password files (staging's and every protected path's),
   then test and reload nginx.
2. Stop the slice: the PHP master, cron jobs run now and wp-cli runs go
   together (systemd kills the whole cgroup). An unknown slice is fine.
3. The master unit, if its file is there: stop (a no-op after the slice),
   disable, remove unit, drop-in and pool; the slice unit (§3.1);
   daemon-reload when a unit file went, reset-failed.
4. `ops/cron` RemoveSite: runs still going and the schedule.
5. The database and its account (local only).
6. `ops/certs` RemoveSite: the certbot lineage and the operator's
   certificate.
7. `ops/sftp` RemoveShell (R-4.5): the site's open shells (their scopes,
   already ended with the slice), its shell login `cg-shell-<id>` and the
   mask of its uid's user manager. The shell login shares the user's uid
   and has its group as its primary group, so it goes before them.
   Then the user, if it exists. The account is expired (no new login), every
   process whose uid is the user's (real, effective, saved or filesystem)
   is killed and the agent waits for them to exit (bounded, 10 s; zombies
   do not count), then `userdel`. The kill comes first whatever `userdel`
   would say: its own busy check reads `/proc/<pid>/root`, which for
   another uid needs `CAP_SYS_PTRACE`, absent from the agent's sandbox, so
   it skips those processes and deletes a user they still run as (R-3.15).
   Exit 8 (`ErrUserBusy`: one started in between) kills once more and
   `userdel` runs again. No retry loop, no sleep, no `pkill`. The user goes
   before its files, so nothing of it runs while they are removed. Then its
   group, if it exists: `groupdel`. `userdel` keeps a user's own group while
   another member is left, and `www-data` is one in every site's group.
8. Page cache, logs, the files the malware scan moved to quarantine
   (`<Quarantine>/<site id>`, ops/malware SPEC §4), the tree.

Every step runs; the errors are joined and returned
("site cleanup incomplete").

## 7. Apply

Under `LockLiveSite`:

1. A tree with absolute layout links (before R-2.3) is relinked.
2. WordPress: the live release's connector is rewritten with the embedded
   one (a panel upgrade brings connector fixes to existing sites); the
   render reports it as a managed file, once. No `mu-plugins` directory means
   it is not a release the panel built, and nothing is written.
3. Render (§4) with `ReloadOnlyIfChanged` as asked. R-0.7: the panel
   re-applies every PHP site when the number of sites changes; a master whose
   pool, unit and drop-in come out the same keeps running untouched, and
   nginx is not reloaded when no nginx file changed.

## 8. Routing (R-5.4)

The site's routing block (internal/engine SPEC §7).

| Method | Params → Result | Table row |
|---|---|---|
| `SiteRouting` | `SiteRef` → `RoutingView{Default, Current, Before, After}` | Sites, read-only, 1 min |
| `SetSiteRouting` | `RoutingParams{Site, Routing}` → `RoutingResult{Routing, Files}` | Sites, `LockLiveSite`, 5 min |

`SetSiteRouting`: a site without a routing block (a PHP-type site without
PHP) is refused; CRLF becomes LF; empty or the type's default is stored as
`""`; a block failing the rules is refused (`invalid`, the rule's message
with its line). Then the site is rendered (§4) with the block and
`ReloadOnlyIfChanged`: the PHP master is left alone, nginx is tested and
reloaded. A failed `nginx -t` restores every file and reloads nginx on them
(§4.6); the error is `invalid`, "nginx refused the routing, nothing was
changed: <nginx's first error>", its vhost line given as the block's line
when it falls in the block. The result is what to store and the files
written. Every later render (Apply, certificates, staging) renders the
stored block again.

## 9. PHP extensions (#29)

A PHP version's extensions are server-wide: the sites of a version share
its packages and its `conf.d`. Both methods refuse (`invalid`) a version
whose FPM binary is not on the machine (the installed-version rule above),
and the parameters refuse a version that is not `8.x` or an extension
outside `rpc.PHPExtensions`.

- `PHPExtensions` runs `php-fpm<v> -m`, the binary the masters run, so the
  list is what they load (the CLI's own `conf.d` may differ). `modules` is
  every name it prints, lowercase, sorted, without the section headers;
  `extensions` is each common extension with `installed` when any of its
  module names is loaded (`mysql` is `mysqli` or `pdo_mysql`).
- `PHPExtensionInstall` installs only from that list, so a name can never
  choose another package: `apt-get -o DPkg::Lock::Timeout=120 install -y
  -qq --no-install-recommends php<v>-<ext>` with `DEBIAN_FRONTEND=
  noninteractive` (a lock held by unattended-upgrades is waited for, not a
  failure). apt cannot work inside the agent's sandbox (`/usr`, dpkg's
  database and apt's cache are read-only under `ProtectSystem=strict`), so
  it runs as a transient root service, `systemd-run --wait --pipe
  --collect` with `RuntimeMaxSec=720`, which systemd starts outside that
  sandbox; its output and exit status come back through systemd-run. A master loads its extensions when it starts, so every site master
  running that version is then restarted: the agent reads its own unit
  files (`cloudground-php-cg-site-*.service` in the systemd directory) and
  takes those whose `ExecStart` starts `php-fpm<v>`, so it needs no site
  list from the API and restarts exactly the masters on that binary. An
  install that worked with a restart that failed is an error naming the
  units. The answer is the version's
  extensions afterwards.

## 11. Managed apps (#28)

A `node` or `python` site (internal/api SPEC §3.19) has no PHP master; its app is the unit of
internal/runtimes SPEC §3.

### 11.1 Create and render

- **Bootstrap** (§5): the first release gets a small app that answers every path with a page naming
  the site and the runtime's version: Node `server.js` (the `http` module, listening on
  `process.env.PORT`), Python `app.py` (a WSGI `app`) and `requirements.txt` (`gunicorn`). Then
  the dependency step (ops/runtimes SPEC §3) in it.
- **Render**: instead of step 4's PHP master, the unit `cloudground-app-<user>.service` is written
  (0644) in the systemd directory with the site's slice, then daemon-reload when it changed; then
  `enable` and, when the unit changed or without `ReloadOnlyIfChanged`, `restart`, else `start` (a
  no-op on a running app). The vhost's input has `AppUpstream` (internal/engine SPEC §4.11). A
  failure restores the unit and the previous app state like the PHP master's (§4 step 6).
- **Delete** (§6): the unit is stopped, disabled and removed, and so is the environment file.
- `ReloadPHP` and `RestartPHP`, which releases, restores and staging call, restart a managed app's
  unit (it has no reload: an app reads its code when it starts).
- The log viewer's source `app` is `<LogRoot>/<domain>/app.log` (the unit's output).
- Drift covers the unit (`cloudground-app-cg-site-<id>.service`); the environment file is not a
  managed file (it holds secrets and changes with them).

### 11.2 SetAppEnv

Validates the names and values (`runtimes.CheckEnv`) and writes
`/etc/cloudground-app/<user>.env` (the directory root's 0700, the file root's 0600, atomic), then
restarts the app when the content changed and the unit exists. An empty environment writes an
empty file.

### 11.3 AppStatus and AppControl

**AppStatus**: the unit's `ActiveState`, `SubState`, `ActiveEnterTimestamp` (`since`),
`NRestarts` (`restarts`) and `ExecMainStatus` (`last_exit`), from `systemctl show`; then the health
probe: `GET <health_path>` with `Host: <domain>` to the socket (or the port), 5 seconds, from the
agent: `status` the HTTP status (0 when none came), `ms`, `error` the transport's (connection
refused, timeout). An app is healthy when it is active and answers under 500.

**AppControl**: `start`, `stop` or `restart` the unit (another action is `invalid`), then the
status.
