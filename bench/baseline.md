# Baseline before the hardening work

Every change to caching, request handling or tuning is compared against this
run. Measured on the unmodified import (commit e7e8957, after the rebrand and
before any fix), loopback, with `bench/setup-workload.sh` and
`bench/wrk-suite.sh`.

| | |
| --- | --- |
| Date | 8 October 2026 |
| Target | OVH VPS, 2 vCPU Intel Haswell (no TSX), 3.8 GB RAM, 40 GB disk |
| OS | Ubuntu 26.04 LTS, PHP 8.5, nginx from the distribution |
| Workload | WordPress 7.1.3, WooCommerce 11.2.0, 100 posts, 50 products, profile `commerce`, object cache on |
| Generator | `wrk` 4.1.0 on the target (loopback) |
| CPU parity | 2.21 s before, 2.26 s after, steal 0 |
| Install time | 1 m 22 s |
| e2e | `scripts/e2e-verify.sh` 55 passed, 0 failed |

The second of two runs (the first is discarded as warm-up):

| Scenario | req/s | p50 | p99 | Errors |
| --- | ---: | ---: | ---: | --- |
| cached-sustained (500 conn) | 9,877.75 | 55.30 ms | 629.43 ms | none |
| cached-single (1 conn) | 6,887.16 | 0.129 ms | 0.250 ms | none |
| cached-spike (200 conn) | 9,815.95 | 17.88 ms | 509.66 ms | none |
| static, 1 KB (200 conn) | 15,625.45 | 11.81 ms | 248.06 ms | none |
| flood (2,000 conn) | 8,951.69 | 196.27 ms | 1.43 s | none |
| uncached (10 conn) | 5.99 | 1.64 s | 2.56 s | none |

Warm-up run for reference, to show the run-to-run spread: cached-sustained
9,609, static 15,108, flood 8,857, uncached 5.72 req/s.
