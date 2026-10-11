# Benchmarks

Speed is the claim CloudGround makes, so speed gets measured. A change to caching, request
handling or the tuning defaults arrives with numbers from before and after (AGENTS.md, rules 5
and 6). This folder holds what you need for that. The exact behaviour of each part is in
[SPEC.md](./SPEC.md).

| File | What it is |
| --- | --- |
| `setup-workload.sh` | builds the bench site on a CloudGround server |
| `wrk-suite.sh` | runs every scenario once and prints the summary table |
| `cpu-parity.sh` | checks that the box is still the box it was |
| `cgbench` | runs `cmd/cgbench`: suite, repeat, ab, compare, parity |
| `baseline.md`, `phase-*.md`, `final-review.md` | measurements taken during the rewrite |
| `site-resources.md` | per-site limits' defaults and the cost of the usage collection (issues #18, #19) |
| `uncached-php.md` | where an uncached render's time goes, and the PHP-FPM candidates A/B (issue #10) |

No results are published yet: the benchmarks page comes once the whole suite has run on a
CloudGround release.

## Before you trust a number

**The box is the biggest variable.** In earlier measurements, two VPS instances sold as the
same plan were 2.5 times apart on a fixed CPU loop, and another pair differed by 16 %. Those gaps
are bigger than most of the effects we measure. So two builds, or two products, are compared on
one machine, one after the other, and never on two machines or at the same time. Two stacks on
one box at once would fight over CPU, memory, disk and port 443.

**The same box drifts too.** On the 2 vCPU test VPS, two runs of the same build have differed by
up to 28 % on cached-sustained. A full e2e run beforehand left a box with nine sites that read
8–10 % slow. That is why:

- you run `cpu-parity.sh` before and after, and treat any non-zero steal as a void run;
- you clean the box (no e2e leftovers) before you measure;
- you never compare a single run with another single run. Alternate the two builds and look at
  how far apart the runs of the *same* build are. `cgbench ab` does this for you.

**Read the error column with the percentiles.** When a request takes longer than its timeout,
wrk counts it as an error and leaves it out of the latency figures. A server that fails to answer
can therefore post a better p99 than one that answers slowly. Every scenario that reports latency
uses a 10 s timeout so that slow answers are still counted. The report marks any scenario that had
socket errors.

**The flood decides nothing on a small box.** Two thousand connections on loopback, with wrk on
the same two cores, swing by ±20 % between identical runs (phase-4.1.md). Use a separate load
generator, bigger than the target, before you accept or reject a change on the flood number.

## Running it

On the server, once:

```sh
sudo PANEL_EMAIL=... PANEL_PW=... IP=<public-ip> bash bench/setup-workload.sh
```

`make docker-dist` builds `dist/cgbench` next to the panel binaries. Copy `dist/` together with
the checkout, and the wrappers will find it.

One build, one pass, the way the phase records were taken:

```sh
bash bench/cpu-parity.sh
bash bench/wrk-suite.sh https://bench.<ip>.sslip.io phase5
bash bench/cpu-parity.sh
```

The same build several times, to see its spread:

```sh
bench/cgbench repeat -n 3 -scenarios cached-sustained -duration 30s https://bench.<ip>.sslip.io
```

Two builds, alternated. Put each checkout on the box with its own `dist/` and `installer/`.
`cgbench` installs A, runs, installs B, runs, for as many rounds as you ask:

```sh
bench/cgbench ab -a /root/cg-main -b /root/cg-next -rounds 2 https://bench.<ip>.sslip.io
```

At the end you get a table in the shape of the `phase-*.md` records, with every run, the mean
change, each build's own spread, and a verdict per scenario. You also get an overall verdict:
"change within noise", or "regression beyond the measured spread" with the scenarios involved.
A spread taken from two runs is a thin estimate. When something is flagged, run the command the
tool prints, which repeats only those scenarios with more rounds, before you conclude anything.
On 2026-10-10, cached-sustained read −4.8 % over two rounds and −2.3 % over six runs. In the six
runs, the reference build alone spread 9 %.

`bench/cgbench compare bench/results/<label>` rebuilds the report from saved runs, and so does
`-a dir,dir -b dir,dir` for any `summary.tsv` directories.

## Loopback or a second machine

The phase records are loopback: wrk runs on the target. The generator takes cores from the server,
so the absolute numbers understate the box. But both builds carry the same handicap, and the
network plays no part. A generator in the same region, larger than the target, gives numbers
closer to what a visitor sees. Upstream, it raised the cached and static figures by a third or
more and left the uncached figure unchanged, since that path is bound by PHP and not by the
network. Label which kind each run is, and compare loopback with loopback and network with
network.

## What the load generator cannot see

For a published comparison, also record, on the target:

| Metric | How |
| --- | --- |
| Install time | time the installer from a fresh OS |
| Disk and RAM added | measure before the install and at idle after |
| Peak memory and swap | sample `free -m` during the suite |
| Services and PHP versions | `systemctl list-units --type=service`; `ls /usr/sbin/php-fpm*` (a failed provision can leave a stale `/etc/php/<v>`) |
| Database load | deltas of `SHOW GLOBAL STATUS LIKE 'Questions'` |
| Object-cache flush | change an option with wp-cli, and time how long until the site serves it |

## Comparing with another product

The same box, reinstalled between stacks, in the order CloudGround, other, CloudGround, so that
drift shows up. The same WordPress, WooCommerce, theme and data on both, from the same archives,
with SHA-256 checked. The same PHP and database versions. CloudGround on its shipped defaults, and
the other product tuned as well as its own documentation allows: beating an untuned stack proves
nothing.

## Publishing

Publish every number together with the versions, the date, the hardware and the configuration
that produced it. Publish the rows where CloudGround loses next to the rows where it wins. The
claim is "faster under this benchmark and this configuration", never "always faster". If a re-run
gives different numbers, the published page is what gets corrected.
