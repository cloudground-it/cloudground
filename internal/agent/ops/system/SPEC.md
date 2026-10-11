# ops/system — status, services, logs, firewall, drift

The agent's read-mostly view of the machine, and the two things an operator
may change on it from the panel: restarting a managed service and editing a
firewall rule. Written from the e2e suite (`e2e/services_test.go`,
`e2e/features_test.go` firewall/status sections, `e2e/security_test.go` log
routes), the API callers (`internal/api/ops.go`, `system.go`, `alerts.go`,
`scheduler.go`), the UI pages (`services`, `logs`, `firewall`, dashboard).

Everything here goes through the host adapters: `host.Systemd` (D-Bus),
`host.Firewall` (ufw), `host.Exec` (journalctl, sshd, version probes). No
`systemctl` is ever run, and no tool output is classified by its wording.

The RPC methods, their parameters and their results are unchanged
(`internal/agentrpc/methods.go`, types in `internal/rpc`).

## 1. SystemStatus

Result: `rpc.SystemStatusResult`.

- CPU count, load average (1 min), memory total/available, uptime from
  `sysprobe.Probe`; disk total/free of the filesystem holding `SitesRoot`.
- `CPUHeadroomPct = clamp(100 - load1*100/cpus, 0, 100)`;
  `MemHeadroomPct = available*100/total`; `DiskHeadroomPct = free*100/total`
  (0 when the total is 0).
- `NginxRunning`, `MariaDBRunning`, `RedisRunning`: the unit's `ActiveState`
  is `active` (`nginx`, `mariadb`, `redis-server`), read over D-Bus.
  A systemd that cannot be asked reports the three as false and logs a
  warning: the dashboard and the 5-minute metric sample still get the load
  numbers. (Contrast `ServiceStatus`, §2.)
- `AgentVersion` is the build's version.
- **No process is started.** The old implementation forked `systemctl
  is-active` three times on every call (measured with the test runner: 3
  forks per poll); a unit test counts the fake exec's calls and requires 0.

## 2. ServiceStatus and RestartService

Managed services, by name → unit: `nginx` → `nginx`, `mariadb` → `mariadb`,
`redis` → `redis-server`, `cloudground-api` → `cloudground-api` (restart
only). Anything else is `invalid` and no job is started.

`ServiceStatus` returns, in this order, `nginx`, `mariadb`, `redis`, then one
`site-php` row. `redis` is optional: the installer no longer installs it
(R-6.1), and where systemd does not know `redis-server` (LoadState
`not-found`) it has no row; an upgraded server that keeps it shows it,
switched off (`disabled`). nginx and mariadb always have their row, a missing
unit included.

- per service, from one `UnitProps` call: `Active` (ActiveState `active`),
  `Enabled` (UnitFileState `enabled`), `Detail` = the raw ActiveState (the e2e
  compares it with `systemctl show -p ActiveState`), `State` = `running` if
  active, else `failed` if ActiveState is `failed`, else `disabled` if not
  enabled, else `stopped`. While running: `MemoryBytes` from MemoryCurrent
  when systemd accounts it, `Since` from ActiveEnterTimestamp.
- `Version`: the program's own answer (`nginx -v`, `mariadbd --version`,
  `redis-server --version`; nginx writes to stderr, so both streams are read),
  the first `N.N[.N]` in it. **Cached per (service, ActiveEnterTimestamp)**:
  a package upgrade restarts the service and so re-probes, and a probe that
  fails (redis not installed) is not retried on every poll. The old cache
  kept only successes, so a missing redis cost one fork per poll forever.
- `site-php`: every `cloudground-php-*.service` unit. Units whose LoadState is
  `not-found` (a deleted site systemd still remembers) are skipped. `Detail` =
  `"<n> running"`; if any other unit is not active, State `failed`, Active
  false and `Detail` gets `", <k> not running: <user> <state>, …"`.
  **The read changes nothing**: the old code ran `reset-failed` on the
  not-found units from inside this read.
