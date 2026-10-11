# Per-site limits and usage: the defaults and the cost (issues #18, #19)

2026-10-11, the reference OVH VPS (2 vCPU Haswell, 4 GB, Ubuntu 26.04, PHP 8.5.4, kernel 7.0), the
bench site of `setup-workload.sh` (WooCommerce 11.2.1, store open, `uncached-php.md`). Main after #10
for the "before", this branch for the rest. Loopback, wrk on the box.

## 1. What a normal WooCommerce site uses

The bench site's slice (`cg.slice/cg-site.slice/cg-site-1.slice`), sampled every second through a
whole `wrk-suite.sh` (every scenario, the uncached one at 10 connections with 6 PHP workers busy),
then the site's cron run (wp-cli in the slice) and a cache warm:

| | Peak |
| --- | ---: |
| memory (`memory.current`), during the suite | 370 MB |
| memory, the slice's lifetime peak (`memory.peak`: provisioning's cron runs, the suites, cron, a warm) | 561 MB |
| tasks (`pids.current`) | 8 |
| CPU | 2.05 CPUs, the whole box, during the uncached scenario |

Defaults chosen from it (ops/sites SPEC §3.1), so that a normal store never reaches them:

- `MemoryMax`: half the server's memory, never under 1024 MB. 1907 MB on this box, 3.4 times the
  peak; `MemoryHigh` at nine tenths of it.
- `TasksMax`: 512, 64 times the peak. Before this the slice had no task limit (`pids.max` read
  `max`).
- No CPU quota: the store used both CPUs when rendering uncached pages, so any quota would have
  cut its throughput. Fairness between sites stays with the kernel's equal CPU weights; a quota is
  for the administrator to set on a site that needs it.

## 2. The slice unit on the request path

The bench site with its slice unit (accounting for CPU, memory, IO and tasks, plus the limits)
against the same site with the unit moved away (`memory.max`, `pids.max` back to `max`, no
`io.stat`), alternated, uncached scenario, 60 s after a 20 s load:

| Arm | run 1 | run 2 | run 3 | Mean |
| --- | ---: | ---: | ---: | ---: |
| with the slice unit | 8.39 | 8.65 | 8.51 | 8.52 |
| without | 8.14 | 8.38 | 8.46 | 8.33 |

**Reading:** the arm with the unit was not slower: +2.3 %, inside this box's spread. The cached
paths never enter a site's slice (nginx serves them).

One suite on the branch, for the record:

| Scenario | req/s | p50 | p99 | errors |
| --- | ---: | ---: | ---: | --- |
| cached-sustained (500 conn) | 9,219 | 48.22 ms | 790.27 ms | none |
| cached-single (1 conn) | 6,401 | 140 µs | 302 µs | none |
| cached-spike (200 conn) | 9,761 | 20.42 ms | 102.75 ms | none |
| static, 1 KB (200 conn) | 15,867 | 12.63 ms | 65.25 ms | none |
| flood (2,000 conn) | 8,956 | 102.00 ms | 302.86 ms | 987 refused connections |
| browsers | 9,287 | 19.93 ms | 162.17 ms | none |
| uncached (10 conn) | 7.69 | 1.29 s | 1.63 s | none |

## 3. What the collection costs

The agent's CPU (`cpu.stat` of `cloudground-agent.service`) over direct calls to `SiteUsage`, and
the API's over maintenance ticks:

| | CPU |
| --- | ---: |
| one read of the slice counters (100 calls) | 0.78 ms per call |
| one read with the disk walk of the bench site (13,186 entries, 5 calls) | 100 ms per call |
| a whole maintenance tick, every job of it (reconcile, metrics, drift scan, backups' sweep, the usage sample…), API side | 323 ms |

A tick runs every five minutes and a site's directories are walked once an hour. The usage
collection of this site is therefore 0.78 ms + 100 ms / 12 ≈ 9 ms of CPU every five minutes:
**0.0015 % of the 2-CPU box**. Even the whole maintenance tick, with all its other jobs, is 0.06 %.
The walk grows with the number of files. A hundred stores the size of this one would take 10 s
of CPU an hour, about 0.14 % of this box. The issue's bound was 1 %.

## 4. A site over its limit (e2e `site resources`)

With a memory limit of 128 MB, a PHP request allocating 400 MB on site A is ended by the kernel
inside A's slice (`memory.events` `oom_kill` counts it). Site B answers during and after it,
keeps its PHP master, and has no kill. A's master stays up (`OOMPolicy=continue`), so the next
request to A is served at once. On the first run, before that setting, systemd's default
(`OOMPolicy=stop`) stopped A's whole master, and the next request got a 502 until the restart.
