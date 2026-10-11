# bench — specification

What the benchmark tooling measures, how, and what it prints. The tool is `cmd/cgbench`. The shell
files in this folder are thin wrappers around it, so that the commands already in use keep working.
I wrote this document before the code, and the tests in `cmd/cgbench` check it.

## 1. The workload

`bench/setup-workload.sh` builds the site that every scenario hits. It runs on a CloudGround
server, as root, and talks to the panel through the API socket (`/run/cloudground/api.sock`).

- Input: `IP` (the public address), `PANEL_EMAIL`, `PANEL_PW`. Optional: `PHPV`, `API_SOCK`.
- Site: `bench.<ip with dashes>.sslip.io`, type `woocommerce`, profile `commerce`, object cache on,
  with the newest PHP 8.x that has an FPM binary in `/usr/sbin`. A bench site that already exists is
  reused, so running the script twice is safe.
- Content: at least 100 published posts and 50 simple products (`Bench product <n>`, price
  `10+n`), the default theme, and a 1,024-byte file at `/bench-static.txt`.
- The script writes files in the site tree as the site's own user. It never writes there as root
  and never runs `chown` (AGENTS.md, uutils).
- Output: the site id, the post and product counts, the WordPress, WooCommerce and PHP versions,
  and the line `target https://<domain>`.

## 2. Scenarios

Each scenario is one `wrk` run. Every run sends `Accept-Encoding: gzip` and asks for the latency
distribution (`--latency`). The page cache stores compressed entries, so a client that does not ask
for gzip would hit a different entry from the one browsers hit.

| Name | Path | Threads | Connections | Duration | Timeout | Script |
| --- | --- | ---: | ---: | --- | --- | --- |
| cached-sustained | cached | 4 | 500 | long | latency | |
| cached-single | cached | 1 | 1 | short | latency | |
| cached-spike | cached | 4 | 200 | short | latency | |
| flood | cached | 8 | 2000 | short | 5s | |
| static | static | 4 | 200 | short | latency | |
| browsers | cached | 4 | 200 | short | latency | `browsers.lua` |
| uncached | cached | 2 | 10 | short | latency | `uncached.lua` |

The table gives the run order. The summary uses a different order: cached-sustained, cached-single,
cached-spike, static, flood, browsers, uncached.

Defaults, each one also read from an environment variable:

| Setting | Variable | Default |
| --- | --- | --- |
| cached path | `PATH_CACHED` | `/shop/` |
| static path | `PATH_STATIC` | `/bench-static.txt` |
| long duration | `DURATION` | `60s` |
| short duration | `SHORT` | `30s` |
| latency timeout | `LAT_TIMEOUT` | `10s` |

Why each choice:

- **Timeout 10s on every scenario that reports latency.** When a request takes longer than its
  timeout, wrk counts it as a timeout error and leaves it out of the latency percentiles. A server
  slow enough to miss wrk's default 2s timeout can therefore show a *better* p99 than a server
  that answered everything slowly. On one measured run, a stack with a 385.90 ms p99 had dropped
  319 requests, and a stack with an 84.89 ms p99 had dropped 1. With a 10s timeout, slow requests
  finish and are counted. Even so, always read the error column next to the percentile.
- **The flood keeps 5s.** The flood measures how the server holds up, not its latency.
- **Static uses a 1 KB file we create, not a WordPress asset.** With a 59 KB file, loopback runs out
  of bandwidth before the server runs out of capacity, so the number would describe the link. A
  WordPress file would also change size from one WordPress version to the next.
- **The file-descriptor limit.** Before it starts wrk, the tool raises its own `RLIMIT_NOFILE` soft
  limit to 65535, or to the hard limit if that is lower. wrk inherits the new limit. With the usual
  1024, about 987 of the flood's 2,000 connections fail to open, and wrk reports them as connect
  errors. That looks like the server refusing load, but it is the client's own limit. If the tool
  cannot raise the limit, it prints a warning and goes on.
- **browsers.lua** sends traffic shaped like real visitors. It rotates four Accept-Encoding values
  (`gzip, deflate, br, zstd`; `gzip, deflate, br`; `gzip`; `br, gzip`). It cycles through six link
  suffixes, three empty and three with a tracking query string (`utm_*` from a newsletter,
  `fbclid`, `gclid`), so half the requests are tagged. A cache keyed on
  the raw header, or one that any query string bypasses, loses throughput here.
- **uncached.lua** gives every request a query string that no other request uses, in any thread,
  so every response comes from PHP and the database. The query string is `?bench=<thread>-<n>-<random>`:
  each thread has its own number, then comes a counter, then a random value seeded per thread.

## 3. Results

A suite run with label `L` writes to `<out>/L/` (`<out>` is `bench/results` by default, and git
ignores it):

- `<scenario>.txt`: wrk's output (stdout and stderr), whatever wrk's exit status was;
- `summary.tsv`: a header line, then one line per scenario that has a `.txt` file, in summary order:

```
scenario	req_per_sec	p50	p99	errors
```

- `req_per_sec` is the number on wrk's `Requests/sec:` line. `p50` and `p99` are the values on
  the ` 50%` and ` 99%` lines of the latency distribution, exactly as wrk printed them (`55.30ms`,
  `1.43s`, `250.00us`). A value that is missing is written as `na`.
