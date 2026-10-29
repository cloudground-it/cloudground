# install — `cgctl install`, `cgctl install --plan`, `cgctl doctor`

The installer takes a fresh Ubuntu 24.04 or 26.04 (amd64) server to a running CloudGround, and on a
server that already runs CloudGround it is the upgrade: the same steps, each changing only what is
not already as it should be. It replaces the 715-line `installer/install.sh` (R-6.1). What that
script did, step by step, is written down here first; the Go code follows this text, and the
golden test (§7) holds the rendered files to the bytes the bash script wrote.

`installer/install.sh` is now a bootstrap of about 40 lines (§6). Everything else is here.

## 1. Model

A **step** has a stable ID (`nginx.vhost`, `packages.main`, …), a one-line title, and two
operations:

- `Check(ctx) (needed bool, why string)` looks at the machine and changes nothing. `why` says what
  is missing or different (`"/etc/sysctl.d/60-cloudground.conf: content differs"`), or, when
  nothing is needed, why the step has nothing to do (`"systemd is not running: skipped"`). A check
  that cannot look (a command fails) answers needed, with the error as the reason.
- `Apply(ctx) error` makes the machine match. It is only called when `Check` said needed, and
  running it twice is the same as running it once.

The steps run in the order of §3, which is install.sh's order. Three entry points share them:

| Command | What it does | Exit |
| --- | --- | --- |
| `cgctl install --plan` | preflight (§2), then every `Check`, printing `+ <id>  <why>` for each needed step and `= <id>` for the others. Nothing is changed. | 0, or 1 when preflight fails |
| `cgctl install` | preflight, then for each step `Check` and, when needed, `Apply`. The first `Apply` that fails stops the run with its error. | 0 when every step is done |
| `cgctl doctor [--skip-packages] [--container]` | every `Check` (no preflight beyond the distribution, no binaries step), printing the same lines; a needed step is drift | 0 with no drift, 1 with drift |

Options of `cgctl install`:

- `--from <dir>`: install the binaries from a local build (`panel-api`, `panel-agent`, `cgctl` in
  `<dir>`), unverified, as `CLOUDGROUND_LOCAL_BUILD` did.
- `--release <dir>`: install the binaries from a downloaded release in `<dir>`, verified (§5).
- `--remove-release-dir` (with `--release`): remove `<dir>` when the run ends, however it ends
  (success, failure, interrupt). The bootstrap passes it for its own download directory.
- With neither, the binaries step has nothing to do (re-converging an installed server).
- `--skip-packages`: the apt, PPA, restic, wp-cli and composer steps answer "skipped". For a
  container or a box without network.
- `--container`: preflight checks only that this is a supported Ubuntu (no root/arch/RAM/disk/port
  checks), and every step that needs systemd, the kernel (sysctl) or ufw answers "skipped".
  Implied, for the systemd steps alone, whenever systemd is not PID 1 (`/run/systemd/system`
  missing).

Output: as before, everything the commands print goes to `/var/log/cloudground-install.log`
(`/tmp/cloudground-install.log` when that cannot be opened; opened `O_NOFOLLOW`, so a link planted
at the name is not written through), except the setup link's journal read (step 30), whose output
is a credential and is never logged; the terminal shows one line per step
with its duration (a spinner on a TTY), the reason on failure followed by the log's last 12 lines,
and at the end the setup URL (§3, step 30). `--plan` and `doctor` print to stdout only.

The environment the steps see is an `Env`: a filesystem root (`/`, or a temporary directory in
tests), a command runner, a package manager (apt), systemd (systemctl), accounts (user and group
lookup, useradd, groupadd), a downloader (HTTPS only, no redirects to plain HTTP) and a clock. Each
has a fake in `fakes_test.go`; the unit tests run every step against them.

## 2. Preflight

Every problem is collected and all are reported together (one run to fix them all), as before:

- not root → stop at once: `run as root: sudo bash installer/install.sh`;
- `/etc/os-release`: `ubuntu-24.04` → PHP **8.4** from `ppa:ondrej/php` (24.04 ships 8.3);
  `ubuntu-26.04` → PHP **8.5**, native; anything else → `Ubuntu 24.04 or 26.04 LTS required (found
  <PRETTY_NAME>)`; no file → `cannot detect the OS (/etc/os-release missing)`;
- architecture not `x86_64` → `amd64 required (found <arch>)`;
- `MemTotal` below **900 MB** → `at least 1 GB of RAM required (found <n> MB)` (a "1 GB" instance
  reports 950–990 MB; 512 MB must fail);
- free space on `/` below **10000 MB** → `at least 10 GB free disk required (found <n> MB)`;
- **upgrade** = `/etc/cloudground/agent.token` exists and `cloudground-agent` is enabled or
  active. **Changed:** it was "active" only, so on a server whose agent failed at every start (the
  `status=226/NAMESPACE` loop of step 23) the re-run meant to repair it was refused for the ports
  its own nginx holds. On a
  fresh install, a listener on TCP 80 or 443 → `port <p> is already in use (<process>) —
  CloudGround must own the web ports`. (That is also what stops an install over another web
  control panel: it holds those ports. The installer does not look for other products by name;
  **changed (R-10.2)**: the old check for one such binary on `PATH` is gone.)

## 3. Steps, in order

`<php>` is the PHP version from preflight. "Owner unchanged" means: an existing directory keeps its
owner, a new one is created root-owned (what `install -d -m` does).

### Packages (`--skip-packages` skips 1–8)

1. **`packages.base`** — check: `dpkg-query` says each of `software-properties-common curl gnupg
   ca-certificates bzip2` is `install ok installed`. Apply: `apt-get update -qq`, then `apt-get
   install -y -qq` of the missing ones (`DEBIAN_FRONTEND=noninteractive`).
2. **`packages.ppa`** (24.04 only) — check: a file under `/etc/apt/sources.list.d/` whose name
   contains `ondrej` and `php`. Apply: `add-apt-repository -y ppa:ondrej/php` (a failure is
   ignored, as before: the install step then fails on the missing PHP packages with apt's own
   message), then `apt-get update -qq`.
