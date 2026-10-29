# phpfpm — a site's PHP master

Every PHP site has its own PHP-FPM master: one systemd unit, one configuration file with one
pool, running as the site's user inside the site's sandbox. This package renders both files and
sizes what goes in them. It writes nothing and runs nothing: `internal/agent/ops/sites` writes the
files, tests them (`php-fpm -t`) and reloads or restarts the unit (that package's SPEC §4.4).

The byte-exact output for a matrix of machines and sites is pinned by `testdata/*.golden`
(`golden_test.go`). Those files are the reference for every value below; a change to any rendered
byte is a deliberate change of the goldens (`CG_GOLDEN_UPDATE=1`) and is listed in its commit.

## 1. Why one master per site

- OPcache and APCu live in the master's shared memory. One master shared by several sites would let
  any of them read and overwrite another's cached objects, and a per-site cache size could not be
  set (both are master-wide settings).
- The tenant boundary is the unit's mount namespace (§4), not `open_basedir`. On the test box
  `open_basedir` cost 28 % of uncached throughput (6.16 req/s with a shared master and
  `open_basedir`, 7.86 req/s per-site without).

## 2. Names and paths

| What | Value |
| --- | --- |
| `LogRoot` (variable) | `/var/log/cloudground`; a site's PHP error log is `<LogRoot>/<domain>/php-error.log`, in a root-owned tree so logrotate (root) never works inside a directory the site controls |
| `MasterConfDir` (variable) | `/etc/cloudground-php`; the configuration is `<MasterConfDir>/<system user>.conf` |
| `UnitName(user)` | `cloudground-php-<user>.service` |
| `SocketFor(user)` | `/run/cloudground/php/<user>/php.sock` (the unit's runtime directory) |
| pid file | `/run/cloudground/php/<user>/fpm.pid` |

## 3. Sizing

### 3.1 Workers — `SizeWorkers(budgetMB, requested) Workers`

`CPUCount` (variable, default `runtime.NumCPU()`) is the CPU count workers are capped by.

1. `max` = `requested` when `requested > 0`. Otherwise `budgetMB / 80` (80 MB: a WordPress worker's
   resident size is about 64–96 MB), capped at `3 × CPUCount` when that is positive. The uncached
   path is CPU-bound: measured upstream on 2 vCPU, 6 workers 5.54 req/s, 10 → 4.29, 16 → 4.24.
2. `max` is clamped to 2…64 (also an explicit request).
3. `StartServe = ceil(max/4)`, `MinSpare = ceil(max/8)`, `MaxSpare = ceil(max/2)`, each at least 1,
   and `MaxSpare` at least `StartServe`. `MaxRequests = 1000`.

### 3.2 Caches — `SizeSiteCaches(type) SiteCaches`

Master-wide settings, so they go on the master's command line (§4), never in the pool
(`php_admin_value` cannot set them).

| | OPcache MB | interned strings MB | max files | JIT buffer MB | APCu MB |
| --- | --- | --- | --- | --- | --- |
| WooCommerce | 128 | 16 | 20000 | 32 | 64 |
| any other type | 64 | 8 | 10000 | 32 | 32 |

### 3.3 Per-request limits (pool)

| | default | WooCommerce default | override (when > 0) |
| --- | --- | --- | --- |
| `memory_limit` | 256M | 512M | `Config.PHP.MemoryLimitMB` |
| `upload_max_filesize` = `post_max_size` | 64M | 64M | `Config.PHP.UploadMaxMB` |
| `max_execution_time` | 120 | 120 | `Config.PHP.MaxExecutionSeconds` |
| `request_terminate_timeout` | `Config.PHP.RequestTimeoutSeconds()` s (max_execution + 10, at least 120) | | |

`max_execution_time` counts CPU time only; `request_terminate_timeout` is the wall-clock kill for a
request waiting on I/O. nginx's `fastcgi_read_timeout` uses the same value.

### 3.4 Process manager

- `ondemand` (no idle workers; `pm.process_idle_timeout = 30s`) for a **Balanced** site that is not
  WooCommerce: a server of many quiet sites does not keep hundreds of MB of idle workers.
- `dynamic` for every other site (stores, Commerce and Maximum profiles keep warm workers):
  `start_servers`, `min_spare_servers`, `max_spare_servers` from §3.1.
- Both: `pm.max_children`, `pm.max_requests`, `pm.status_path = /__cloudground/fpm-status`.

## 4. The unit — `RenderUnit(site, phpBinDir) (name, content, error)`

Refused (error, nothing rendered) without a system user, a PHP version or a root path.

- `Type=notify`, `User=`/`Group=` the site user, `RuntimeDirectory=cloudground/php/<user>` mode 0750.
- `ExecStart=<phpBinDir>/php-fpm<version> --nodaemonize --fpm-config <conf>` followed by the §3.2
  sizes as `-d opcache.memory_consumption=<MB>`, `-d opcache.interned_strings_buffer=<MB>`,
  `-d opcache.max_accelerated_files=<n>`, `-d opcache.jit=tracing`,
  `-d opcache.jit_buffer_size=<MB>M`, `-d apc.shm_size=<MB>M`.
- **The JIT mode is on the command line, never in the pool.** PHP reads `opcache.jit` once, when
  the master starts; a pool's `php_admin_value[opcache.jit]`, where it was until v0.2.0, is
  ignored, and `opcache_get_status()` reported the JIT off on every site. Measured on the 2 vCPU
  test box, uncached WooCommerce shop page, four alternated rounds of 60 s: 8.00 req/s without the
  JIT, 8.26 with it (+3.2 %, ahead in three rounds of four; `bench/uncached-php.md`).
- `ExecReload=/bin/kill -USR2 $MAINPID` (a graceful reload: §5.2), `Restart=on-failure`,
  `RestartSec=2`, `LimitNOFILE=65536`, `OOMPolicy=continue`: a worker the kernel kills at the site's
  memory limit (its slice's `MemoryMax`, ops/sites SPEC §3.1) ends that request only; systemd's
  default (`stop`) would stop the whole master and every request of the site with it.
- The sandbox: every line of `Sandbox(site)` (§4.1), under a comment that names the boundary,
  then `MasterHardening` (§4.1).
- `WantedBy=multi-user.target`.

### 4.1 `Sandbox(site)`

The confinement of everything long-lived a site runs: its PHP master, its SSH shell and its
wp-cli/cron runs take their properties from this one list, so they cannot drift apart. Its lines
are part of the goldens (`sandbox.golden`) and must stay byte-identical: the shell, wp-cli and cron
pass them to `systemd-run -p`.

`PrivateDevices=yes` gives a private `/dev` but binds the host's `/dev/shm` into it, a directory
every site can write and read (mode 1777). So `Sandbox` mounts a tmpfs of the unit's own over it
(`TemporaryFileSystem=/dev/shm:size=64M,mode=1777,nosuid,nodev,noexec`, `ShmSize`): what a site
leaves there no other site and not the host sees, and it can pin 64 MiB of memory at most. PHP
does not rely on it (APCu and OPcache map anonymous memory, sessions are in `<root>/tmp`), but
POSIX semaphores and `shm_open` need it, so it stays writable. The master, the shell and each run
get separate instances.

`NoPortBinds` (`SocketBindDeny=1-65535`) keeps a site's code from binding any TCP or UDP port, IPv4 or
IPv6 (F-6-2). nginx proxies to an app's loopback port, and WordPress sends its connector token to
`127.0.0.1:9080`. Either port, left free while its owner restarts, could otherwise be taken by
another site's process. The rule names ports 1–65535, not `any`: systemd 259's `any` also refuses
`bind()` to port 0, which a client that binds before it sends needs, while a port the kernel picks
from the ephemeral range is nobody's. `connect()`, unix sockets (PHP-FPM's `listen`, the shell and
mail gates) and outgoing connections are not concerned. A managed app in TCP-port mode adds
`AllowPortBind(port)` (`SocketBindAllow=tcp:<port>`), and an allow wins over the deny (internal/runtimes
SPEC §3). systemd enforces the rule with a cgroup BPF program, which it loads through `libbpf.so.1`
(`libbpf1`, internal/syspkg). Ubuntu 26.04's systemd 259 has the BPF framework. Ubuntu 24.04's 255
is built without it (`-BPF_FRAMEWORK` in `systemd --version`) and ignores the line. A systemd
that started before libbpf was installed ignores it too, until it is re-executed, so the installer
and the agent's package step run `systemctl daemon-reexec` after an install that brought `libbpf1`,
and only then (`syspkg.NeedsReexec`). Running units and jobs survive it. The update guard's restart of
the agent, which waits on that very start, is enqueued again once (host SPEC: an idempotent job
whose connection is lost). Where the line is ignored, the unit still starts and the rest of the
sandbox stands, but the ports are not held (docs/security-model.md, "Limits, stated plainly"). The
e2e asserts the rule wherever `systemctl --version` says 249 or later with `+BPF_FRAMEWORK`, so a
bind that works there is a failure, and skips with the reason only where it does not.

`RunSandbox(site)` is `Sandbox(site)` plus `RunHardening` (R-10.5): `RestrictSUIDSGID=yes`,
`PrivateIPC=yes`, `SystemCallFilter=@system-service`, `SystemCallErrorNumber=EPERM`
(`LockPersonality=yes` is in `Sandbox` already). Every run of a site off the request path takes it: the SSH shell, cron jobs
(scheduled and run now) and every wp-cli call (installs, updates, the staging search-replace, the
warmer's and the object cache's). `RemoveIPC` is not set: it would remove the IPC objects of the
uid, which is also the PHP master's.

The master's unit takes `Sandbox` plus `MasterHardening`: `PrivateIPC=yes`, an IPC namespace of
its own, so no other site's PHP can attach to its SysV shared memory, semaphores or POSIX message
queues (it shared the host's until v0.2.0). Measured on the 2 vCPU box, uncached WooCommerce shop
page, three alternated rounds of 60 s: 8.15 req/s without, 8.22 with (+0.8 %, inside the spread);
APCu, OPcache and the JIT work unchanged, since they map anonymous memory
(`bench/uncached-php.md`). `RestrictSUIDSGID` and the system-call filter stay off the master
until they are measured there (AGENTS.md rules 5 and 6).

## 5. The pool — `RenderPool(site, budgetMB) (path, content, error)`

Refused without a system user or a PHP version. `budgetMB <= 0` means 1024. The file has a
`[global]` section (the master) and one pool named after the site user.

### 5.1 `[global]`

pid file (§2), `error_log = syslog`, `syslog.ident = php-fpm-<user>`, `daemonize = no`,
`emergency_restart_threshold = 10` within `emergency_restart_interval = 1m`, and
`process_control_timeout` (§5.2).

### 5.2 Reloads and running requests

A reload (`SIGUSR2`) asks every worker to finish its request and exit, then the master re-executes
itself on the new configuration; workers still busy after `process_control_timeout` are killed.

`process_control_timeout` = min(`request_terminate_timeout` (§3.3), 30 s) (`reloadDrainCapSec`):
120 s → `30s` by default, and never more than 30 s.

Two costs pull in opposite directions:

- **Running requests.** A reload kills every request still running when the timeout ends, and the
  client gets a 502. An import, an export or a payment callback killed halfway is not recoverable.
  Until R-5.2 the timeout was a fixed 10 s, so any reload killed every request running longer than
  10 s. Reloads come from a site-count reconcile that resizes the pool, a deploy, a cache refresh
  or a PHP setting change.
- **Every other request of the site.** During a reload the master is RELOADING and spawns no
  worker, so all the site's uncached requests (cart, checkout, wp-admin, a fast page) wait in the
  socket backlog (§5.3) until the slowest running request ends or the timeout kills it. nginx's
  `fastcgi_read_timeout` (also `request_terminate_timeout`) counts that wait, so letting the drain
  last the full request limit (up to 610 s) would freeze the site on every reload and turn long
  waits into 504s. Page-cache hits never reach PHP and are not delayed.

The 30 s cap keeps the freeze short (the e2e measures it: a concurrent uncached request during a
reload answers within 35 s) and lets most requests finish (anything under 30 s survives a
reload, three times more than before). A request longer than 30 s that is running at a reload is
still ended. The site-count reconcile reloads only masters whose pool changed (ops/sites
SPEC §4.4, R-0.7), so that is rare. This setting affects reloads only: no steady-state request
path changes.

### 5.3 The pool

- `listen` = the socket (§2), `listen.mode = 0660` (nginx reaches it through the site group),
  `listen.backlog = 65535` (the default 511 drops connections in a burst, which nginx reports as
  502 rather than queueing them), `rlimit_files = 65536`.
- The process manager (§3.4), `request_terminate_timeout` (§3.3), `catch_workers_output = yes`.
- Temporary files, uploads and sessions in the site's own `<root>/tmp` (`upload_tmp_dir`,
  `sys_temp_dir`, `session.save_path = <root>/tmp/sessions`); the error log (§2), `log_errors` on,
  `display_errors` off; `disable_functions = exec,passthru,shell_exec,system,proc_open,popen`.
- The limits of §3.3.
- On a WordPress or WooCommerce site with two-factor sign-in (`config.wp_two_factor` `optional` or
  `required`, #74), `env[CLOUDGROUND_2FA] = <mode>`, which the connector reads
  (connector/SPEC.md §2.11); nothing otherwise. In the pool, root's, rather than in
  `wp-config.php`, which the site can edit: a site's own code still runs in the site, but turning
  the setting off takes the panel.
- On a WordPress or WooCommerce site with hardening (`config.wp_hardening`, #75):
  `env[CLOUDGROUND_HARDENING]` = the connector's items that are on, comma-separated, in the order
  `xmlrpc`, `file_edit`, `version` (the readme item is nginx's alone), and
  `env[CLOUDGROUND_LOGIN_PATH]` = the login path when it is one (`^[a-z0-9][a-z0-9-]{2,63}$`);
  nothing when neither applies.
- Mail (#77, internal/api SPEC §14.2): on a site that is not a staging copy (`StagingOf == 0`)
  and whose mail is not off (`config.mail.off`), `php_admin_value[sendmail_path] =
  /usr/local/bin/panel-agent sendmail -t -i`, the client of the panel's mail gate, and, when
  the site names a sender (`config.mail.from`, a plain address), `env[CLOUDGROUND_MAIL_FROM]`
  for the connector (connector/SPEC.md §2.13); a staging copy, or a site with mail off, has
  neither, so its `mail()` fails as it did before.
- APCu on (`apc.enabled`), sized on the command line (§4). OPcache on, `validate_timestamps = 1`,
  `revalidate_freq = 60` (the JIT mode is on the command line, §4). `apc.enable_cli` and
  `opcache.enable_cli` off.
- `zlib.output_compression` on at level 5: PHP compresses its own output, so the page cache stores
  a compressed body and serves hits without compressing again.
- **Not enabled, on purpose:** OPcache preload (measured on a config-cached Laravel app: a gain of
  about 3 ms, within noise, against a master that fails to start when the preload file is missing
  and a file to regenerate on every deploy); `open_basedir` (§1).
