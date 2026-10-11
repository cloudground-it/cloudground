# A site's security checks: what they cost (issues #31, #76)

**Not measured yet.** This page lists what is to be measured on the reference VPS and how. The
numbers come with the next run there. Until then, the bounds below are the design's, not measurements.

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
| wall time | to measure | to measure |
| panel-api CPU | to measure | to measure |
| panel-agent CPU | to measure | to measure |
| the site slice's CPU (wp-cli) | to measure | to measure |
| the site slice's memory over its resting level | to measure | to measure |
| panel-api resident memory after the check (`systemctl show -p MemoryCurrent cloudground-api`) | to measure | — |

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
| wall time | to measure | to measure |
| panel-agent CPU (the update: freshclam, the set's rebuild) | to measure | to measure |
| the site slice's CPU (clamscan) | to measure | to measure |
| clamscan's memory peak (the slice over its rest) | to measure | to measure |
| files scanned (13,000 entries in the bench tree) | to measure | to measure |

Also to measure:

- the set's size, in signatures (the stamp) and on disk (`du`), against ClamAV's whole
  databases in `/var/lib/clamav`;
- `clamscan` with ClamAV's whole databases (`--database=/var/lib/clamav`) on the same tree, for
  the memory the reduction saves;
- the uncached scenario of `wrk-suite.sh` during a scan, against without (the scan's priority).

The issue's bound is a scan that never keeps ~1 GB resident and whose cost, spread over the
week, stays far under 1 % of the box.
