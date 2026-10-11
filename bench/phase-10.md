# Phase 10 — A/B against Phase 9b

Same VPS and workload as [phase-9b.md](./phase-9b.md), 2026-10-10. "9b" is a7a2539; "10" is
f2cc311: Go 1.26.9 instead of 1.25.14, updated dependencies (x/crypto, x/sys, sqlite, the MariaDB
driver), the module path github.com/cloudground-it/cloudground, comment-only changes in the
rendered configuration and the licence change. On the box, `nginx -T`, `sysctl` and the PHP-FPM
pools are identical between the two builds once comments are stripped (0 lines differ).

Full suite, alternated, five runs each (v9: two rounds; v10: three rounds, each 90 s after its
install):

| Scenario | Mean change |
| --- | --- |
| cached-sustained (500 conn) | −1.1 % |
| cached-single (1 conn) | +0.4 % |
| cached-spike (200 conn) | −0.4 % |
| static, 1 KB (200 conn) | −1.7 % |
| flood (2,000 conn) | +6.6 % (v9 −6.9 %, v10 +17.0 %: noise) |
| browsers | −9.4 % (v9 −15.6 %, v10 −5.2 %) |
| uncached (10 conn) | −1.0 % |

browsers was then measured alone (v11): six alternated installs per build, three runs per install,
18 runs each.

| Build | Mean | Median | Std dev | Min | Max |
| --- | ---: | ---: | ---: | ---: | ---: |
| 9b | 12,177 | 12,167 | 477 | 11,182 | 12,790 |
| 10 | 11,946 | 11,980 | 580 | 10,686 | 12,738 |

**Reading:** no regression. browsers is −1.9 % (median −1.5 %) over 18 runs each, inside the 3 %
noise floor; 10 was faster in two of the six rounds. The earlier dips came from single runs right
after an install (9,982 and 11,357). Only nginx serves this scenario and its configuration is
unchanged; the Go processes share the box's two cores with nginx and wrk, which is the only path
by which the toolchain could show up here.