3. **`packages.main`** — check: each package installed (`dpkg-query` status `install ok
   installed`, or `hold ok installed`: a package the operator put on hold is installed). Apply:
   `apt-get update -qq`, `apt-get install -y -qq` of the missing ones only. The list (and
   `packages.base`'s) is `internal/syspkg`'s, which the agent also reads when it starts, so a
   self-update installs a package a newer release added (#113, ops/panel SPEC §2.6). For a new
   install:

       nginx mariadb-server certbot python3-certbot-dns-cloudflare git unzip cron nftables libbpf1
       php<php>-fpm php<php>-mysql php<php>-curl php<php>-gd php<php>-intl
       php<php>-mbstring php<php>-soap php<php>-xml php<php>-zip php<php>-imagick
       php<php>-bcmath php<php>-apcu

   **Changed (R-6.1): `redis-server` and `php<php>-redis` are no longer installed.** Redis was
   installed and never enabled, and nothing ever turned it on: install.sh's comment said it stayed
   off "until a site enables object caching", but the object cache is APCu only (`connector/`)
   and no site setting starts Redis. On an upgraded server they are left installed, and step 5 keeps Redis off.

   **Changed (R-6.1): only missing packages are installed.** install.sh ran `apt-get install` on
   the whole list every time, which also upgraded the installed ones on every re-run. Upgrades of
   distribution packages now come from the system's own `unattended-upgrades` (security updates,
   on by default on Ubuntu) or the operator's `apt upgrade`, never from re-running the installer.
4. **`php.distro-fpm-off`** — every site runs its own FPM master (`cloudground-php-<user>.service`,
   `internal/phpfpm`) from the `php-fpm<php>` binary; the distribution's `php<php>-fpm` service and
   its `www` pool serve nothing. Check: `php<php>-fpm.service` exists and is enabled or active.
   Apply: `systemctl disable --now php<php>-fpm`; a failure is a warning in the log, not fatal
   (as before).
5. **`redis.off`** — check: `redis-server.service` exists and is enabled or active. Apply:
   `systemctl disable --now redis-server` (a failure is a warning, as before). On a new install
   the unit does not exist, so the step has nothing to do.
6. **`tools.restic`** — restic **0.19.1**, pinned (R-4.6): 24.04 ships 0.16, which exits 1 for every
   failure; 0.17+ have typed exit codes (locked, wrong password, no repository) that the agent turns
   into messages. Check: `/usr/local/bin/restic version` prints `0.19.1` as its second field.
   Apply: download
   `https://github.com/restic/restic/releases/download/v0.19.1/restic_0.19.1_linux_amd64.bz2`,
   require SHA-256 `f415415624dcc452f2a02b8c33641791a8c6d6d3b65bbb3543fcf9a25151585c` (from the
   release's SHA256SUMS, whose signature by restic's key `CF8F 18F2 8445 7597 3F79 D4E1 91A6 868B
   D3F7 A907` was checked when it was pinned), decompress (bzip2), install `0755` as
   `/usr/local/bin/restic` (ahead of `/usr/bin` on `PATH`).
7. **`tools.wp-cli`** — **Changed (R-6.1): pinned by hash.** Before, the latest phar from
   `wp-cli/builds` gh-pages was checked against the SHA-512 fetched beside it, and the step was
   skipped whenever any `wp` was on `PATH`. Now: wp-cli **2.12.0**,
   `https://github.com/wp-cli/wp-cli/releases/download/v2.12.0/wp-cli-2.12.0.phar`, SHA-256
   `ce34ddd838f7351d6759068d09793f26755463b4a4610a5a5c0a97b68220d85c` (2026-10-10: the same bytes
   as the gh-pages phar the old installer fetched, and matching the project's published `.sha256`
   and `.sha512`). Check: `/usr/local/bin/wp` has that hash. Apply: download, compare, install
   `0755`.
8. **`tools.composer`** — composer **2.10.3** for the site shells (R-4.5), the phar rather than
   Ubuntu's package (which pulls in the distribution's PHP CLI):
   `https://getcomposer.org/download/2.10.3/composer.phar`, SHA-256
   `7a2d379d5b8ffdaa028580ef26494c36d2feef4b178d3dd1473a4dbc5e17c8d6` (the same bytes from
   getcomposer.org and the GitHub release, 2026-10-10), never a sum fetched beside the file. Check:
   `/usr/local/bin/composer` has that hash (before: any `composer` on `PATH`). Apply: download,
   compare, install `0755`.

### Users, directories, secrets

9. **`user.cloudground`** — check: user `cloudground` exists. Apply: `useradd --system
   --no-create-home --shell /usr/sbin/nologin cloudground`.