- A systemd that cannot be asked is an error, not a page of stopped services
  (the API raises an alert per stopped service).

`RestartService` restarts the unit over D-Bus and returns when systemd's job
has finished (bounded at 3 minutes: MariaDB can be slow); a job that ends in
anything but `done` is the adapter's `ErrJobFailed`. Result
`{"restarted": "<unit>"}`. A unit systemd does not know (LoadState
`not-found`: the package is not installed) is `conflict`, "<name> is not
installed on this server", and no job is started (the job failed, and the API
answered 502). A unit that is not active and whose
UnitFileState is `disabled` is `conflict` and no job is started: systemd's
restart starts a stopped unit, and a service switched off on purpose (Redis,
off until a site needs it, installer) would then listen on loopback for
every site's PHP and shell. The services page shows no restart for a
`disabled` row; this holds for the API too.

## 3. TailLog

Params `{source, site, lines}`; `lines` outside 1..2000 becomes 200.

| source | read from | `Path` in the result |
| --- | --- | --- |
| `agent` | journal of `cloudground-agent.service` | `journal:cloudground-agent` |
| `api` | journal of `cloudground-api.service` | `journal:cloudground-api` |
| `php-error` | journal of every site master, `cloudground-php-*.service` | `journal:php-fpm` |
| `nginx-access` | `<NginxLogDir>/access.log` | the path |
| `nginx-error` | `<NginxLogDir>/error.log` | the path |
| `mariadb` | `<MariaDBLogDir>/error.log` | the path |
| `site:<domain>:access` | `<LogRoot>/<domain>/access.log` | the path |
| `site:<domain>:error` | `<LogRoot>/<domain>/error.log` | the path |
| `site:<domain>:php` | `<LogRoot>/<domain>/php-error.log` | the path |

Anything else is `invalid`. For a site source the domain is the one in the
source; `site`, when given, must name the same domain (the API passes both).
The domain must pass `site.ValidateDomain` (no traversal).

Journal: `journalctl --no-pager -q -o json -n <lines> -u <unit>`, decoded as
typed JSON (MESSAGE may be a string, a byte array or null), rendered one entry
per line as `<RFC 3339 local time> <host> <identifier>[<pid>]: <message>`
(the shape of `-o short-iso`). No entries → `-- No entries --`. A journalctl
failure is an error for every journal source (the old `php-error` path
swallowed it and showed an empty log).

Files (`tailFile`):

- opened `O_RDONLY|O_NOFOLLOW|O_NONBLOCK`: a symlink at the log's name is
  refused, a FIFO cannot hang the agent; anything but a regular file is an
  error. A missing file is the text `(log is empty or not created yet)`.
- only the last 4 MiB are read; if that window starts mid-line the partial
  first line is dropped; then the last `lines` lines; each line over 8 KiB is
  cut to 8 KiB plus ` …(line truncated)`.

## 4. Firewall

