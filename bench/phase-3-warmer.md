# Rewrite plan, Phase 3 — cache warmer before and after

R-3.8 changed the warmer from a sequential crawl with two or three requests per URL to one
gzip request per URL with 2–8 concurrent workers. Same VPS (OVH, 2 vCPU, Ubuntu 26.04), bench
WordPress site. Each run: `wrk -t2 -c50 -d25s` on the cached home page, first alone, then while
a warm of the same site runs (started 3 s in). Two runs per build; the first warm of each pair
starts from an empty cache.

| Build | Run | Alone req/s | Alone p99 | During warm req/s | During warm p99 | Warm time |
| --- | --- | ---: | ---: | ---: | ---: | ---: |
| before (Phase 3 batch 1) | 1 | 14,862.70 | 11.30 ms | 10,631.43 | 25.10 ms | 30.0 s |
| before | 2 | 15,970.58 | 6.83 ms | 14,023.28 | 13.40 ms | 5.0 s |
| after (R-3.8) | 1 | 15,762.06 | 6.90 ms | 9,875.65 | 22.12 ms | 20.7 s |
| after | 2 | 15,138.63 | 7.58 ms | 14,177.57 | 13.46 ms | 3.3 s |

**Reading:** the concurrent warmer finishes a cold warm about a third sooner and does not make live
cached traffic worse: p99 during the warm is the same or lower, and throughput during the warm is
within this VPS's run-to-run noise (up to 28 % on cached-sustained). Script:
`scratchpad/warmbench.sh` in the session; to be folded into bench/ with the Phase 9 rewrite.
