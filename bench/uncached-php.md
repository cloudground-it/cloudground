# Uncached PHP — where the time goes (issue #10)

2026-10-11, the reference OVH VPS (2 vCPU "Intel Core Processor (Haswell, no TSX)", 4 GB, Ubuntu
26.04, PHP 8.5.4 from Ubuntu, CPU parity steal 0), one bench site built by `setup-workload.sh`
(WooCommerce 11.2.1, WordPress 7.1.3, Twenty Twenty-Five, 100 posts, 50 products). Build: main at
5c3c069 (v0.1.0 + 5), then the branch with the fix. Loopback, wrk on the box.

The uncached scenario read about 7 req/s (`phase-10-fresh.md`); another panel on the same box had
been seen at about 15 req/s with what was meant to be the same shop. Each run below is the
uncached scenario's shape (2 threads, 10 connections, a query string never sent twice, 10 s
timeout) for 30 s unless stated, every arm restarted and warmed, the arms alternated. No run had
a non-2xx answer.

## 1. Profile of a worker under load

- **perf** (`perf record -F 499 -a -g`, 15 s under load): 89 % of all samples are in
  `php-fpm8.5` user space: `execute_ex` 28 %, `zend_hash_find` 7 %, then the allocator and array
  copies. MariaDB does not appear among the busy processes; the CPU is PHP running WordPress and
  WooCommerce code, not waiting.
- **strace -c** on one worker for 15 s (17 requests): about 1,640 system calls per request (686
  `access`, 238 `read`, 92 `openat`, 37 `getdents64`: WordPress and WooCommerce looking for
  templates and files), 2.6 ms of system-call time per request, against about 250 ms of CPU. The
  sandbox and the file system are not where the time goes.

## 2. CloudGround's master against a plain PHP-FPM pool

Ubuntu's own `php8.5-fpm` service with its stock configuration (pool `www`, `pm = dynamic`,
5 children, stock OPcache, no systemd sandbox) running as the site's user, against the same
tree. "plain" and "minimal" go through the same minimal nginx server (`try_files`, `fastcgi_pass`,
no cache); "full" is the site's own vhost over HTTPS.

| Arm | run 1 | run 2 | run 3 | Mean |
| --- | ---: | ---: | ---: | ---: |
| plain pool, minimal vhost | 7.49 | 7.05 | 6.86 | 7.13 |
| CloudGround master, minimal vhost | 7.16 | 6.82 | 6.76 | 6.91 |
| CloudGround master, its vhost | 7.32 | 7.19 | 7.12 | 7.21 |

**Reading:** the per-site master, its sandbox and its pool settings are level with a plain pool
(−3 % to +1 %, inside the spread of each arm). Nothing in how CloudGround runs PHP-FPM halves the
throughput.

## 3. The page was not the same page

Turning the object-cache drop-in off (6.46, 6.66) or the connector off (7.29, 6.96) against the
same build (6.92, 6.86) changed nothing either. What did: switching the theme and back. On a
second WooCommerce site made the same way, 7.04 req/s became 13.72 at once, and the shop page went
from 171 KB listing 12 products to an 80 KB page listing none.

A store created entirely from the command line has not run WooCommerce's first-run steps; a theme
switch or a first visit to the admin runs them, and one of them puts a new store in **coming soon**
mode (`woocommerce_coming_soon = yes`, store pages only). Visitors then get a placeholder that
renders about twice as fast as the real shop. The earlier comparison recorded neither the response
size nor the product count, and on the other panel a theme other than the default had been
activated, which runs those steps. That it measured the placeholder is the likely explanation for
most of the gap; it cannot be checked after the fact. `setup-workload.sh` now keeps the store open
and fails unless `/shop/` lists the bench products (`SPEC.md` §1).

With the store open after its first-run steps, the shop page renders at about 7.8–8 req/s (the
first-run steps also add blocks to the header), which is the baseline for the rest of this record.