10. **`dirs`** — check: each directory exists with its mode (and owner where named); a symlink at
    the name is drift. Apply: create or fix. A symlink at the name is refused (the step fails), and
    the mode is set through a descriptor opened `O_NOFOLLOW|O_DIRECTORY` (owner with `lchown`):
    nothing is ever chmod-ed or chown-ed through a link.

    | Directory | Mode | Owner |
    | --- | --- | --- |
    | `/srv/sites`, `/var/log/cloudground`, `/var/cache/cloudground` | 0755 | unchanged |
    | `/srv/functions` (the functions' code, internal/functions SPEC §2: traversed, never listed; the agent's unit also makes it, `ExecStartPre=+install -d`, so a server updated from a release without it gets it, and names it `-/srv/functions` in `ReadWritePaths`) | 0711 | root:root |
    | `/var/lib/cloudground` (the API's state) | 0750 | cloudground:cloudground |
    | `/var/www/cloudground-acme`, `/run/cloudground`, `/run/cloudground/php` | 0755 | unchanged |
    | `/etc/cloudground-php` (each site's PHP master configuration, read by the site's user) | 0755 | root:root |
    | `/var/lib/cloudground-agent`, `/var/lib/cloudground-agent/work` | 0700 | root:root |
    | `/etc/cloudground`, `/etc/cloudground/certs` | 0750 | group cloudground |
    | `/var/lib/letsencrypt` (certbot's work directory), `/var/spool/cron` | 0755 | unchanged |

    Every path the units name in `ReadWritePaths=`, `ReadOnlyPaths=`, `BindPaths=`,
    `BindReadOnlyPaths=`, `InaccessiblePaths=` or `TemporaryFileSystem=` must exist before the unit
    starts: systemd fails it at the NAMESPACE step otherwise (`status=226/NAMESPACE`), and with
    `Restart=on-failure` the agent loops for ever. Measured on a freshly imaged Ubuntu 26.04: the
    agent's `/var/lib/letsencrypt` exists only after certbot's first run, and `/var/spool/cron`
    only where cron is installed (the installer does not install it; a container image has
    neither). So the table creates them, with the modes certbot (`0755`, its `CONFIG_DIRS_MODE`)
    and cron's package give them. The `-` prefix ("ignore if missing") is not the fix for a path
    the agent writes: under `ProtectSystem=strict` a path left out of the namespace is read-only,
    so certbot, run by the agent, could not create `/var/lib/letsencrypt` later and every
    certificate request would fail. `-` is right only for a path that may legitimately never exist
    (`/var/backups`, `/mnt`, the sandboxes' `InaccessiblePaths=`). A unit test
    (`unitpaths_test.go`) holds every absolute path of the embedded units, and the fixed paths of
    the site sandbox (`internal/phpfpm.Sandbox`) and the shell's (`ops/sftp.ShellQuiet`), to: in
    this table, prefixed `-`, or one of `/etc`, `/run`, `/var/log`, `/usr/local/bin`,
    which every Ubuntu image has. `/etc/letsencrypt` needs no entry: the agent's unit names `/etc`,
    and step 28 creates it.

    The agent's scratch space is root's alone: `/var/lib/cloudground` belongs to the API user, who
    could otherwise swap a directory in it for a symlink. panel-api traverses `/etc/cloudground` to
    read the agent token and the panel certificate.
11. **`panel.env`** — `/etc/cloudground/panel.env` has exactly one `CLOUDGROUND_PHP_VERSION=<php>`
    line, the file is `root:cloudground 0640`. Line writer: an existing `CLOUDGROUND_PHP_VERSION=`
    line is replaced in place (every one, as `sed s///` did), else the line is appended (after a
    newline if the file does not end in one); other lines are kept byte for byte.
12. **`token.agent`** — check: `/etc/cloudground/agent.token` exists, is a regular file (not
    followed), mode `0640` and `root:cloudground`. Apply: when missing, 48 characters from
    `[A-Za-z0-9]` (crypto/rand; before: base64 of urandom with `/+=` removed, the same alphabet),
    no newline, written `0600` and then `root:cloudground 0640`. Its content is never rewritten
    once present (the API and the agent share it), but a looser mode or another owner is put back
    (H-3-2: before, only its existence was checked, and a token readable by every account of the
    box handed them the agent); one that is not a regular file (a link) fails the step, naming
    the file to remove.
13. **`cert.panel`** — `/etc/cloudground/certs/panel.{pem,key}`: self-signed, RSA 2048, 3650
    days, subject `CN=cloudground-panel`, key PKCS#8 PEM. Check: `panel.pem` exists. Apply: write
    both, `root:cloudground`, `panel.pem 0600`, `panel.key 0640`. (Before: openssl under umask 077;
    the modes are the ones that produced.) Replaced later by the domain's certificate from
    Settings (`internal/agent/ops/panel`).
14. **`cert.panel-ip`** — `panel-ip.{pem,key}`: the same, kept for the panel port, so the panel
    stays reachable by IP after `panel.pem` becomes a Let's Encrypt certificate. `root:root`,
    `panel-ip.pem 0600`, `panel-ip.key 0640`.
15. **`cert.fallback`** — `fallback.{pem,key}`, `CN=fallback.invalid`: what a vhost serves before
    its own certificate. `fallback.pem 0644`, `fallback.key 0640` group `www-data` (when the group
    exists).

### Binaries

16. **`binaries`** — `panel-api`, `panel-agent`, `cgctl`, and `cg-site-shell` (the shell gate's
    client, R-10.5) when the build has it, into `/usr/local/bin`, `0755`; `cg-site-shell` replaces
    the link to `panel-agent` an older install left at that name (the rename replaces the link
    itself, never its target). Check: the
    installed file's SHA-256 equals the source's. Apply: copy to a random `O_EXCL` temporary name
    in `/usr/local/bin`, then rename (a running binary is never written in place), then hash what
    landed and fail unless it is the source's bytes. The source is `--from` (unverified local
    build) or, for `--release`, the root-only directory the verified bytes were copied into (§5),
    never the operator's directory itself. No source: nothing to do.

### Kernel and nginx

These live outside the panel's managed nginx config because they are main/events level
(`worker_connections` cannot be set from an http-level include) or kernel level. Everything
http-level is rendered by the agent into `conf.d` (`internal/engine/nginx`).

17. **`sysctl`** — `/etc/sysctl.d/60-cloudground.conf` (template `templates/sysctl.conf`), `0644`.
    Check: content, and (outside a container) the running kernel: each `key = value` of the file
    against `/proc/sys/<key with / for .>` (whitespace-normalised; a key the kernel lacks is not
    compared), so `doctor` reports a value something changed at run time. Apply: write the file
    if it differs, then `sysctl -p <file>` — also when only the live values differed (after a
    self-update the new agent does the same at its start: ops/panel SPEC §2.7); a failure is
    a warning (a kernel that refuses a value must not stop the install), and doctor keeps
    reporting the difference. Values: `net.core.somaxconn = 65535`, `net.ipv4.tcp_max_syn_backlog
    = 65535`, `net.core.netdev_max_backlog = 16384`, `net.ipv4.tcp_tw_reuse = 1`,
    `net.ipv4.tcp_slow_start_after_idle = 0`, `net.ipv4.ip_local_port_range = 32768 65535` (the ports below stay with
    services, so a managed app's TCP port can lie outside the range: internal/api SPEC, `PUT
    /api/sites/{id}/app`),
    `fs.file-max = 500000`, and, pinned rather than left to the distribution (SR-T8),
    `fs.protected_hardlinks = 1` and `fs.protected_symlinks = 1`: a symlink in a sticky
    world-writable directory is followed only by its owner (or the directory's), a hard link
    is made only to a file the linker owns or can write. Doctor compares their live values as
    for every other key.
18. **`nginx.tuning`** — `/etc/nginx/nginx.conf`, a line editor on the distribution's file. Check:
    the file contains `cloudground-tuned`. Apply:
    1. copy the file to `nginx.conf.cloudground-pre` (content, mode, owner and times, as `cp -a`);
       every write here goes through a random `O_EXCL` temporary name and a rename;
    2. every line matching `^(\s*)worker_connections\s+[0-9]+;` has that part replaced by
       `\1worker_connections 4096;` (the rest of the line kept);
    3. before the first line that starts with `worker_processes`, insert `# cloudground-tuned` and
       `worker_rlimit_nofile 65535;` (no such line: nothing inserted, as with sed);
    4. `nginx -t`; on failure put the backup back, and if `nginx -t` still fails, fail the step
       (`nginx config is broken and the backup did not restore it`).

    Ubuntu ships `worker_connections 768`, so two workers capped the box at ~1536 connections,
    reached well before the cache runs out.
19. **`nginx.default-site`** — check: `/etc/nginx/sites-enabled/default` exists. Apply: remove it.
20. **`nginx.vhost`** — `/etc/nginx/sites-enabled/cloudground-panel-bootstrap.conf` (template
    `templates/panel-bootstrap.conf`), `0644`: the default servers on 80 (ACME webroot, else 444)
    and 443 (fallback certificate, 444), and the panel's own port **8443** with the panel-ip
    certificate, `error_page 497 =301 https://$http_host$request_uri`, `include
    /etc/cloudground/panel-access.conf*`, `client_max_body_size 16m` (the API's per-file limit;
    nginx's 1 MB default refused larger uploads with 413, measured) and the proxy to
    `unix:/run/cloudground/api.sock` (buffering off, 1 h read timeout, `Host $http_host`,
    `X-Real-IP`, `X-Forwarded-For`, `X-Forwarded-Proto https`). When `nginx -V` (stderr) names
    `http_v3_module`, the 443 default server also has `listen 443 quic reuseport default_server;`
    and `listen [::]:443 quic reuseport default_server;` (the one `reuseport` a QUIC address may
    carry is the default server's). Check: content. Apply: write, `nginx -t` (failure fails the
    step).

### Services

21. **`mariadb`** — check: `mariadb` enabled and active. Apply: `systemctl enable --now mariadb`.
    The installer writes **no MariaDB tuning**: the agent renders `internal/tune`'s configuration
    when the first database is created (`internal/agent/ops/db`, `Tune`), from the probed RAM and
    CPUs, and re-tunes on demand. That is unchanged.
22. **`units`** — the three unit files, embedded in cgctl (`internal/units`), to `/etc/systemd/system/`,
    `0644`: `cloudground-agent.service`, `cloudground-api.service`, `cloudground-api.socket`. Check:
    content. Apply: write, `systemctl daemon-reload`. Before, they were copied from the source tree
    (`installer/systemd/`) or from the release download; now cgctl carries them, and a verified
    cgctl is what makes them verified. The release still publishes them for self-update
    (`internal/agent/ops/panel`). **Upgrade note:** the embedded units have the same directives
    as `installer/systemd/`, but their comments were rewritten, so the first `cgctl install` on a
    server installed by the old script rewrites them, and step 23 restarts the agent and the API
    once (a few seconds; open requests and event streams are dropped). Later runs do not.
23. **`services`** — a re-run applies what it rewrote. Check: needed when any of
    `cloudground-api.socket`, `cloudground-agent`, `cloudground-api`, `nginx` is not enabled and
    active; or when this run changed what the panel reads at start (`binaries`, `units`,
    `panel.env`, `token.agent`) while the agent or the API is running; or when it changed what
    nginx serves (`nginx.tuning`, `nginx.default-site`, `nginx.vhost`, `cert.panel`,
    `cert.panel-ip`, `cert.fallback`). (The API reloads `panel.pem` itself, `cmd/panel-api`
    tlsreload; nginx serves it for the panel domain, hence the reload.) Apply: `systemctl enable
    --now` the four; then, for the panel, `systemctl stop cloudground-api` (failure ignored),
    `restart cloudground-api.socket`, `restart cloudground-agent cloudground-api` — decided before
    `enable --now`, so a panel that was not running just starts on the new files; then, for
    nginx, `systemctl reload nginx` (else `restart`; failures ignored): every nginx change was
    tested with `nginx -t` by the step that made it, and on a fresh install apt started nginx
    before the vhost existed, so `enable --now` alone never bound :443. Then wait up to 10 s for a
    TCP listener on 443; none → fail (`nginx is not listening on 443 — the panel would be
    unreachable`). Then wait up to 30 s, checking once a second, for the panel to be up:
    `cloudground-agent` and `cloudground-api` both `active`, and `GET /healthz` over
    `/run/cloudground/api.sock` answering 200 (3 s per try; the API answers 200 only when its ping
    to the agent over `/run/cloudground/agent.sock` succeeds). Not up by then → the step fails with
    `the panel did not come up within 30s: <unit> is not active` (or `/healthz: <why>`), followed by
    the last 15 lines of `journalctl -u <unit> -o cat` of the unit that is not active (both
    units' when `/healthz` failed), so the terminal shows why, e.g. `Failed at step NAMESPACE`.
    The journal is read quietly and a line carrying the setup link (step 30) is replaced by a
    placeholder. `systemctl start` of a `Type=simple` unit returns as soon as it forks, so without
    this wait a unit that fails at exec (`status=226/NAMESPACE`) and restarts for ever left the
    installer printing "CloudGround is running" (measured on a reimaged 26.04). A start job that
    fails outright (the agent's `ExecStartPre=panel-agent ensure-packages` runs in its sandbox, so
    the same NAMESPACE failure fails the job) makes `enable --now` or the restart exit non-zero
    before the wait: the error is systemctl's, then the panel unit that is not active and its
    journal, the same 15 lines. The same wait runs
    on every apply of the step, so on an upgrade it follows the restart; and the check is needed
    when, with all four units enabled and active, the panel still does not answer (`the panel does
    not answer: …`, a single `/healthz` try), so `cgctl doctor` reports a dead panel and a re-run
    tries again and fails the same way. A re-run that changed nothing restarts and reloads nothing. **Changed
    (R-6.1):** install.sh restarted the panel on every upgrade run and reloaded nginx on every run;
    it never restarted anything for a changed `panel.env`.

### Firewall, SFTP, logs, renewal

24. **`firewall`** — the installer **never enables ufw**, as install.sh never did: it only adds rules
    to a firewall that is already on. This is a decision, not an omission (R-10.4): many servers sit
    behind a provider's cloud firewall that already filters traffic, and enabling ufw from a script
    risks locking the operator out (SSH on a non-standard port, a rule a provider image relies on).
    On a fresh Ubuntu cloud image ufw is installed but inactive, so after a fresh install the
    firewall is inactive unless the operator had enabled it before; enabling it is the operator's
    choice, and the end of the run (step 31) says so and gives the commands. The panel's Firewall
    page shows whether ufw is active and edits rules; it does not turn ufw on. The step runs only
    when `ufw` is installed and `ufw status` says `Status: active`. Check: each of `<ssh> 80/tcp 443/tcp 8443/tcp 443/udp` appears as an `ALLOW` rule in `ufw status`,
    where `<ssh>` is every `port` that `sshd -T` prints (`<n>/tcp`, in its order), and `22/tcp`
    when `sshd -T` fails or names none — the same list step 31 gives (before, the active case
    always opened 22/tcp and never sshd's own port).
    Apply: `ufw allow <rule>` for each. 443/udp is QUIC: with HTTP/3 the vhosts advertise
    `Alt-Svc`, and a browser that cannot reach 443/udp hangs on the attempt before falling back.
25. **`sftp.group`** — check: group `cg-sftp-off` exists. Apply: `groupadd -f --system
    cg-sftp-off`. A site whose SFTP is off is in it: no login at all (a locked password alone still
    lets keys in under PAM).
26. **`sshd.sftp`** — `/etc/ssh/sshd_config.d/10-cloudground-sftp.conf` (template
    `templates/sshd-sftp.conf`), `0644`: `DenyGroups cg-sftp-off`; `Match User cg-site-*`:
    `ChrootDirectory %h`, `ForceCommand internal-sftp -d / -P symlink,hardlink@openssh.com` (no
    links over SFTP: their targets would be resolved later outside the chroot, by nginx or root),
    no forwarding, password authentication on, empty passwords off. Check: content. Apply: write,
    `sshd -t` (failure fails the step), `systemctl reload ssh` else `sshd` (failure ignored).
27. **`logrotate`** — `/etc/logrotate.d/cloudground` (template `templates/logrotate.conf`),
    `0644`: `/var/log/cloudground/*/*.log`, daily, `maxsize 1G`, 14 kept, compressed with
    `delaycompress`, `copytruncate` (nginx holds the logs open; renaming would leave it writing to
    an unlinked inode). Check: content. Apply: write, then `logrotate --debug <file>`; a failure is
    logged as a warning, not fatal. A full disk takes every site down: this is availability.
27a. **`needrestart`** — `/etc/needrestart/conf.d/cloudground.conf` (template
    `templates/needrestart.conf`), `0644`: `$nrconf{override_rc}` entries (value 0) for the agent's
    transient units `cg-helper-*`, `cg-restic-*`, `cg-scan-*`, `cg-wp-*`, `cg-malware-signatures-*`,
    `cg-clamav-install-*`, `cg-apparmor-freshclam-*`, `cg-cron-*` and `cg-apt-*` (the PHP extension
    and panel package installs), so that needrestart, after a library upgrade, does not restart a
    command the agent waits for (systemd would run it again from the start). The panel's services
    and the sites' PHP masters keep its normal handling. Check: content (`cgctl doctor` reports
    drift). Apply: write. Written also without needrestart installed (nothing reads it).
28. **`certbot.hook-dir`** — `/etc/letsencrypt/renewal-hooks/deploy` exists, `0755`.
29. **`certbot.hook`** — `.../deploy/cloudground-reload-nginx.sh` (template
    `templates/renew-hook.sh`), `0755`: reloads nginx after a renewal, which otherwise keeps
    serving the old certificate from memory. Check: content and mode.

### End of a run

30. Not a step: the public IPv4 (`https://api.ipify.org`, 5 s, else the first address of the
    host's interfaces), then, after 2 s, the last `"url":"…"` in `journalctl -u cloudground-api
    --since "1 min ago" -o cat` — read quietly: the command line goes to the log, its output (the
    one-time setup token) never does — with `<server-ip>` replaced by the IP; else
    `https://<ip>:8443`. Printed with "valid for 24 hours, works once" and the self-signed
    certificate warning.
31. Not a step: the firewall note. When `ufw` is installed and `ufw status` says
    `Status: inactive` (not in a container), the end of the run prints two dim lines (plain text
    without a TTY): "The firewall (ufw) is off. To turn it on with SSH, the sites and the panel
    open:" and the commands, joined by ` && `: `ufw allow <port>/tcp` for every `port` line of
    `sshd -T` (read quietly; `22` when it fails or prints none), then `ufw allow` for the other
    rules of step 24 (`80/tcp 443/tcp 8443/tcp 443/udp`), then `ufw enable`. Nothing is printed
    when ufw is missing, active, or its status reads as neither.

### Not the installer's

Named in the R-6.1 task, and on purpose not here, because something else owns them:

- **PHP-FPM per-version defaults**: none are written globally. Each site's master and pool are
  rendered by the agent (`internal/phpfpm`), and the distribution's service is off (step 4).
- **MariaDB tuning**: the agent's (step 21).
- **The admin account**: created by the operator through the one-time setup URL (step 30). The
  file `/root/cloudground-admin.env` the e2e suite reads is written by whoever ran the setup, not
  by the installer.
- **API tokens**: created in the panel or with `cgctl tokens create`. The installer makes only the
  agent token (step 12).
- **The panel's domain vhost**: rendered by the agent when a domain is attached in Settings
  (`internal/agent/ops/panel`). The installer writes only the bootstrap vhost (step 20).

## 4. Tuning settings tried and rejected

Carried over from `installer/install.sh`, which recorded them (AGENTS.md rule 6), word for word except that the product the KTLS idea came from is now "another panel" (no third-party product names here). None of
them may come back without a new A/B measurement that reproduces a win; add to this list rather
than quietly re-adding a setting. `TestRejectedTuningStaysOut` fails if a template or the nginx.conf
editor produces one of them.

> Only worker_connections is changed here. multi_accept was measured on a
> 2-core box and made no reproducible difference (sustained 9529 vs 9438 rps,
> static 21216 vs 21447 — the runs overlap), so it stays at the distro
> default rather than becoming config we cannot justify.

> Kernel TLS offload (ssl_conf_command Options KTLS) is deliberately NOT enabled.
> Another panel ships it, so we implemented it, verified the kernel really was doing
> the encryption (/proc/net/tls_stat TlsTxSw incrementing) — and then measured it
> as a large net LOSS on this workload:
>
>     KTLS on : sustained 6642 rps, static 14246 rps
>     KTLS off: sustained 9177 rps, static 19601 rps
>
> ~28% off cached throughput and ~31% off static, reproduced with multi_accept
> both on and off. KTLS pays for itself on large sequential transfers; a page
> cache serves many small responses, where the per-record kernel transitions
> cost more than the copy it avoids. Left off, with the numbers, so nobody
> "optimises" it back in without re-measuring.

> PHP-FPM's process manager, measured for issue #10 on the 2 vCPU box (uncached WooCommerce shop
> page, three alternated rounds of 30 s, `bench/uncached-php.md`): `pm = static` with the same 6
> children read 8.04 req/s and `pm.max_children = 4` read 7.99, against 7.83 for the shipped
> `dynamic` with 6, whose own runs spread 3.9 %. Inside the noise, and static keeps every worker of
> every site resident, so both stay out.

## 5. A downloaded release

`--release <dir>` holds `SHA256SUMS`, `SHA256SUMS.sig`, `panel-api`, `panel-agent`, `cgctl` (and, from R-10.5, `cg-site-shell`, which the bootstrap also downloads). Before
anything is installed:

1. the signature is checked by the release's one check, `version.VerifyManifestPinned`
   (internal/version SPEC §3.3). The keys: those compiled into this cgctl (`releaseKeys`, at most
   two during a rotation, plus any link-time `version.UpdatePublicKey`), any of which may have
   signed; with `CLOUDGROUND_RELEASE_PUBKEY` (base64 ed25519) from the operator, only that key may
   have signed, and when the build has keys too it must be one of them
   (`CLOUDGROUND_RELEASE_PUBKEY is not a release key this cgctl was built with`), because the
   self-updates after this install check with the compiled keys; no key at all → `no release
   signing key: …`;
2. `SHA256SUMS` carries exactly one `version ` line;
3. the release check (`CheckRelease`), as install.sh's `release_check` did (tests ported from
   `internal/version/installer_test.go`; versions ordered by `version.Compare`, so a
   `-beta.N` is older than its release and is not the release it leads to): `CLOUDGROUND_VERSION` other than `latest`/empty must parse
   (`CLOUDGROUND_VERSION=<v> is not a vX.Y.Z version`) and equal the manifest's (`the signed
   manifest is for <m>, not the requested <r>: refusing to install`); the manifest must not be
   older than any installed version that is a release tag (`version.ReleaseTag`; a development
   build, `git describe`'s `vX.Y.Z-N-gHASH` included, is not, as in step 3a) —
   `/usr/local/bin/panel-api --version` (10 s) and `/var/lib/cloudground-agent/highest-release` —
   unless `CLOUDGROUND_ALLOW_DOWNGRADE=1`
   (`release <m> is older than <v>, already installed: refusing to downgrade (set
   CLOUDGROUND_ALLOW_DOWNGRADE=1 to force it)`). The same release may be reinstalled;
3a. **another release over an installed panel is refused** (R-6.5, `CheckSameRelease`): on an
   upgrade run (§2), when the installed `panel-api --version` is a release tag (`version.ReleaseTag`:
   a development build, `git describe`'s `vX.Y.Z-N-gHASH` included, is not) and the manifest names
   any other one, newer or older, the install stops before any step with `CloudGround <a> is
   installed here, and this is the installer of <b>: …`, pointing to `cgctl update --to <b>` (which
   snapshots and rolls back, §10) and to the installer of `<a>` for a repair. The installer has no
   snapshot and no rollback, so it is not an update path; `CLOUDGROUND_ALLOW_DOWNGRADE` does not
   lift this. The same release is a **repair** and goes ahead as before (re-converging a server
   whose agent crash-loops, say). A fresh install, and an install over a build whose version is
   not a release (a development build), take any release; `--from` (an unverified local build,
   the development path) is not checked;
4. each of the three binaries matches its sum in the manifest; one missing from it fails;
   `cg-site-shell` is verified and installed when the manifest lists it (a release made before it
   existed does not).

Every file is opened `O_NOFOLLOW` and read once; the bytes that were verified are written into a
new root-only directory (`mktemp`, `0700`), and the binaries step installs from there: a file the
operator's directory swaps after the check is never installed. The staged copy is removed at the
end of the run.

**What this trusts.** `curl -fsSL https://get.cloudground.it | bash` (or a release's
`install.sh`; where the script is served, and what that host is trusted with, is §6.1) trusts the
release origin: the script, the cgctl hash stamped in it, cgctl and the
signing key compiled into cgctl all come from the same place, so whoever controls that place
controls the install. The signature then protects the rest of the download (binaries, manifest)
against tampering on the way, and the hash pins the release the script names. For a trust anchor
that does not come from the download, the operator takes values published out of band (the
website, the documentation) and passes them:

- `CLOUDGROUND_CGCTL_SHA256`: the expected SHA-256 of the release's cgctl; the bootstrap checks it
  in bash before running anything it downloaded;
- `CLOUDGROUND_INSTALL_SHA256`: the expected SHA-256 of the release's `install.sh`; the bootstrap
  checks the copy it fetches for `CLOUDGROUND_VERSION=latest` before running it (to check the
  first script itself, download it and compare `sha256sum install.sh` before running it);
- `CLOUDGROUND_RELEASE_PUBKEY`: the release key; cgctl requires the manifest to be signed with it
  and the key to be one of those compiled into it.

After the install, **self-updates use the keys compiled into the installed binary**
(`internal/agent/ops/panel`): trust on first use. A server installed from a tampered origin keeps
trusting that origin's key; one installed with an out-of-band anchor keeps the genuine key.

## 6. Bootstrap — `installer/install.sh`

About 40 lines of bash, run as root, everything inside `main` (a download cut off half way defines
a function and runs nothing). A release's copy is stamped by the release build: `@CGCTL_VERSION@`
with its tag and `@CGCTL_SHA256@` with its cgctl's SHA-256.

1. root, `ubuntu-24.04` or `ubuntu-26.04`, `x86_64`; otherwise a one-line reason, exit 1;
2. `CLOUDGROUND_LOCAL_BUILD=<dir>`: `exec <dir>/cgctl install --from <dir> "$@"`. The test VPS
   runs `sudo CLOUDGROUND_LOCAL_BUILD=/home/ubuntu/x/dist bash /home/ubuntu/x/installer/install.sh`;
3. the version is `CLOUDGROUND_VERSION`, by default the stamped one: a release's `install.sh`
   installs **its own** release, whose cgctl hash it carries. `CLOUDGROUND_VERSION=latest` must be
   asked for: there is no pinned hash for "whatever is newest", so the bootstrap downloads the
   latest release's own stamped `install.sh` (`…/releases/latest/download/install.sh`), checks it
   against `CLOUDGROUND_INSTALL_SHA256` when given, and runs it with `CLOUDGROUND_VERSION` cleared
   (it then installs its stamped release; a second "latest" is refused, no loop);
4. otherwise an unstamped copy (no version, no hash) refuses: `this script names no release`;
5. the five files of §5 come from `https://github.com/<CLOUDGROUND_REPO>/releases/download/<tag>`
   (`CLOUDGROUND_RELEASE_URL` replaces that root) with `curl --proto '=https' --proto-redir
   '=https'`, into a `mktemp -d` directory (0700) that a failure before the hand-over removes;
   cgctl must hash to `CLOUDGROUND_CGCTL_SHA256` (default: the stamped hash), else nothing
   downloaded is run; then `CLOUDGROUND_VERSION=<tag> exec <dir>/cgctl install --release <dir>
   --remove-release-dir "$@"`, and cgctl removes the directory when it ends.

### 6.1 Where the script is served, and what that host is trusted with (R-6.6)

**GitHub Releases of `cloudground-it/cloudground` is the source of truth**: the binaries, the
signed manifests and the update feed (`releases.atom`) come from there, an origin compiled into
every binary (internal/version §4) with no server of CloudGround's to run. The script is served at:

- `https://github.com/cloudground-it/cloudground/releases/latest/download/install.sh`: the
  `install.sh` asset of the release GitHub marks latest, which the release workflow makes the
  newest stable tag only (a patch to an older line is published but not marked latest, a beta
  never). This is the documented command until the short domain exists;
- `https://github.com/cloudground-it/cloudground/releases/download/vX.Y.Z/install.sh`: a pinned
  release's copy;
- `https://get.cloudground.it/` (and `/install.sh`): GitHub Pages of the same repository, deployed
  by the release workflow's `pages` job from the same build artifact, for the newest stable release
  only; `/` is the script published as `index.html` (Pages serves it as `text/html`, which `curl
  -fsSL … | bash` ignores). The domain is the repository's Pages setting (the job's `CNAME` file
  only records it). Setup and DNS: `.github/RELEASING.md`.

**What the script host must give, and what it cannot take.** The stamped script names one release
and the SHA-256 of that release's cgctl; it downloads the release from GitHub Releases (never from
the script host), runs cgctl only if it has that hash, and cgctl installs nothing whose manifest is
not signed by a key compiled into it (§5). So once the operator runs the **genuine** script, the
host it came from has no further say: a compromised Pages site, or a hijacked `get` record with a
certificate for it, cannot make an honest script install a different panel, because it holds
neither the bytes the hash names nor the signing key.

**But it can serve a different script**, and a script decides everything: a forged one can
download anything, skip every check, and leave a server whose later self-updates trust the forger's
key (§5, trust on first use). HTTPS protects the script only between the host and the operator;
the host itself, the repository's Pages settings, the DNS records and the GitHub account that can
change either are trusted with the first install. The environment anchors of §5
(`CLOUDGROUND_CGCTL_SHA256`, `CLOUDGROUND_RELEASE_PUBKEY`) do not help here: a forged script does
not read them. Mitigations, from simplest:

1. **Pin a version** in the documentation and the website: the
   `releases/download/vX.Y.Z/install.sh` of a named release, rather than `latest` or the short
   domain. The release assets are behind the repository's own protections (protected tags, the
   approved `release` environment), not the Pages site's or the DNS provider's.
