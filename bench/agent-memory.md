# The agent's memory limit, and its helpers' (issue #66)

**Status: measured once (§4, 2026-10-12); the values stay until a second run on another day.**
The values in `cloudground-agent.service` and `host.HelperMemoryMax` were chosen from what the code
bounds, on the generous side, so that they cannot cut work that runs today. The first run of §3
puts the agent's own memory far below them; §3's rule keeps them until a second run on another day
gives the same peaks.

## 1. What changed

#42 bounded the output the agent captures. A `MemoryMax=` on `cloudground-agent.service` is the
second guard, but restic, `mariadb-dump`, the `mariadb` client and certbot ran as the agent's
children, in its cgroup: a cap there would also have capped them, and a large backup or restore
would have been killed. They now run each in a transient unit of its own, in `cg.slice/cg-helper.slice`
(host SPEC: helpers), with the agent's sandbox, line for line, and a limit of their own. The agent's
cgroup keeps the agent and its small children (`nginx -t`, ufw, the account tools, apt-get).

| Unit | Setting | Value |
| --- | --- | --- |
| `cloudground-agent.service` | `MemoryHigh` | 768M |
| | `MemoryMax` | 1G |
| | `OOMPolicy` | continue |
| `cg-helper-<tool>-<n>.service` | `MemoryMax` | 75% (of the machine's memory) |

## 2. Why these values, until measured

What lives in the agent's cgroup, from the code's own bounds:

| Part | Bound | Where |
| --- | --- | --- |
| the Go process at rest | not known: to measure (§3, idle) | |
| a captured stdout | 16 MiB per command (`StdoutLimit`) plus 64 KiB of stderr | host SPEC: exec |
| restic's kept output | 16 MiB per run (`ResticOutputLimit`), still in the agent's heap | host SPEC: restic |
| requests in flight | at most 64 (`agentrpc.DefaultMaxInFlight`) | agentrpc |
| Quarry's 1 GB export and import | the agent's RSS grows by less than 192 MiB (an e2e check) | e2e `F4b Quarry` |
| children still in the cgroup | `nginx -t` (parses every vhost), apt-get and dpkg (a PHP extension), ufw (Python), journalctl: tens to a couple of hundred MB, one or two at a time | |
| page cache of the files the agent writes (restores, releases, staging copies) | reclaimable: past `MemoryHigh` the kernel reclaims it before anything is killed | |

A busy agent, added up generously: about 60 MiB at rest, 192 MiB for a large Quarry transfer, four
jobs at once each holding 32 MiB of output, 200 MiB for apt-get, 50 MiB for `nginx -t`: about
630 MiB. **`MemoryHigh=768M`** is above that: the kernel starts to reclaim and throttle there, and
does not kill. **`MemoryMax=1G`** is the hard stop a third higher, for a leak, not for normal work.
On the smallest supported server (1 GB) it does not bite before the machine runs out; it is a
backstop, and §3 decides whether it can be lower.

**`OOMPolicy=continue`.** systemd's default for a service (`stop`) stops the whole unit when the
kernel kills any process of it. A child killed for memory (`nginx -t`, apt-get) must fail its own
request only; if the agent itself is killed, the unit fails and `Restart=on-failure` brings it back.

**The helpers' 75 %.** restic's memory grows with the repository's index, the `mariadb` client's
with the largest statement of a dump; neither has a bound in the code. Without a limit, a runaway
helper is found by the kernel's global OOM killer, which may pick a site's PHP or MariaDB instead.
With three quarters of the machine as its own limit, a helper that needs more than that is killed in
its unit first (swap is not limited, so a helper that needs swap still gets it), and every backup
that fits in memory at all still runs.

Considered and not set until measured: `GOMEMLIMIT` for the agent (a soft heap limit that makes the
Go collector work harder near `MemoryHigh`); `MemoryHigh` on the helpers.

## 3. To measure on the VPS

On the reference OVH VPS (2 vCPU, 4 GB, Ubuntu 26.04), a clean box with the bench site of
`setup-workload.sh` and a few small sites, the release under test installed. `agent-memory.sh`
samples the agent's cgroup (`memory.current`, its anon and file parts, `memory.events`), the agent's
VmRSS and the helpers' slice every second and prints the peaks. Restart the agent before each
scenario (`memory.peak` is the cgroup's lifetime peak).

```sh
sudo systemctl restart cloudground-agent
sudo bash bench/agent-memory.sh idle 600          # 1. ten minutes at rest, the scheduler running
sudo systemctl restart cloudground-agent
sudo bash bench/agent-memory.sh busy &            # 2. busy, then Ctrl-C (or kill -INT) when done
#    at once: a full backup of every site, a staging clone of the bench site, a WordPress update,
#    three site creations, a PHP extension install (apt-get), certificate renewals, and the
#    dashboard open (usage collection every few seconds)
sudo systemctl restart cloudground-agent
sudo bash bench/agent-memory.sh large-restore &   # 3. a site with 2 GB of files and a 1 GB
#    database: a backup, its restore test, an in-place restore; then Quarry's 1 GB export and import
```

Record, per scenario: the agent's peak `memory.current` and its anon part, VmRSS, the
`memory.events` counters, and the helpers' peak. Then:

- `memory.events` `high` must stay 0 in every scenario: a non-zero count means `MemoryHigh`
  throttled normal work, and is too low.
- The new `MemoryHigh` is the largest anon peak of the three, times four, rounded up to a multiple
  of 128 MiB, and never below 256 MiB; `MemoryMax` a third above it. If that is above 768M/1G, the
  measured values replace these; if below, they replace them only after a second run on another
  day gives the same peaks (AGENTS.md rule 6: a result that does not repeat is not shipped).
- The helpers' peak (restic on the 2 GB site, the `mariadb` client on the 1 GB dump) says how far
  below 75 % the largest backup stays; record it beside the limit.
- e2e `helper units` checks the mechanism (the helpers' cgroups, a helper killed for memory while
  the agent lives on) on every run; this file records the numbers.

## 4. First run (2026-10-12)

The reference OVH VPS (2 vCPU, 4 GB, Ubuntu 26.04, CPU parity 2.2 s, no steal), main at 85da91c
(the v0.3.0 candidate before the release-candidate fixes, none of which touches the agent's
memory), installed on the purged box. The bench site of `setup-workload.sh` and three small
WordPress sites; a local restic repository. The agent restarted before each scenario, the
scenario driven through the panel's API.

| Scenario | What ran | agent `memory.current` peak | of which anon | agent VmRSS | `memory.events` high / max / oom_kill | helpers' slice peak |
| --- | --- | ---: | ---: | ---: | --- | ---: |
| idle, 600 s | the scheduler, nothing queued | 14.7 MiB | 12.8 MiB | 25.7 MiB | 0 / 0 / 0 | 361 MiB |
| busy, 76 s | a backup with its restore test of all five sites, the staging copy of the bench site made again, a WordPress update of everything, a PHP extension (gmp, apt-get), three site creations, the dashboard's usage polled every 3 s | 342.3 MiB | 20.2 MiB | 31.1 MiB | 0 / 0 / 0 | 405 MiB |
| large-restore, 781 s | a site with 2.1 GB of random files and a 1 GiB table: backup and restore test (133 s), verify (79 s), full restore in place (196 s); then the e2e section `F4b Quarry` (a 1 GB export, download, upload and import, 372 s, 42 checks passed) | 768.2 MiB | 62.9 MiB | 77.8 MiB | 9,767 / 0 / 0 | 1,794 MiB |

`certbot` was not exercised: a certificate request needs the ACME email, which the bench box does
not have (e2e `wordpress certificates` covers the mechanism). The helpers' slice peak at rest is
page cache left charged to it by the backups that ran before.

**Reading.**

- **The agent itself is small.** Its anonymous memory peaked at 63 MiB, during Quarry's 1 GB
  transfer (the e2e's own sample: 24 MiB before, 78 MiB peak), and stayed under 21 MiB otherwise.
- **The cgroup fills with page cache, and that is what `high` counts.** A restore in place writes
  2 GB through the agent's cgroup, so its `file` part grows to `MemoryHigh` and the kernel reclaims
  it there: every one of the 9,767 `high` events came with anon under 63 MiB. §3's rule ("high must
  stay 0") cannot hold for any `MemoryHigh` below the size of a restore, and it is not the signal it
  was meant to be. Whether the reclaim slows the work was measured: the same full restore in place
  of the large site, alternated, after a cache drop:

  | `MemoryHigh` | round 1 | round 2 | `high` events per restore |
  | --- | ---: | ---: | ---: |
  | 768M (shipped) | 177 s | 184 s | 5,533 / 5,613 |
  | infinity | 185 s | 180 s | 0 |

  No cost: the reclaim is of clean cache. The rule to read is therefore anon, with `max` and
  `oom_kill` at 0.
- **The helpers.** restic's restore of the 2 GB site is the helpers' peak, 1,794 MiB with its page
  cache, against a limit of 2,860 MiB (75 % of 3,814 MiB); nothing was killed.
- **What §3 would set.** The largest anon peak, 62.9 MiB, times four is 252 MiB: `MemoryHigh=256M`
  (the floor) and `MemoryMax` a third above, 342M. That is below the shipped 768M/1G, so by §3's own
  rule it waits for a second run on another day; until then the unit keeps 768M/1G, which cost
  nothing measurable here.