## 4. The candidates, one at a time

Applied to the bench site's master with a systemd drop-in and a copy of its pool file (6 children,
as rendered for one site on this box), three alternated rounds:

| Arm | run 1 | run 2 | run 3 | Mean | vs base |
| --- | ---: | ---: | ---: | ---: | --- |
| base (as shipped) | 7.76 | 8.02 | 7.72 | 7.83 | |
| tracing JIT on the command line | 7.22 | 8.36 | 8.36 | 7.98 | +1.9 % |
| `pm = static`, 6 children | 8.06 | 7.96 | 8.09 | 8.04 | +2.6 % |
| `pm.max_children = 4` | 7.79 | 8.36 | 7.83 | 7.99 | +2.0 % |
| no sandbox (every `Sandbox` line reset) | 8.12 | 8.19 | 8.02 | 8.11 | +3.6 % |

The base arm alone spreads 3.9 %, so none of these is a result yet. The JIT's first run was low
because a tracing JIT compiles while it runs; it was measured again with a 20 s load before each
run and 60 s runs, four rounds:

| Arm | run 1 | run 2 | run 3 | run 4 | Mean |
| --- | ---: | ---: | ---: | ---: | ---: |
| base | 7.96 | 7.87 | 8.04 | 8.14 | 8.00 |
| tracing JIT | 8.36 | 8.32 | 8.34 | 8.02 | 8.26 |

**+3.2 %**, ahead in three rounds of four and in every measurement taken after warm-up. No worker
crashed and no PHP error was logged with the JIT on.

**What the JIT finding really is:** the pool already asked for the tracing JIT
(`php_admin_value[opcache.jit] = tracing`), but PHP reads the JIT mode only when the master
starts, so that line was ignored: `opcache_get_status()` reported the JIT off on every site. The
mode now goes on the master's command line next to the buffer size (`internal/phpfpm` SPEC §4),
and the e2e section `woocommerce` asks the site's PHP whether the JIT is on (red on 5c3c069:
`jit=off mode=disable`).

Not taken:

- `pm = static` and `pm.max_children = 4`: inside the spread; static also keeps every worker of
  every site resident.
- Removing the sandbox: at most 3.6 %, inside the spread, and it is the tenant boundary; it stays.
- `open_basedir`, `realpath_cache`: `open_basedir` is not used (phpfpm SPEC §1, where it cost
  28 %); the realpath cache is PHP's default (4 MB, 120 s) and held 140 KB on this site, so it is
  never full.
- A different PHP build: the profile is the interpreter running application code, with no sign of
  a slow build; the plain pool with the same binary is no faster.

## 5. Result

The branch installed over 5c3c069 (the upgrade's re-render restarted the master with the new
command line), against the plain pool of §2, alternated, three rounds of 60 s after a 20 s load:

| Arm | run 1 | run 2 | run 3 | Mean |
| --- | ---: | ---: | ---: | ---: |
| plain PHP-FPM pool, stock Ubuntu configuration, minimal vhost | 7.97 | 8.10 | 8.04 | 8.04 |
| CloudGround, this branch, its own vhost (TLS, cache bypass) | 8.37 | 8.37 | 8.27 | 8.34 |

**CloudGround's uncached path is 3.7 % ahead of a plain PHP-FPM pool on the same box** (the issue
asks for no more than 10 % behind), with every isolation property kept: the e2e sections
`security`, `SR tenant data`, `R-10.5 root boundary` and `woocommerce` passed on the branch (169
checks, 0 failed).

One pass of the whole suite on the branch (CPU parity 2.23 s before, 2.35 s after, no steal), for
the record; nothing on the cached path changed:

