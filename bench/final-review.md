# After the final security review — compared with Phase 4.3

Same box and workload as [baseline.md](./baseline.md). The cache change in this
round is one map (`$cg_private_req`) and two extra variables on
`fastcgi_cache_bypass`/`fastcgi_no_cache`, so requests carrying `Authorization`
or the Performance Guard probe header skip the cache. Ordinary traffic carries
neither.

| Scenario | After 4.3 | After review | Change |
| --- | ---: | ---: | --- |
| cached-sustained (500 conn) | 10,125.66 | 12,960.83 | +28 % |
| cached-single (1 conn) | 6,882.38 | 7,666.06 | +11 % |
| cached-spike (200 conn) | 11,312.51 | 12,262.73 | +8 % (p99 6.44 s, one outlier run) |
| static, 1 KB (200 conn) | 16,781.98 | 15,754.20 | −6 % (noise) |
| flood (2,000 conn) | 9,022.52 | 7,863.04 | −13 %, 1,054 timeouts (noise-bound, see phase-4.1.md) |
| browsers | 9,842.95 | 12,953.36 | +32 % |
| uncached (10 conn) | 7.95 | 14.98 | +88 % |

**Reading:** nothing regressed beyond the noise already measured on this VPS,
but the gains are far larger than one extra map lookup could explain, so they
are not attributed to this change. Between the two runs the box was reinstalled
several times, the bench site's connector was refreshed to the current one, and
the VPS is shared, noisy hardware. The CPU-parity probe printed only the core
count on this run, so the runs could not be normalised. Treat this as "no
regression detected", not as an improvement; repeat it on a quiet, dedicated box
before quoting any number.