`FirewallStatus` → `{enabled, rules}` from `host.Firewall.Status` (`ufw
status` under C locale; rules are ufw's own lines, which the UI shows as is).
When ufw is inactive, `ufw status` lists no rules although ufw keeps them
and applies them once enabled, so the rules are those of `ufw show added`
without the leading `ufw ` (`allow 22/tcp`), with `enabled: false`; the UI
says they are stored and not enforced.
ufw not installed → `{enabled: false, rules: ["ufw is not available"]}`, not
an error. Any other failure is an error.

`FirewallRule {action, port, proto, from}`:

- port 1..65535; proto `tcp`/`udp`, anything else becomes `tcp`; `from`, when
  set, is an IP or CIDR. Otherwise `invalid`, before ufw runs.
- **Essential ports can never be closed**: every port sshd listens on
  (§5), 80, 443 and the panel's port (8443). For those, `deny` without
  `from`, or from a CIDR of prefix 0 (`0.0.0.0/0`, `::/0`: every address),
  is `invalid` and ufw is not run. Deleting an allow rule is a lockout
  as surely as a deny, because ufw's incoming default is deny, so `delete`
  there runs only `ufw delete deny` of that shape, which can only widen
  access: the result is `{"rule": "delete deny <port>/<proto>"}`; ufw says
  "Rule deleted" when active and "Rules updated" when inactive (it then
  only rewrites its stored rules); when it had no such deny ("Could not
  delete non-existent rule", in every address family) it is `invalid`,
  naming the allow rule it will not remove. `allow`, and `deny` limited to
  one source, are fine, and that deny can be deleted again (the old guard
  refused every delete on an essential port, so a deny it had accepted could
  not be undone). The old list missed the panel port: `deny 8443` closed the
  panel.
- `allow`/`deny` → `ufw allow|deny <port>/<proto>`, or with `from`:
  `ufw allow|deny from <from> to any port <port> proto <proto>`.
- `delete` removes the deny and the allow rule **of the same shape**: with
  `from`, the source-limited rule. (The old code dropped `from` on delete and
  so removed the port's global rules instead of the one the operator named,
  and it ignored the error of `ufw delete deny`.)
- Result `{"rule": "<action> <port>/<proto>"}` (the API audits it).

## 5. SSH ports

One parser, used by the firewall guard and by the SFTP port shown with a
site's SSH keys (`Agent.sshPort`, the first port).

- `sshd -T` (the effective configuration: Includes, repeated `Port`, Match
  defaults). Every `port N` line, **and the port of every `listenaddress
  host:N` line** — a `ListenAddress` with its own port makes sshd listen
  there and not on `Port`, and the old parser missed it.
- If `sshd -T` fails or reports no port: the `Port` and `ListenAddress` lines
  of `sshd_config` and `sshd_config.d/*.conf`.
- 22 is always in the essential set; the first port found is the SFTP port
  (22 if none).

### 5.1 sshd drop-ins the agent writes

`ApplySSHDDropIn(path, content)` (used by ops/sftp for the site shell,
R-4.5):

- the file already holds `content`: nothing is done (no reload);
- else it is replaced (`rootfile.Replace`, root, 0644) and `sshd -t` checks
  the whole configuration. A refusal puts the previous file back, or
  removes the new one when there was none, and returns the error: sshd is
  never reloaded on a configuration it refuses;
- then the first of `ssh.service`, `sshd.service` that is active is
  reloaded. None active (socket activation, before the first connection)
  is not an error: sshd reads the file when it starts. Neither unit
  readable is an error.

## 6. CheckDrift

Params `{expected: path → sha256}`. For every path:

- not clean, not absolute, or outside the places the panel writes → listed in
  `NotManaged`, never opened (otherwise a root-side hash oracle for any file).
  Managed: below `NginxSites`, `NginxConfDir`, the MariaDB tuning directory,
  `PHPMasterDir`, `NginxAuthDir`; a `cloudground-php-*.service` directly in
  `SystemdDir`, and a `*.conf` directly in its `cloudground-php-*.service.d`
  drop-in directory (the site's slice, R-3.7); anything below `SitesRoot`
  with `/wp-content/mu-plugins/` in its path; and every path in
  `internal/install/managed` (the installer's configuration files, the same
  list `cgctl doctor` checks: internal/install/SPEC.md §7).
- opened `O_NOFOLLOW|O_NONBLOCK` and hashed. Missing, a symlink at the name
  (ELOOP) or anything but a regular file → actual hash `""`: drift of that
  file, not a failure of the whole check. The connector's directory belongs
  to the site, so its user can plant either; the old check failed for every
  site on a symlink and blocked in open(2) on a FIFO. Any other error fails
  the check.
- `Drifted` lists `{path, expected_hash, actual_hash}` where they differ.

## 7. Not here

`PurgeCache` lives in `ops/cache`. Cron (`ApplyCron`, `RunCron`) lives in
`ops/cron`.
