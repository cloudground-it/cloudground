# After Phase 4.1 (cache correctness) — compared with the baseline

Same box and workload as [baseline.md](./baseline.md); second of two runs.

| Scenario | Baseline (e7e8957) | After 4.1 | Change |
| --- | ---: | ---: | --- |
| cached-sustained (500 conn) | 9,877.75 | 10,083.97 | +2 % |
| cached-single (1 conn) | 6,887.16 | 7,168.87 | +4 % |
| cached-spike (200 conn) | 9,815.95 | 10,310.43 | +5 % |
| static, 1 KB (200 conn) | 15,625.45 | 15,387.17 | −2 % (noise) |
| flood (2,000 conn) | 8,951.69 | 7,917.22 | see below |
| browsers (rotating Accept-Encoding, tracking links) | — | 9,859.95 | new scenario |
| uncached (10 conn) | 5.99 | 5.49 | see below |

The **browsers** scenario is the one the fixes are for. Before them, each of the
four Accept-Encoding values had its own cache entry and every tracking-tagged
request bypassed the cache to PHP (about 6 req/s), so a third of that traffic
would have run at uncached speed.

## The flood scenario is noise-bound on this box

Two thousand connections on loopback, with wrk sharing the two cores, swing by
±20 % between identical runs. Interleaved A/B runs of the page-cache location
(eight runs each, 8 October 2026):

| Page-cache `open_file_cache_valid` | flood req/s | sustained req/s |
| --- | --- | --- |
| 1 s (shipped) | 5,259 · 6,642 · 5,513 · 6,002 · 6,294 · 6,479 · 7,687 · 6,701 | 8,873 · 9,894 |
| 5 s | 7,837 · 6,131 | 10,060 · 10,458 |
| 10 s | 8,353 · 8,179 | 9,587 · 10,062 |
| 60 s (no override) | 8,545 · 8,528 · 6,747 · 6,277 | 9,464 · 9,957 |

The same 60 s configuration measured 8,545 and then 6,277. No variant reproduces
a win at 500 connections, so the shortest window, which makes a purge visible
within a second, ships. The flood number is not used to accept or reject a
change on this box; a separate load generator is needed for that.

## Uncached

Toggling the OPcache isolation settings (validate_root, validate_permission):
5.07 and 5.27 req/s with them, 4.95 and 5.71 without. No measurable cost; they
stay. The uncached path is CPU-bound and swings about ±10 % between runs.
