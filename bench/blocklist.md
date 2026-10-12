# The block list and site rules: what they cost cached traffic (issues #21, #25)

**Status: measured on 2026-10-12.** The design keeps the cost on cached traffic close to nothing,
and the measurements below confirm it. Same reference box and method as `site-resources.md`: the
OVH VPS (2 vCPU, 4 GB, Ubuntu 26.04, CPU parity 2.07–2.43 s, no steal), main at 85da91c (the v0.3.0
candidate), the bench site of `setup-workload.sh`.

## What a cached request pays, by design

| Where | What | When |
| --- | --- | --- |
| netfilter, `inet cloudground` input chain (internal/blocklist SPEC §5) | `iif "lo"` compare, then up to four interval-set lookups on the source address (allow4/allow6, block4/block6) | every packet that is not loopback, blocked or not |
| nginx, a WordPress or WooCommerce vhost (internal/engine SPEC §4.6) | the `$cg_auth_failed` map: one hash lookup on the request method, at log time | every request of those vhosts, cached ones included |
| nginx, the same vhosts | one syslog datagram to the agent | only a POST WordPress answered as a failed sign-in |
| sshd, the panel | nothing: the readers are the agent's (the journal) and the panel's own sign-in | — |

No other site type changes, and the static location (`access_log off`) is not affected.

## 1. The netfilter table (`bench/blocklist-cost.sh`)

Three arms alternated on one box, `rounds` times (3 by default): no table (the build before #21),
the table with nothing blocked (a quiet server), and the table holding 10,000 blocked addresses,
1,000 blocked /24s and 100 allowed /16s (far more than a busy server collects in a day). The load
comes from a network namespace on the box (`198.18.78.2` behind a veth pair): from loopback the
table's first rule accepts every packet and the lookups would not be measured. The panel's API is
stopped for the run so that it does not put its own list back.

```sh
bash bench/cpu-parity.sh
sudo bash bench/blocklist-cost.sh https://bench.<ip>.sslip.io 3
bash bench/cpu-parity.sh
```

Means of three alternated rounds (req/s), from the network namespace:

| Scenario | none | empty | full | empty vs none | full vs none |
| --- | ---: | ---: | ---: | ---: | ---: |
| cached-sustained (500 conn) | 9,348 | 9,187 | 9,101 | −1.7 % | −2.6 % |
| cached-single (1 conn) | 5,843 | 5,891 | 5,887 | +0.8 % | +0.8 % |
| static, 1 KB (200 conn) | 14,619 | 14,793 | 14,482 | +1.2 % | −0.9 % |

`cgbench compare` reads "within noise" for every scenario of both pairs (the "none" arm alone
spread 3.1 %, 9.1 % and 3.7 %). No run had an error.

**Bound:** within this box's measured spread (bench/README.md: up to 9 % for one build alone over
six runs); the verdict line of `cgbench compare` decides.

## 2. The WordPress vhost's failure log

The bench site is WooCommerce, so its vhost carries the new `access_log … if=$cg_auth_failed` and
`fastcgi_hide_header`. Main before #21 against this branch, alternated:

```sh
bench/cgbench ab -a /root/cg-main -b /root/cg-blocklist -rounds 3 -scenarios cached-sustained,cached-single,browsers https://bench.<ip>.sslip.io
```

The A/B of two builds could not run: #21 brought the state database to schema 27, and the build
before it refuses to start on it (`the state database is at schema 27, newer than this build's
26`), as it should. So the two lines were measured alone, on the candidate: the bench site's vhost
as rendered against the same file without the `access_log … if=$cg_auth_failed` and
`fastcgi_hide_header` lines, alternated, each arm put in place with `nginx -t` and a reload (the
reconcile did not rewrite the file in between: checked after every run).