2. **Verify before running**: download the script, check it against the release's signed
   `SHA256SUMS` with the release key published out of band (the documentation, the website,
   `internal/version/keys.go`), then run it — `docs/install-update-uninstall.md` gives the commands
   (`openssl pkeyutl -verify` of the ed25519 signature, then `sha256sum -c` of the script's line).
   `gh attestation verify install.sh --repo cloudground-it/cloudground` is the independent check
   of where it was built.
3. **Read it**: it is about 40 lines (§6), all inside `main`.
4. On the DNS side: a `CAA` record limiting issuance to Let's Encrypt, DNSSEC where the registrar
   offers it, and the domain verified for the organisation on GitHub (so no other Pages site can
   claim it).

## 7. Files the installer manages — one list

`internal/install/managed` lists every file the installer writes or edits (path, mode, owner, and
whether it is rendered whole or edited in place), and nothing else does: the steps of §3 render
exactly those, `cgctl doctor` checks exactly those, and the agent's drift check
(`internal/agent/ops/system/drift.go`, `managed`) accepts exactly those paths besides its own.

## 8. Tests

- Unit tests per step against the fakes (§1): a fresh root needs every step; after `Apply` the same
  `Check` says not needed; a second `Apply` changes nothing; a drifted file is found by `doctor`.
- Golden (`golden_test.go`): `testdata/golden/<24.04|26.04>/` was captured on 2026-10-10 by
  running **install.sh's own lines** (the heredocs and `sed` edits, by line range of the committed
  script) inside `ubuntu:24.04` (nginx 1.24, no HTTP/3) and `ubuntu:26.04` (nginx 1.28.3, HTTP/3)
  containers, with the distributions' `nginx.conf` before and after the edit. The renders and the
  nginx.conf editor must reproduce those bytes.
