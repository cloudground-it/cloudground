# After Phase 4.3 (one PHP-FPM master per site) — compared with 4.2

Same box and workload as [baseline.md](./baseline.md); CPU parity 2.38 s before
the run and 2.13 s after (the VPS was noisier than in earlier runs).

| Scenario | Baseline | After 4.2 | After 4.3 | 4.3 vs 4.2 |
| --- | ---: | ---: | ---: | --- |
| cached-sustained (500 conn) | 9,877.75 | 9,601.24 | 10,125.66 | +5 % |
| cached-single (1 conn) | 6,887.16 | 7,083.77 | 6,882.38 | −3 % (noise) |
| cached-spike (200 conn) | 9,815.95 | 10,257.98 | 11,312.51 | +10 % |
| static, 1 KB (200 conn) | 15,625.45 | 17,494.47 | 16,781.98 | −4 % (noise) |
| flood (2,000 conn) | 8,951.69 | 8,807.16 | 9,022.52 | +2 % |
| browsers | — | 10,356.32 | 9,842.95 | −5 % (noise) |
| uncached (10 conn) | 5.99 | 6.29 | 7.95 | **+26 %** |

The uncached gain matches the A/B prototype (+28 %): each site now has its
own OPcache and APCu, sized for it, and no `open_basedir`. p99 uncached fell
from 1.93 s to 1.57 s. Cost: about 14 MB PSS per site master.

Isolation (in `scripts/e2e-verify.sh`): a site cannot see another site's APCu
entries nor list `/srv/sites`.