| Scenario | without | with #21's lines | change |
| --- | ---: | ---: | ---: |
| cached-sustained | 9,166 | 8,838 | −3.6 % |
| cached-single (three rounds) | 6,356 | 5,985 | −5.8 % |
| cached-single (six rounds, again) | 6,306 | 6,247 | −0.9 % |
| browsers | 8,895 | 8,991 | +1.1 % |

Within noise in every row (the arms alone spread up to 5.6 % on cached-sustained and 9.4 % on
cached-single). cached-single read −5.8 % over three rounds, so it was run again over six: −0.9 %.

## 3. A site's refused visitors (#25)

The bench site with rules against the same site without, alternated (`cgctl sites block`, then
`cgbench repeat`; loopback is fine here: the rules are nginx's, and the server's own addresses
are exempt only from being refused, the lookups still run for them). Three rule sets: 500
networks; 50 user agents; the countries CN and RU (about 20,000 prefixes) — and, for memory and
reload, US alone (about 200,000 prefixes).

```sh
cgctl sites block <id> '{}'                                   # arm: none
cgctl sites block <id> '{"addresses": [...500 networks...]}'  # arm: addresses
cgctl sites block <id> '{"user_agents": [...50 patterns...]}' # arm: user agents
cgctl sites block <id> '{"countries": ["CN", "RU"]}'           # arm: countries
bench/cgbench repeat -n 3 -scenarios cached-sustained,cached-single https://bench.<ip>.sslip.io <arm>
```

Means of three alternated rounds (req/s), loopback:

| Arm | cached-sustained | cached-single | vs none |
| --- | ---: | ---: | ---: |
| none | 9,519 | 6,207 | |
| 500 networks | 9,310 | 6,418 | −2.2 %, +3.4 % |
| 50 user agents | 9,333 | 6,113 | −2.0 %, −1.5 % |
| CN + RU (rounds 2–3) | 9,224 | 6,238 | −3.8 %, +1.9 % |

Every arm reads "within noise" (the "none" arm alone spread 2.5 % and 7.7 %). In round 1 the
country rules were not applied: the first download of DB-IP's file timed out after its two minutes
(`could not download the country data from db-ip.com`, HTTP 503, the site kept the user-agent
rules it had, as ops/blocklist SPEC §4 says), and it succeeded seven minutes later; the CN + RU row
therefore uses rounds 2 and 3, against the same rounds of "none".

| US alone | Result |
| --- | --- |
| prefixes in the rules file, its size | 252,915 prefixes, 6.2 MB |
| `nginx -t` and reload time | `nginx -t` 0.22 s (peak RSS 65 MiB); `systemctl reload` 0.24 s |
| nginx master and worker RSS added | +50 MiB PSS in all, on a fresh master (11 → 61 MiB over the master, three workers and the cache manager, which share it copy-on-write); the master's own RSS goes 5 → 55 MiB. After several reloads with the rules on, the master's RSS had grown to 120 MiB: each reload builds the tree again and the old heap is not returned |
| the rules' save (`PUT …/blocking`: the agent reads the country file, renders, tests, reloads) | 1.8–1.95 s, against 1.0–1.1 s for an empty rule set |

## 4. Reading and enforcing

| | How | Result |
| --- | --- | --- |
| apply of a 10,000-entry list | `time` of one `BlocklistApply` through the agent's socket | 0.097–0.102 s (three runs; 10,000 elements in `block4`, 100 allowed); an empty list 0.014 s |
| the agent's CPU while sshd is under a guessing run (200 failed logins a minute from 20 addresses) | `cpu.stat` of `cloudground-agent.service` over 10 minutes | 2,579 ms, against 1,779 ms at rest: +0.8 s in 10 minutes for 3,966 sshd failure lines, 0.13 % of one core |
| the panel's CPU for the same | `cpu.stat` of `cloudground-api.service` | 477 ms, against 317 ms at rest |

The guessing run came from a network namespace (`198.18.79.10–29`), allowlisted so that the
addresses kept failing instead of being blocked after three tries: the reader still parses and
counts every line.
