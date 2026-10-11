# Phase 9b and the docs-check fixes — A/B against the security-fix build

Same VPS and workload as [phase-4-5.md](./phase-4-5.md), 2026-10-10. "sec" is e30cde3 (the final
security review's fixes); "9b" is integration at a7a2539: per-site databases and users, database
choices at site creation, DB Studio as its own service, the docs-check fixes (R-9.2) and the fixes
from the VPS runs. Nothing in the visitor's request path changed; the uploads PHP deny and the PHP
bypass cookies from the security fixes are in both builds. Alternated sec, 9b three times, one full
suite each (run "v8"); the earlier two-round run "v7" (sec against dac36c4) is below it.

| Scenario | sec 1 | 9b 1 | sec 2 | 9b 2 | sec 3 | 9b 3 | Mean change |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | --- |
| cached-sustained (500 conn) | 12,537 | 12,473 | 11,820 | 12,786 | 12,693 | 12,249 | +1.2 % |
| cached-single (1 conn) | 7,956 | 8,068 | 7,506 | 7,417 | 7,900 | 7,280 | −2.6 % |
| cached-spike (200 conn) | 13,513 | 12,770 | 12,566 | 12,602 | 13,666 | 13,244 | −2.8 % |
| static, 1 KB (200 conn) | 15,665 | 16,636 | 13,019 | 14,235 | 15,993 | 17,507 | +8.3 % |
| flood (2,000 conn) | 10,504 | 13,078 | 12,466 | 11,308 | 12,222 | 12,077 | +3.6 % |
| browsers | 12,033 | 13,260 | 12,558 | 12,522 | 11,940 | 12,976 | +6.1 % |
| uncached (10 conn) | 13.3 | 13.2 | 13.9 | 14.1 | 14.3 | 13.9 | −0.8 % |

v7, two rounds, sec against dac36c4: cached-sustained −1.9 %, cached-single −0.3 %, cached-spike
−2.0 %, static −0.8 %, uncached +1.2 %, browsers −6 % from one 9b run at 10,363 with a 6.7 s p99
(its second run was +2 %); v8 re-measured browsers at +6.1 %.

**Reading:** no regression. Every scenario is within the 3 % noise floor or better in v8, and the
v7 browsers dip did not repeat.