- Container (`make docker-install-test`): `cgctl install --plan`, then `cgctl install --container
  --skip-packages --from dist` in `ubuntu:24.04` and `ubuntu:26.04` with nginx, openssh-server
  and logrotate preinstalled (neither certbot nor cron, like a fresh server before their first
  run); then every path the written units name in their sandbox directives without `-` exists
  (systemd is not PID 1 in the container, so the units never start: this is the part of a start
  the container can check, the one that failed the agent at NAMESPACE on a fresh 26.04); a second
  `--plan` must find nothing to do, `nginx -t` and
  `sshd -t` pass, nginx answers on 80 and 443 (444, no reply), 8443 (502: no API in a container)
  and plain HTTP on 8443 (301), and `cgctl doctor --container --skip-packages` exits 0.
- Bootstrap in the container run: with curl and uname stood in for and a fake release, an
  unstamped copy refuses; a stamped copy fetches its own tag and runs its cgctl with
  `--release <dir> --remove-release-dir` and `CLOUDGROUND_VERSION=<tag>`; `latest` fetches the
  latest release's `install.sh` and runs it; a wrong `CLOUDGROUND_INSTALL_SHA256` refuses; a
  tampered cgctl is not run and its download is removed.
- Bootstrap (`bootstrap_test.go`): at most 50 lines, `bash -n`, `main` called on the last line
  (a truncated copy runs nothing), the placeholder and the two hand-overs present, and it refuses a
  machine that is not a supported Ubuntu.
