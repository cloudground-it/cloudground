# Integration build (Phase 4 in progress) — A/B against Phase 3

Same VPS and workload as [phase-3.md](./phase-3.md), 2026-10-10. "phase3" is the closed Phase 3
(de5be98); "integ" is the integration branch at e207bee: the new API router, the agent's
Capabilities, DB Studio, realtime events, locales, the rewritten connector, the panel icons.

Full suite, alternated phase3, integ, phase3, integ:

| Scenario | phase3 1 | integ 1 | phase3 2 | integ 2 | Mean change |
| --- | ---: | ---: | ---: | ---: | --- |
| cached-sustained (500 conn) | 13,100 | 12,496 | 12,942 | 12,304 | −4.8 % |
| cached-single (1 conn) | 7,895 | 7,867 | 7,534 | 7,481 | −0.5 % |
| cached-spike (200 conn) | 12,733 | 13,671 | 12,718 | 12,646 | +3.4 % |
| static, 1 KB (200 conn) | 17,083 | 15,417 | 16,219 | 16,385 | −4.5 % |
| flood (2,000 conn) | 11,680 | 11,055 | 11,068 | 9,367 | −10.2 % |
| browsers | 13,861 | 13,004 | 13,236 | 13,136 | −3.5 % |
| uncached (10 conn) | 14.2 | 13.9 | 14.2 | 14.1 | −1.6 % |

cached-sustained read lower for integ in both rounds, so it was measured again on its own, six
alternated 30 s runs (integ first): integ 12,607 / 12,481 / 12,966, phase3 13,558 / 13,010 /
12,395 — means 12,685 against 12,988, −2.3 %, with phase3 alone spreading 9 %.

**Reading:** no regression attributable to the code. The request path is unchanged: `nginx -T`
is byte-identical between the two builds on the box, the connector only runs on uncached
requests (uncached −1.6 %, within noise), and the panel's own services use 0.4 % of one core at
rest. flood spreads 15 % between two runs of the same build here.
