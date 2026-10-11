# Phases 4 and 5 — A/B against Phase 3

Same VPS and workload as [phase-3.md](./phase-3.md), 2026-10-10. "phase3" is de5be98; "integ" is
integration at cd2d546: the API, state, accounts, sites, deployments, backups, proxies, alerts and
job queue rewritten (Phase 4), nginx/PHP-FPM/tuning/cache engines rewritten with byte-identical
output except the per-type cache bypass and the 30 s reload drain, plus the routing editor (Phase 5).
Alternated phase3, integ, phase3, integ, one full suite each.

| Scenario | phase3 1 | integ 1 | phase3 2 | integ 2 | Mean change |
| --- | ---: | ---: | ---: | ---: | --- |
| cached-sustained (500 conn) | 12,249 | 12,782 | 13,341 | 12,764 | −0.2 % |
| cached-single (1 conn) | 7,685 | 7,576 | 7,919 | 7,778 | −1.6 % |
| cached-spike (200 conn) | 13,983 | 13,258 | 12,858 | 13,644 | +0.2 % |
| static, 1 KB (200 conn) | 17,121 | 15,585 | 15,897 | 16,540 | −2.7 % |
| flood (2,000 conn) | 10,861 | 10,664 | 7,358 | 11,369 | +20.9 % |
| browsers | 12,211 | 13,105 | 12,534 | 12,857 | +4.9 % |
| uncached (10 conn) | 14.1 | 14.3 | 14.1 | 14.4 | +2.4 % |

**Reading:** no regression. Every scenario is within the 3 % noise floor or better; flood's
+20.9 % comes from one phase3 run at 7,358 and is noise (flood spreads up to 32 % between runs of
one build here).
