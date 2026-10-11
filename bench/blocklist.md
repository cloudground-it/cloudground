# The block list: what it costs cached traffic (issue #21)

**Status: to measure on the VPS.** The design keeps the cost on cached traffic close to nothing;
these are the measurements that have to confirm it, with the commands, before the numbers below
replace the dashes. Same reference box and method as `site-resources.md`.

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

| Scenario | none | empty | full | empty vs none | full vs none |
| --- | ---: | ---: | ---: | ---: | ---: |
| cached-sustained (500 conn) | – | – | – | – | – |
| cached-single (1 conn) | – | – | – | – | – |
| static, 1 KB (200 conn) | – | – | – | – | – |

**Bound:** within this box's measured spread (bench/README.md: up to 9 % for one build alone over
six runs); the verdict line of `cgbench compare` decides.

## 2. The WordPress vhost's failure log

The bench site is WooCommerce, so its vhost carries the new `access_log … if=$cg_auth_failed` and
`fastcgi_hide_header`. Main before #21 against this branch, alternated:

```sh
bench/cgbench ab -a /root/cg-main -b /root/cg-blocklist -rounds 3 -scenarios cached-sustained,cached-single,browsers https://bench.<ip>.sslip.io
```

| Scenario | main | #21 | change |
| --- | ---: | ---: | ---: |
| cached-sustained | – | – | – |
| cached-single | – | – | – |
| browsers | – | – | – |

## 3. Reading and enforcing

| | How | Result |
| --- | --- | --- |
| apply of a 10,000-entry list | `time` of one `BlocklistApply` through the agent's socket | – |
| the agent's CPU while sshd is under a guessing run (200 failed logins a minute from 20 addresses) | `cpu.stat` of `cloudground-agent.service` over 10 minutes | – |
| the panel's CPU for the same | `cpu.stat` of `cloudground-api.service` | – |
