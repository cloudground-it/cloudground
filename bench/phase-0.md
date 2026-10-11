# Rewrite plan, Phase 0 — before and after

Same VPS (OVH, 2 vCPU, Ubuntu 26.04) and workload as [baseline.md](./baseline.md), run on the box
itself with `bench/wrk-suite.sh`. "Before" is 68b35dd, "after" is the Phase 0 merge.

Phase 0 does not change any rendered directive for an ordinary site: the PHP timeouts (R-0.8) only
differ for a site whose `max_execution_seconds` is above 110, and the reconcile change (R-0.7)
only skips reloads that rendered nothing new.

| Scenario | Before | After | Change |
| --- | ---: | ---: | --- |
| cached-sustained (500 conn) | 13,049.92 | 12,144.38 | −7 % (p99 1.19 s → 0.54 s) |
| cached-single (1 conn) | 7,676.90 | 7,671.20 | 0 % |
| cached-spike (200 conn) | 13,870.83 | 13,742.14 | −1 % |
| static, 1 KB (200 conn) | 16,383.46 | 16,753.39 | +2 % |
| flood (2,000 conn) | 11,313.83 | 11,676.21 | +3 % |
| browsers | 12,632.56 | 13,821.26 | +9 % |
| uncached (10 conn) | 13.50 | 15.31 | +13 % |

**Reading:** no regression beyond this VPS's noise. Runs of the same build on this box have
differed by up to 28 % on cached-sustained (see final-review.md), and its p99 halved here. The
runtime effect of R-0.7 is not on the request path: it is the reloads that no longer happen when a
site is created or deleted, which the e2e measures directly (zero nginx reloads and no reload of an
unchanged PHP master during the site-count reconcile, against 8–10 nginx reloads before).
