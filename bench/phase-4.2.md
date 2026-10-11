# After Phase 4.2 (sizing) — compared with the baseline

Same box and workload as [baseline.md](./baseline.md); second of two runs;
CPU parity 2.07 s before and 2.08 s after.

| Scenario | Baseline | After 4.1 | After 4.2 | 4.2 vs baseline |
| --- | ---: | ---: | ---: | --- |
| cached-sustained (500 conn) | 9,877.75 | 10,083.97 | 9,601.24 | −3 % (noise) |
| cached-single (1 conn) | 6,887.16 | 7,168.87 | 7,083.77 | +3 % |
| cached-spike (200 conn) | 9,815.95 | 10,310.43 | 10,257.98 | +5 % |
| static, 1 KB (200 conn) | 15,625.45 | 15,387.17 | 17,494.47 | +12 % |
| flood (2,000 conn) | 8,951.69 | 7,917.22 | 8,807.16 | −2 % (noise, see phase-4.1.md) |
| browsers | — | 9,859.95 | 10,356.32 | +5 % vs 4.1 |
| uncached (10 conn) | 5.99 | 5.49 | 6.29 | +5 % |

p99 latency fell in most scenarios (static 248 → 123 ms, uncached 2.56 → 1.93 s).
The individual A/B results behind each change are in docs/benchmarks.md.
