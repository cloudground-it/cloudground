# Phase 10 on the reimaged box — A/B against the last verified build

2026-10-11, the same OVH VPS reimaged with Ubuntu 26.04 (2 vCPU, "Intel Core Processor (Haswell,
no TSX)", CPU parity 2.29 s; the box before the reimage measured 2.07–2.26 s). A fresh install of
b2bc332 measured well below the numbers in [phase-10.md](./phase-10.md) (cached-sustained ~10,000
instead of ~12,500, uncached ~7.3 instead of ~13.8), so the last verified build was installed on
the same box and the two alternated: "verified" is f2cc311, "fresh" is b2bc332 (the fresh-install
fixes: unit paths, the services wait, cron, the agent's directory modes under UMask=0027).
Three rounds, one full suite each, 90 s after each install.

| Scenario | verified | fresh | Mean change |
| --- | ---: | ---: | --- |
| cached-sustained (500 conn) | 9,621 | 9,759 | +1.4 % |
| cached-single (1 conn) | 6,699 | 6,745 | +0.7 % |
| cached-spike (200 conn) | 9,829 | 10,221 | +4.0 % |
| static, 1 KB (200 conn) | 15,700 | 15,618 | −0.5 % |
| flood (2,000 conn) | 8,238 | 9,132 | +10.8 % (noise) |
| browsers | 9,554 | 9,757 | +2.1 % |
| uncached (10 conn) | 6.8 | 7.2 | +5.9 % |

**Reading:** no regression; the fresh build is level or ahead everywhere. The drop against the
earlier records is the reimaged machine, not the code: the verified build is just as slow here.
Absolute numbers from this box are therefore not comparable with the records before it.

## The release candidate on the purged box

3172a9f (the v0.1.0 tree before its changelog), installed on the same box after it was purged back
to stock Ubuntu: clean install in 1m 0s, the full e2e (1290 passed, 0 failed, the R-6.5 upgrade
included), then three suites.

| Scenario | run 1 | run 2 | run 3 | Mean | vs "fresh" above |
| --- | ---: | ---: | ---: | ---: | --- |
| cached-sustained (500 conn) | 9,673 | 9,304 | 10,152 | 9,710 | −0.5 % |
| cached-single (1 conn) | 6,464 | 6,365 | 6,213 | 6,347 | −5.9 % |
| cached-spike (200 conn) | 9,486 | 9,801 | 9,997 | 9,762 | −4.5 % |
| static, 1 KB (200 conn) | 15,068 | 15,727 | 15,640 | 15,478 | −0.9 % |
| flood (2,000 conn) | 8,805 | 9,242 | 8,799 | 8,949 | −2.0 % |
| browsers | 9,634 | 9,536 | 9,754 | 9,641 | −1.2 % |
| uncached (10 conn) | 6.82 | 6.75 | 6.89 | 6.82 | −5.3 % |

**Reading:** level with the verified build f2cc311 above (cached-single 6,699, uncached 6.8), so
within this box's run-to-run spread; no error in any scenario but the flood's expected refused
connections.

## v0.1.0 from the public installer on a reimaged box

The released v0.1.0, installed with `curl -fsSL https://cloudground-it.github.io/cloudground/install.sh | bash`
(the script get.cloudground.it serves) on the same VPS reimaged to stock Ubuntu 26.04: clean install
in 1m 24s, the full e2e (1289 passed; the one failure, W3's panel-port allow rule, was the check
assuming a rule an earlier install had left, fixed in #8), then three suites.

| Scenario | run 1 | run 2 | run 3 | Mean | vs the purged box above |
| --- | ---: | ---: | ---: | ---: | --- |
| cached-sustained (500 conn) | 9,364 | 9,888 | 9,993 | 9,748 | +0.4 % |
| cached-single (1 conn) | 6,780 | 6,861 | 6,948 | 6,863 | +8.1 % |
| cached-spike (200 conn) | 10,228 | 10,071 | 9,972 | 10,091 | +3.4 % |
| static, 1 KB (200 conn) | 14,877 | 14,489 | 15,033 | 14,800 | −4.4 % |
| flood (2,000 conn) | 9,248 | 8,810 | 9,226 | 9,095 | +1.6 % |
| browsers | 9,855 | 9,796 | 9,409 | 9,687 | +0.5 % |
| uncached (10 conn) | 7.02 | 7.09 | 7.09 | 7.07 | +3.7 % |

**Reading:** the release as users get it performs like the release candidate, within this box's
run-to-run spread. Run 2's flood also had 499 timeouts besides the expected refused connections;
no other scenario had an error. `cgctl uninstall --remove-sites --purge-packages` then left no
CloudGround file, unit, user or data-plane package; the base tools the installer added (bzip2,
unzip) stay by design.
