# Rewrite plan, Phase 3 — A/B against the last pushed build

Same VPS (OVH, 2 vCPU, Ubuntu 26.04) and workload as [baseline.md](./baseline.md), run on the box
itself with `bench/wrk-suite.sh`, on 2026-10-09. "main" is 4375b86, the last pushed build;
"phase3" is the Phase 3 branch (de5be98). Each build was installed with its own installer and
measured twice, alternating main, phase3, main, phase3, with the box cleaned beforehand (three
sites, no leftovers from the e2e).

| Scenario | main 1 | phase3 1 | main 2 | phase3 2 | Mean change |
| --- | ---: | ---: | ---: | ---: | --- |
| cached-sustained (500 conn) | 12,631 | 12,536 | 12,936 | 13,862 | +3.2 % |
| cached-single (1 conn) | 7,734 | 7,399 | 7,672 | 8,345 | +2.2 % |
| cached-spike (200 conn) | 13,473 | 12,831 | 13,424 | 13,904 | −0.6 % |
| static, 1 KB (200 conn) | 15,744 | 16,928 | 16,104 | 16,223 | +4.1 % |
| flood (2,000 conn) | 9,960 | 11,673 | 11,208 | 10,751 | +5.9 % |
| browsers | 12,242 | 12,612 | 12,895 | 12,952 | +1.7 % |
| uncached (10 conn) | 13.6 | 15.0 | 15.0 | 15.0 | +4.6 % |

**Reading:** no regression. A single run of phase3 taken earlier the same evening, right after a
full e2e on a box with nine sites, read 8–10 % below Phase 0's numbers on cached-single, static
and browsers; alternating the two builds on a clean box shows that was the box, not the code.
Comparisons on this VPS are made A/B, alternated, from now on.
