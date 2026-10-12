# A site's security checks: what they cost (issues #31, #76)

**Measured on 2026-10-12** on the reference OVH VPS (2 vCPU, 4 GB, Ubuntu 26.04, CPU parity 2.2 s,
no steal), main at 85da91c (the v0.3.0 candidate), the bench site of `setup-workload.sh`. The
bounds below are the design's; the tables under them are the measurements.

## 1. The WordPress vulnerability check (#31, api SPEC §11.6)

What one check does: one wp-cli run in the site's slice and sandbox (`core version`,
`plugin list`, `theme list`), then one HTTPS request per component to the vulnerability
database, four at a time. Each answer is reused for six hours by every site that has the same
component.

Bounded by design:

- **One check at a time on the server** (the `security:vulncheck` lock). Each site is checked
  once a day, at the time it was first seen, so the checks spread over the day.
- **Memory in the panel:** one answer read at a time per request, each at most 4 MB (the largest
  plugin record, Elementor's, is about 170 KB), parsed into compact records. The cache holds at
  most 4 096 components.
- **The site:** wp-cli runs in the site's own slice, under its memory and task limits
  (`site-resources.md`).

To measure, on the bench site (`setup-workload.sh`, WooCommerce with its default theme):

```sh
sudo COLD=1 PANEL_EMAIL=... PANEL_PW=... IP=<public-ip> bash bench/security-cost.sh vuln 5
```

| | Run 1 (cold cache) | Runs 2–5 (cached) |
| --- | ---: | ---: |
| wall time | 3.79 s | 3.40–3.58 s |
| panel-api CPU | 80 ms | 34–44 ms |
| panel-agent CPU | 75 ms | 76–108 ms |
| the site slice's CPU (wp-cli) | 3,053 ms | 2,802–2,975 ms |
| the site slice's memory over its resting level | 135 MB | 135–136 MB |
| panel-api resident memory after the check (`systemctl show -p MemoryCurrent cloudground-api`) | 137 MiB | — |

Almost all of a check is wp-cli loading WordPress and WooCommerce in the site's slice (about 3 s of
CPU); the lookups themselves cost the panel well under 0.1 s even with a cold cache (WooCommerce,
its default theme and core: a handful of components). A hundred WordPress sites checked once a day
cost about 310 s of CPU a day, under the 864 s bound.

The issue's bound is the same as the usage collection's: far under 1 % of the 2-CPU box. That
is 864 s of CPU a day, so the check must stay under that even with a hundred WordPress sites.

## 2. The malware scan (#76, api SPEC §11.7, ops/malware SPEC)

What one scan does:

- `MalwareSignatures` installs ClamAV the first time, and updates the set when it is a day old:
  freshclam, then the set rebuilt by streaming ClamAV's `main` and `daily` databases and keeping
  only the web platforms' signatures, plus LMD's PHP rules.
- Then `clamscan` runs over the site's tree as the site's user, in its slice and sandbox.

Bounded by design:

- **Nothing resident.** No `clamd`; freshclam's daemon is stopped and masked; `clamscan` runs for
  one scan.
- **Memory:** `MemoryMax=768M` on the scan's own unit, inside the site's slice. The bound rests
  on the reduced set: ClamAV with all its databases holds about a gigabyte, mostly Windows and
  document signatures a web tree does not run.
- **CPU and IO:** `CPUWeight=10`, `IOWeight=10`, `Nice=19`, `IOSchedulingClass=idle`; one scan at a
  time on the server (`security:malwarescan`); each site weekly at an hour of its own, so a
  server's scans spread over the week.
- **Size:** `--max-filesize=25M --max-scansize=100M --max-files=10000`, two hours at most.

To measure, on the bench site:

```sh
sudo FORCE_SIGS=1 PANEL_EMAIL=... PANEL_PW=... IP=<public-ip> bash bench/security-cost.sh malware 3
ps -eo rss,comm | grep -E 'clamd|freshclam|clamscan'   # after the runs: nothing
```

| | Run 1 (with the signature update) | Runs 2–3 (scan only) |
| --- | ---: | ---: |
| wall time | 124.3 s | 119.3 s, 117.9 s |
| panel-agent CPU | 330 ms | 323 ms, 251 ms |
| the update (freshclam and the set's rebuild, in its own unit `cg-malware-signatures-*`) | 5.4 s CPU, 5.9 s wall, 25 MB peak | — |
| the site slice's CPU (clamscan) | 115.3 s | 114.7 s, 115.0 s |
| the slice's memory over its rest (sampled every 0.1 s) | 188 MB | 100 MB, 200 MB |
| files scanned | 11,559 | 11,559 |

The update ran in a unit of its own, so the agent's CPU does not include it (the journal gives its
cost); ClamAV's databases were already a day old or newer, so freshclam downloaded nothing.

| | Result |
| --- | --- |
| the set | 105,685 signatures in the stamp (106,640 loaded by clamscan, with LMD's PHP rules), 10.2 MB on disk |
| ClamAV's whole databases (`/var/lib/clamav`) | 110 MB on disk |
| `clamscan` by hand on the same tree, the set | 112.6 s, peak RSS **109 MiB** |
| `clamscan` by hand on the same tree, ClamAV's whole databases | 228.6 s, peak RSS **1,007 MiB** |
| uncached scenario, quiet against during a scan (alternated, two each) | 8.2, 8.2 against 7.3, 7.2 req/s: **−11.3 %** |

**Reading.**

- **The 768 MB bound holds with room to spare**: the scan peaks near 110 MiB with the reduced set
  (the slice's sampled peak, 100–200 MB over its rest, also holds the page cache of the files it reads). With
  ClamAV's whole databases the same scan needs a gigabyte and would be killed at the bound: the
  reduction is what makes the bound possible, and it also halves the scan's time.
- **CPU:** about 115 s of CPU per scan of this 11,559-file store, once a week per site: 16 s a day
  per site, 0.01 % of the box. Fifty such sites stay near 0.5 %.
- **While a scan runs, the site's uncached pages are 11 % slower.** The scan runs in the site's own
  slice with `CPUWeight=10` against PHP-FPM's 100, so it takes about a tenth of the CPU the site
  is entitled to when the site is saturated, as designed; cached pages do not go through PHP. The
  scan lasts two minutes a week per site, at an hour of its own.

The issue's bound is a scan that never keeps ~1 GB resident and whose cost, spread over the
week, stays far under 1 % of the box.