- e2e section `R-6.1 installer`: on the box, `cgctl doctor` reports no drift, Redis is not
  running, and wp-cli is the pinned build.
- Container, uninstall (R-6.4, §9.5): after the checks above, `cgctl uninstall --plan` changes
  nothing; `cgctl uninstall --yes` keeps a site directory planted in `/srv/sites`; afterwards the
  list of every path on the machine (pseudo filesystems, `/tmp`, logs, apt's state and what running
  nginx itself leaves left out) is the one taken before the install, but for `/srv/sites` itself;
  nginx.conf is byte for byte the distribution's and `sites-enabled/default` points where it did;
  no user or group of CloudGround's is left, no file named `*cloudground*` or `cg-site*` (but the
  kept directory); `nginx -t` and `sshd -t` pass; a second uninstall says there is nothing to
  remove; and `cgctl install` then installs again with `doctor` finding no drift.

## 9. Uninstall — `cgctl uninstall` (R-6.4)

    cgctl uninstall [--plan] [--yes] [--remove-sites [--confirm <hostname>]] [--purge-packages]

Root only. Like the install it is a list of **actions**, each idempotent, built from what is on the
machine when it runs (`TakeInventory`, which changes nothing): a run that stopped half way is
finished by running it again, and a machine with nothing of CloudGround's left answers `CloudGround
is not installed here: nothing to remove` (an action that only follows a removal, such as a reload,
does not count). `--plan` prints every action (`- <id>  <what>`) and what will be kept, and changes
nothing.

**Refusals, before anything changes:** a running job that `jobs.BlocksPanelChange` names (a
backup, a restore, a job that cannot resume; internal/jobs SPEC §11), read from the state database
opened read-only → `not uninstalling: task <id> (<kind>) running: wait until it has finished`.
Without `--yes`, the question `Remove CloudGround? [y/N]` (anything but `y`/`yes`: nothing is
changed, exit 1). With `--remove-sites`, `--yes` is not enough: the server's hostname must be typed
(or given as `--confirm <hostname>`), else `the hostname does not match: nothing was changed`.

A failing action is reported and the run goes on (exit 1 at the end, with the failures), except
`keep.state`: until the panel's state has been copied out, nothing else is removed. What the run
did also goes to `/root/cloudground-uninstall/uninstall.log`.

### 9.1 What the installer records — `/var/lib/cloudground-agent/installed.json`

So that an uninstall takes away only what the install brought, the install records (root `0600`,
each list only grows, written only when something changed; a failure to write it is a warning in
the install log, so an uninstall then purges less, never more):

- `packages`: the packages `packages.base` and `packages.main` installed because they were
  missing; one already installed is never recorded;
- `ppas`: `ondrej/php` once `add-apt-repository` succeeded;
- `tools`: `/usr/local/bin/restic`, `wp`, `composer` when no file was at that path before;
- `nginx_default_site`: the target of the `sites-enabled/default` link `nginx.default-site`
  removed;
- `dirs`: the topmost missing directory on the way to `/var/lib/letsencrypt`, `/var/spool/cron`
  and `/etc/letsencrypt/renewal-hooks/deploy`, which belong to certbot and cron.

A server installed before this record has none: `--purge-packages` then purges nothing, and says
so. (The name differs from the `installed-packages` first proposed: the record holds more than
packages.)

### 9.2 The default tier: CloudGround goes, the sites' data stays

In order:

1. **Services**: `stop cloudground-update-guard`, `disable --now` `cloudground-api.socket`,
   `cloudground-api`, `cloudground-agent` (failures are warnings: a unit already gone).
2. **`keep.state`** (fatal; when `/var/lib/cloudground/state.db` or `secret.key` exists): §9.3.
3. **The sites' wiring**: `stop cg-site.slice` (every site's PHP master, cron runs, wp-cli and shell
   sessions, and managed apps); `disable --now` each `cloudground-php-*.service` and
   `cloudground-app-*.service` (#28), then their unit files and `.d`
   drop-in directories; each site's slice unit `cg-site-<id>.slice` (its limits, #19); the vhosts in `sites-enabled` that are the panel's (`cloudground-*`) or
   start with `Managed by CloudGround` (an operator's own vhost is left), every
   `conf.d/cloudground-*.conf`, `/etc/nginx/cloudground-auth`; `/etc/cron.d/cloudground-*` and the
   legacy `/var/spool/cron/crontabs/cg-site-*`; each shell login `cg-shell-<id>` (`userdel -f`), its
   `user@<uid>.service` mask (only a link to `/dev/null`), the group `cg-shell`. The functions'
   wiring (#143): `stop cg-fn.slice` (every invocation), `disable --now` each
   `cloudground-fn-*.socket`, then every `cloudground-fn-*.socket` and `cloudground-fn-*@.service`
   (a v0.8.x site function's too) and `cg-fn.slice`; their nginx files are among the panel's
   `cloudground-*` vhosts.
4. **The sites, kept**: each site user `cg-site-<id>` stays, so its files keep their owner, and is
   **locked** (`usermod --lock --expiredate 1 --shell /usr/sbin/nologin`): its logins were guarded
   by the panel's chroot and gate, which are gone. `/srv/sites`, the site databases and database
   accounts, MariaDB (still running) and its tuning file `99-cloudground.cnf` stay. The functions'
   code `/srv/functions` stays too, and each function user `cg-fn-<id>` is locked like a site
   user; `/srv/functions` goes only when empty (the tuning holds
   the hardening the kept databases ran with: `local_infile` off, symbolic links off). The sites'
   certbot lineages stay (`/etc/letsencrypt/live/<domain>`); their renewal pointed at CloudGround's
   ACME webroot, so they stop renewing until re-pointed or deleted: the end of the run lists them
   with the `certbot certonly --webroot …` and `certbot delete --cert-name …` commands. The certificates brought for a
   site (`/etc/cloudground/certs/custom`) are moved to `<kept>/custom-certs`. **Nothing serves the
   sites any more**: their vhosts and PHP masters are gone.
5. **The panel's certificates**: `certbot delete --cert-name cloudground-panel`, and the functions'
   lineages `cloudground-functions` and `cloudground-fn-<id>` (#143), which served only the panel's
   functions.
6. **The installer's files** (§7) and the agent's global ones: the three sshd drop-ins
   (`10-cloudground-sftp.conf`, `11-cloudground-shell.conf`, `09-cloudground-sftp-keys.conf`), then `sshd -t` and `reload ssh` (a
   failing `sshd -t` fails the action: an sshd that would not restart is the operator's to see);
   `60-cloudground.conf` (the live kernel values stay until a reboot or `sysctl --system`);
   where `nft` is installed, the block list's table `inet cloudground` when it exists (#21; with
   the agent gone its blocks would stay until a reboot); `logrotate.d/cloudground`; `needrestart/conf.d/cloudground.conf`; the renewal hook and `cloudground-cloudflare.ini`; the bootstrap
   vhost; **nginx.conf** (below); the default site's link, put back when recorded and its target
   exists; `nginx -t` (a failure fails the action) and `reload nginx` when it runs; the three unit
   files with their `.d` directories and every `cloudground-*` link in a `*.wants` directory, then
   `daemon-reload`; the binaries (`panel-api`, `panel-agent`, `cgctl` itself, `cg-site-shell`, the
   `cg-site-cron` link, their `.bak` and `.cg-update` files).
7. **The state**: `/etc/cloudground`, `/etc/cloudground-php`, `/var/lib/cloudground`,
   `/var/lib/cloudground-agent` (update snapshots and the malware quarantine included), the malware
   scan's signature set `/var/lib/cloudground-signatures`, the runtimes
   `/var/lib/cloudground-runtimes`, the apps' environment files `/etc/cloudground-app` (#28) and the
   functions' environment files and shims `/etc/cloudground-fn` (#143),
   and its freshclam configuration
   `/etc/clamav/cloudground-freshclam.conf` (ClamAV itself, a distribution package, stays),
   `/var/cache/cloudground`,
   `/var/log/cloudground`, `/run/cloudground`, `/var/www/cloudground-acme`, the install logs; the
   user `cloudground`, the groups `cloudground`, `cg-sftp-off` and `cg-sftp-keys`; the recorded `dirs` where empty;
   `/srv/sites` if empty.

Every removal is `os.RemoveAll` of an absolute path named here (never `/`), which removes a link
rather than following it.

**nginx.conf**: when it is still exactly what the installer's edit (`TuneNginxConf`) makes of the
saved original `nginx.conf.cloudground-pre`, the original comes back byte for byte; otherwise (the
operator edited it since) only the edit's lines go (`# cloudground-tuned` and the
`worker_rlimit_nofile` after it) and `worker_connections 4096;` gets the original's value back
(768, Ubuntu's, without a saved original), every other line kept. The saved original is then
removed.

### 9.3 What is kept for the operator — `/root/cloudground-uninstall/`

Backups live in a repository off the server, and **an uninstall never touches a repository**. But
the repository's password is sealed in the panel's state, which the uninstall removes: without it no
backup can be read. So, before anything is removed (`keep.state`; directory root `0700`, files
`0600`):

- `state/state.db` (and `-wal`) and `state/secret.key`: the state as it was, a link at either name
  refused (the API's user owns `/var/lib/cloudground`). A CloudGround of the same or a newer release
  reads it, which is also how the panel can be brought back over the kept sites.
- `backup.env`: read from a scratch copy of the above (an Open may migrate or compact what it
  opens; the kept copy stays as it was): `RESTIC_REPOSITORY`, `RESTIC_PASSWORD`, and
  `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_DEFAULT_REGION` when set, shell-quoted, ready
  for `set -a; . ./backup.env; set +a; restic snapshots`. It is plain text: the README and the end
  of the run warn to store it safely, off the server, and delete it. A state this build cannot read leaves the
  copy and says so in the README; it does not stop the uninstall.
- `README.txt`: what the files are, and every repository the panel used (the setting, and every
  `backups.repository`), marked as not touched. The end of the run prints the repositories, a URL's
  password masked (it is in `backup.env`, not for the terminal or a log), and the path of
  `backup.env`.

What an earlier uninstall kept there (a server uninstalled, installed again and uninstalled again)
is never overwritten: `state/`, `backup.env`, `README.txt` and `custom-certs/` are first moved into
`earlier-<UTC yyyymmddThhmmssZ>/` (root `0700`) beside them, since the older backup.env may be the
only way into an older repository. A rerun of an interrupted uninstall sets its own first copy aside
the same way, a harmless duplicate.

### 9.4 `--remove-sites` and `--purge-packages`

`--remove-sites` replaces step 4 with, while MariaDB and certbot still run: for each site user,
`pkill -KILL -u`, `userdel -f`, `groupdel`; `DROP USER IF EXISTS '<u>'@'localhost'` for every
account the agent's registry (`databases.json`) names, every `site_<n>` account and every
`cgq_<16 hex>` (Quarry's scoped accounts); `DROP DATABASE IF EXISTS` for every registry database,
every `site_<n>` and every `cgv_`/`cgm_`/`cgh_<16 hex>` (the agent's restore-test, staging and
holding databases); `certbot delete` of each site's lineage; `/srv/sites`; MariaDB's
`99-cloudground.cnf`; and the functions with them: each `cg-fn-<id>` (`pkill -KILL -u`,
`userdel -f`, `groupdel`) and `/srv/functions`, their code. Names are taken only when they match `^[a-z][a-z0-9_]{2,47}$` or those
patterns; any other database is left. A MariaDB that does not answer fails that action, naming
what was not dropped, and the run can be repeated once it is up. The custom certificates are
deleted with `/etc/cloudground`, not moved.

`--purge-packages` adds, last: `apt-get purge -y` of the recorded packages that are CloudGround's
data plane — `nginx`, `certbot`, `python3-certbot-dns-cloudflare`, `php<ver>-*`, and
`mariadb-server` only with `--remove-sites` and only when MariaDB then holds no database but its
own (`mysql`, `information_schema`, `performance_schema`, `sys`); never the base tools (`curl`,
`gnupg`, `ca-certificates`, `bzip2`, `software-properties-common`) nor `git`, `unzip`, `cron`, which
the system may have come to rely on. Debian's purge of `mariadb-server` keeps `/var/lib/mysql`
unless told otherwise, and the uninstall does not tell it. Then `add-apt-repository --remove` of
the recorded PPA, and the recorded tools. Nothing recorded: one line saying there is nothing to
purge.

### 9.5 Tests

`uninstall_test.go`, on the fakes of §1, after a full install and a site added by hand: the default
tier removes every path of §9.2 (listed in the test), keeps the site's tree, user (locked), MariaDB
tuning and an operator's vhost, drops nothing, restores nginx.conf and the default site, writes the
kept state and `backup.env` (values from a real state database, a quote in the password), and a
second inventory has nothing to do; `--remove-sites` drops exactly the CloudGround databases and
accounts, not `mysql` nor an unrelated one; a state database planted as a link stops the run before
any removal; `--purge-packages` purges only recorded data-plane packages, MariaDB not without
`--remove-sites`; the install records only what was missing; an nginx.conf edited after the install
keeps the edit. `cmd/cgctl`: a wrong hostname, `--yes` alone with `--remove-sites`, and a "no"
change nothing. The container run of §8.

## 10. Updates — `cgctl update` and the panel's button (R-6.5)

There is one update, `SelfUpdate` in the agent (internal/agent/ops/panel SPEC §2), and two ways to
ask for it:

- **the panel**: `POST /api/panel/update {version?}` (admin, session only; internal/api SPEC);
- **the shell**: `cgctl update [--to vX.Y.Z] [--channel stable|beta] [--check] [--timeout 10m]`,
  root on the server. It reads the agent's token and calls the agent's `SelfUpdate` over its
  socket, as the API does.

So both verify, snapshot, restart and roll back with the same code. What happens:

1. **Which release**: `--to`, else the newest the feed lists for the channel (`--channel`, else the
   panel's `update_channel` read from the state database opened read-only, else `stable`). The
   origin and the keys are the ones compiled into the installed binaries (internal/version §3–4).
2. **Checks before anything changes** (cgctl makes them first, to give the reason before any
   download; the agent makes them again): the signed manifest verifies with a compiled key and
   names the tag it was published as; the channel takes it (a beta never on stable); it is
   **strictly newer** than the running release and than `highest-release` (**no downgrade**, ever,
   from either path: `refusing to downgrade`; the installer's `CLOUDGROUND_ALLOW_DOWNGRADE=1` is
   the one way down, and it then meets §11's newer-schema refusal); no job `BlocksPanelChange`
   names is running (`task <id> (<kind>) is running`). The release that already runs is `up to date`, exit 0, with or without `--check`. `--check` stops here: `update available:
   <a> -> <b> (signature verified)` or `up to date`, exit 0, nothing changed.
3. **Stage and swap** (agent): every artifact downloaded beside its live name and checked against
   the manifest, binaries swapped by rename with `.bak` hard links, units saved and replaced.
4. **The guard** (`cloudground-update-guard`, the previous release's code): stop the API and its
   socket, **snapshot** the state database and the agent's records (newest 3 kept, in
   `/var/lib/cloudground-agent/update-snapshots`), start the new release, wait for it to be healthy
   (both units active on new PIDs running the new binaries' inodes, `/healthz` 200 through the API,
   which pings the agent, within 60 s). Unhealthy: the previous binaries, units and the snapshot go
   back, and the previous release is started and checked again. The new agent's unit first
   installs the system packages the release needs and the server lacks (#113, ops/panel SPEC
   §2.6); a failed install fails its start, so the update is rolled back.
5. **The verdict**: `/var/lib/cloudground-agent/update-result.json`. `cgctl update` waits for the
   one whose `started` follows its request (up to `--timeout`) and prints `updated: <a> -> <b>` and
   the snapshot (exit 0), or the failure and `rolled back: <a> runs again, with its state from
   <snapshot>` (exit 1), or that the previous release did not come back either, with where to look
   and `cgctl install` to re-converge (exit 1).

**What an update keeps.** It replaces the binaries and, when the release changes them, the three
unit files; it migrates the state database forward. **It never deletes or rewrites site data**:
nothing in the path above opens `/srv/sites`, a site database, a certificate or a backup, and the
snapshot and its restore cover only the panel's state and the agent's records. Sites keep serving
throughout: nginx and the sites' PHP masters are not restarted (only the panel answers 502 for the
seconds between the stop and the new API's start).

**Several releases at once.** v0.1.0 straight to v0.3.0 is one update: the steps of the state
schema travel in the binary (internal/state SPEC, Migrations), so the new API runs every step the
database lacks, in order, at its first start, and the snapshot taken before is the database at
v0.1.0's schema. `TestSchemaGoldenFromEveryVersion` (a database stopped at any version reaches the same schema) and
`TestOpenRefusesANewerSchema` hold both directions.

**The installer is not an update path.** Running another release's `install.sh` (or `cgctl
install --release`) on an installed server is refused (§5 step 3a) and names `cgctl update --to
<release>`; the installer of the installed release still repairs the server.

## 11. Version compatibility

Versions are `vX.Y.Z` (stable) and `vX.Y.Z-beta.N`, with semantic versioning from 1.0.0: a patch
fixes, a minor adds, and only a major (`X`) removes or changes what an earlier release of the same
major offers. Before 1.0 the minor played the major's role.

- **A patch release** (`Z`) fixes: no state schema step, no unit file change, no change to the
  agent protocol (internal/agentrpc), no setting or file format the previous patch cannot read. A
  rollback across a patch needs no snapshot, though one is always taken.
- **A minor release** (`Y`) may add schema steps (forward only, never editing a released one), add
  agent methods and parameter fields, add or change unit files (the guard applies them with
  `daemon-reload`), and add settings with defaults. It may not remove or rename an agent method or
  a parameter field that the previous minor's API sends: a removal takes two minors (stop sending,
  then stop serving).
- **A major release** (`X`) may remove or rename a route, a field, a `cgctl` command or flag, an
  MCP tool, an agent method or a setting, each marked deprecated in the changelog of an earlier
  minor. It updates from the last minor of the previous major, and restores its backups.
- **Backups**: every release restores the backups of every earlier one, in every mode (proved by
  the e2e section R-6.6 on backups made by real older releases).
- **The state database** is migrated only forward and only by the API at start; a build refuses a
  database at a newer schema than its own (`SchemaNewerError`, internal/state). The way back is the
  pre-update snapshot, which a failed update restores on its own.
- **The restart window.** The guard stops the API (socket first), restarts the agent, then starts
  the API: for those seconds there is no API, so no request of the old API can reach the new agent
  (nginx answers 502). What is in flight when it starts is drained: the API finishes its requests on
  SIGTERM, the update's own reply included, and the agent its RPCs (5 s ceiling). After the window
  both run the same release. Before R-6.5 the agent was restarted while the old API still ran, so
  the old API did call the new agent for a moment; the rules above keep that safe too, and still
  apply to an update *from* such a release: unknown JSON fields are ignored (encoding/json), an
  unknown method is a `not_found` the API reports rather than misreads, and a request cut by the
  restart fails as `canceled` and is not retried by the agent.
- **Backups across releases** (#37): every 1.x release restores every backup any 0.x or 1.x
  release made (the snapshot layout and tags of ops/backup SPEC §4, the records of internal/api
  SPEC §5.1), in every mode and as a download, and its restore test passes there; retention there
  never forgets one it still protects. Checked on real old releases by the e2e section `R-6.6
  restore across updates` (docs/e2e.md): each installed on an empty state, a backup taken there,
  the panel's own update to the build under test, then the old backup restored.
- **Jobs across the restart**: queued jobs wait for the API to come back; a running job of a kind
  that cannot resume blocks the update (§10 step 2) rather than being cut; a resumable one
  (`Rerun`, internal/jobs SPEC §6) runs again after the restart.
- **The update path itself** runs in the release being replaced (the guard is its
  `panel-agent.bak`): a new rule for applying updates takes effect from the update *after* the one
  that ships it.