- `errors` is the text after `Socket errors:` (`connect 0, read 0, write 0, timeout 12`), or
  `none` when wrk printed no such line.
- After the table, the tool prints a warning for each scenario where wrk reported non-2xx or
  non-3xx responses. A fast error page is not a fast site.

The table goes to stdout, then the line `raw output in <dir>`. Before it starts, the tool prints
`== L against <target> ==`, and then each scenario's name as it runs.

If wrk hangs, it is killed after its duration plus two minutes. The scenario's file then holds
whatever wrk printed, so its row reads `na`.

## 4. Repetitions and A/B

A **round** is one suite run, which can be limited to some scenarios with `-scenarios`. Runs of the
same build on the test VPS have differed by up to 28 % (bench/final-review.md). A single run is
therefore not evidence, and comparisons are made like this:

- `cgbench repeat` runs the same build N times, into `<out>/L/run<i>/`.
- `cgbench ab` alternates two installed builds: install A, run, install B, run, and so on for N
  rounds, into `<out>/L/A<i>/` and `<out>/L/B<i>/`. A build is a checkout directory that holds
  `dist/` (or the `dist/` directory itself). It is installed with
  `CLOUDGROUND_LOCAL_BUILD=<dist> bash installer/install.sh`, run from the checkout, with stdin
  closed. The log goes to `install-A<i>.log`. `-install-cmd` replaces the command. It runs
  under `bash -c` with the same environment, plus `CG_CHECKOUT` set to the checkout directory. An
  install that fails stops the run.
- After each install, the tool waits until the cached path answers 200 (up to `-ready`, 3 minutes
  by default). Then it waits `-settle` (10s) and runs a warm-up of `-warmup` (10s, 50 connections),
  whose result it throws away. The first run after an install fills the disk cache, which is why
  the old method discarded it.
- Before each run, the tool measures CPU parity (§5) into `parity.txt` in the run directory.
- `cgbench compare <dir>` builds the report again from a directory that `ab` or `repeat` wrote.
  `cgbench compare -a d1,d2 -b d3,d4` does the same from any `summary.tsv` directories.

## 5. CPU parity

`cgbench parity` (wrapper: `bench/cpu-parity.sh`) checks that the box has not changed between runs.
It prints:

```
cpu_loop_seconds <s.ss>
steal_percent <n>
model <cpu model>
cores <n>
```

- The loop is the same awk program as in the historical records, with `LOOPS` iterations (default
  20,000,000) of `s += i % 7`. Its time is the wall time of the awk process, with two decimals.
  The awk loop stays so that new numbers can be compared with old ones (2.07–2.38 s on the test
  VPS).
- Steal is the share of CPU time stolen by the hypervisor (`/proc/stat`), sampled over 2 seconds
  and rounded to an integer. If it cannot be read, it is `unknown`.
- Exit status 1, with a message, when steal is not zero: the box shares its CPU, and its numbers
  cannot be compared.

## 6. The report and the verdict

For each scenario, using req/s, with A the reference build (the "before" build):

- **runs**: the req/s of every run, in the order they ran (A1, B1, A2, B2, …).
- **mean change**: `(mean(B) − mean(A)) / mean(A)`, in percent. This is how the historical tables
  compute "Mean change" (bench/phase-3.md, bench/phase-4-integration.md).
- **spread** of a build: `(max − min) / mean` of its own runs, in percent. It is the noise you
  can see between runs of the same code on the same box. A build needs two or more runs for a
  spread.
- **noise** is the larger of the two spreads, and never less than `-floor` (default 0).
- **verdict**:
  - `within noise` when |mean change| ≤ noise;
  - `regression beyond the measured spread` when the change is below −noise;
  - `improvement beyond the measured spread` when it is above +noise;
  - `not enough runs` when either build has fewer than two valid runs. A run without a number
    (`na`) is not valid.
- A scenario whose runs logged socket errors gets a `*` in the table. Its note lists the error
  lines, because a scenario with timeouts reports percentiles only for the requests that were
  served.

The overall verdict:

- `regression beyond the measured spread: <scenarios>` when any scenario regressed. The tool then
  prints the command that repeats only those scenarios, with more rounds, before anyone draws a
  conclusion. A spread from two runs is a weak estimate. On 2026-10-10, cached-sustained read
  −4.8 % in two rounds and −2.3 % in six alternated runs, with one build alone spreading 9 %.
- otherwise `not enough runs` if any scenario lacked runs;
- otherwise `change within noise`.

`ab` and `compare` print the report as a Markdown table, in the shape of the bench/phase-*.md
files, and write it to `report.md`. They also write `runs.tsv`, one line per run and scenario:
`scenario build round req_per_sec p50 p99 errors`. The exit status is 0 whatever the verdict. The
verdict is information, not a gate.

## 7. Wrappers

- `bench/wrk-suite.sh <target> [label]` runs `cgbench suite -out bench/results <target> [label]`.
  Its output and files are those of §3.
- `bench/cpu-parity.sh` runs `cgbench parity`.
- Both look for the binary at `$CGBENCH`, then at `dist/cgbench` in the checkout, then on `PATH`,
  and finally fall back to `go run ./cmd/cgbench` when Go is installed. `make dist` and
  `make docker-dist` build `dist/cgbench` (linux/amd64). The installer does not install it.