| Scenario | req/s | p50 | p99 | errors |
| --- | ---: | ---: | ---: | --- |
| cached-sustained (500 conn) | 9,543 | 47.70 ms | 390.13 ms | none |
| cached-single (1 conn) | 6,738 | 134 µs | 257 µs | none |
| cached-spike (200 conn) | 9,948 | 20.62 ms | 1.78 s | none |
| static, 1 KB (200 conn) | 15,989 | 11.34 ms | 376.08 ms | none |
| flood (2,000 conn) | 9,262 | 99.90 ms | 274.43 ms | 987 refused connections |
| browsers | 8,911 | 20.30 ms | 1.67 s | none |
| uncached (10 conn) | 7.96 | 1.22 s | 1.53 s | none |

The uncached figure is not comparable with `phase-10-fresh.md`'s 7.07: that store had not run
WooCommerce's first-run steps, and this page carries the blocks they add (§3).

## 6. The master's IPC namespace, and the cost of #51 and #54

**PrivateIPC=yes on the master.** The master shared the host's IPC namespace (SysV shared memory,
semaphores, POSIX message queues), which every other site's PHP shares too; the SPEC left
`PrivateIPC` for the master to an A/B. On the branch rebased on main (which has #54's private
`/dev/shm`), with the master's unit changed by a drop-in, three alternated rounds of 60 s after a
20 s load:

| Arm | run 1 | run 2 | run 3 | Mean | vs base |
| --- | ---: | ---: | ---: | ---: | --- |
| base (main with #54, the JIT fix) | 8.19 | 8.09 | 8.16 | 8.15 | |
| without #54's `/dev/shm` tmpfs | 8.34 | 8.16 | 8.14 | 8.21 | +0.8 % |
| `PrivateIPC=yes` | 8.21 | 8.37 | 8.07 | 8.22 | +0.8 % |

Both inside the spread: #54 costs nothing measurable on a PHP site, and neither does an IPC
namespace. With `PrivateIPC=yes` the master's `ns/ipc` differs from the host's, and APCu (232
entries, hits growing), OPcache and the JIT work as before: all three map anonymous memory. The
master now takes it (`internal/phpfpm` SPEC §4.1, `MasterHardening`); the e2e section `security`
reads each site's master's IPC namespace and was red on the build before.

**#51 and #54 on the cached path.** `cgbench ab`, A = 5c3c069 with the JIT fix (before #51 and
#54), B = main at 1ee2fc8 with the same fix (#51's regex location for leftovers, #54's `/dev/shm`,
and the other merges in between), three rounds, each install followed by a maintenance run so the
sites are rendered by the build under test (`-install-cmd "bash installer/install.sh && sleep 15
&& <POST /api/system/maintenance>"`: without it, an `ab` round measures the previous build's
renders until the five-minute reconcile):

| Scenario | A 1 | B 1 | A 2 | B 2 | A 3 | B 3 | Mean change | Spread A | Spread B |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| cached-sustained | 8,193 | 9,184 | 9,347 | 9,204 | 9,448 | 9,556 | +3.5 % | 13.9 % | 4.0 % |
| cached-single | 6,564 | 6,476 | 6,448 | 6,309 | 6,491 | 6,278 | −2.3 % | 1.8 % | 3.1 % |
| cached-spike | 9,810 | 8,892 | 9,951 | 9,694 | 9,731 | 9,362 | −5.2 % | 2.2 % | 8.6 % |
| static | 16,046 | 15,579 | 15,951 | 14,996 | 14,716 | 14,586 | −3.3 % | 8.5 % | 6.6 % |
| uncached | 8.0 | 8.2 | 7.3 | 8.2 | 8.3 | 8.1 | +3.9 % | 12.7 % | 1.2 % |

CPU parity 2.17–2.25 s, except B2 at 2.76 s (the box was slower for that run); no steal. The tool's
verdict is "within noise" for every scenario. One thing to watch: B was below A in all three rounds
on cached-single (−2.3 %) and cached-spike (−5.2 %). An extra regex location is matched on every
request, so a small cost there is plausible; it is smaller than this box's run-to-run spread and
would need more rounds, or a separate load generator, to be pinned down.
