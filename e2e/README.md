# e2e: end-to-end suite

The end-to-end suite. It runs **on a CloudGround server, as
root**, and drives the real product: the panel API over its Unix socket
(`/run/cloudground/api.sock`), the hosted sites over HTTPS on 127.0.0.1 (as
`curl --resolve` did), and sftp, wp-cli, mariadb, restic, nginx, systemctl and
journalctl on the box. It creates sites, asserts on what they serve and how
the system behaves, and deletes them again.

Run it only on a throwaway server (see `AGENTS.md`, hard rules 1 and 2). It
changes the backup repository setting for the length of the run, restarts
the API, plants files in site trees and puts a fake `mariadb-dump` on the
agent's PATH for a few minutes. All of it is undone at the end, also when a
check fails.

The suite sits behind the `e2e` build tag: `go test ./...` and `go vet ./...`
only build the pure parsers in `parse.go` and `jar.go` and their unit tests.

## Build

```sh
make e2e-bin          # with Go installed: dist/cg-e2e, linux/amd64
make docker-e2e-bin   # without Go, in the golang:1.26 image
```

Both run `go test -c -tags e2e -o dist/cg-e2e ./e2e`. `make vet` and
`make docker-test` also vet the tagged files, so the suite keeps compiling.

## Copy and run

```sh
scp dist/cg-e2e root@<server>:/root/
ssh root@<server>
IP=<public-ip> PANEL_EMAIL=<admin email> PANEL_PW=<admin password> ./cg-e2e -test.v
```

The inputs are the bash suite's. When `PANEL_EMAIL` or `PANEL_PW` is unset,
it is read from `/root/cloudground-admin.env`, which on a CloudGround box holds
exactly two lines, `PANEL_EMAIL=…` and `PANEL_PW=…` (`export`, quotes and the
aliases `ADMIN_EMAIL`, `ADMIN_PASSWORD` are also accepted). So on the box this
is enough:

```sh
./cg-e2e -test.v
```

`IP` is not in that file. When it is unset, the suite uses the source address
of the default route, if that is a public IPv4 address (the case on a VPS
with its address on the interface). Behind NAT, or whenever the address the
panel and sites answer on differs, set `IP` explicitly; the suite stops and
asks for it when the route's address is private. (The bash suite always
requires `IP`.) Optional: `PANEL` (default `http://localhost`), `API_SOCK`, `PHPV`
(otherwise the newest `/usr/sbin/php-fpm8.*` binary).

Useful flags and variables:

- `-test.run '^TestE2E$/^security$'` runs one section, `'^TestE2E$/^(a|b)$'` a
  set (spaces as underscores). Every section runs on its own and gives the
  verdicts it gives in a full run: what it shares with other sections is a
  fixture it declares, made by whichever section asks first. `SECTIONS.md`
  lists the sections, their fixtures and what a section run alone does not
  reproduce.
- `-test.timeout` defaults to 4h here, not `go test`'s 10 minutes.
- `CG_E2E_PARALLEL=1` runs independent sections side by side, in lanes
  (`SECTIONS.md`), and the five site types at once; the sections that change
  or measure something every section shares run alone afterwards.
- `CG_E2E_TRACE=1` prints every command, request and file write to stderr.

The log ends with a line per section that ran (its time, passed and failed
checks), then `RESULT: <pass> passed, <fail> failed` and the failed checks,
like the bash suite. Exit status is non-zero if any check fails.

## Three workflows

A full run takes about 16 minutes. Most changes need a few sections, and a
new check needs proving only on the sections it is in. The two scripts run on
the workstation, in the checkout, and print the `-test.run` regexp to give
cg-e2e on the server; they explain on stderr which change selected what.
`printf %q` quotes the regexp for the remote shell (check names have
apostrophes in them).

**1. Per change: the affected sections, on the new build.**

```sh
R=$(scripts/e2e-affected.sh main) &&
  ssh root@<server> ./cg-e2e -test.v -test.run "$(printf %q "$R")"
```

`e2e/sections.map` maps the changed paths outside `e2e/` to sections (first
matching line wins; a path no line matches selects everything). A changed
line of `e2e/*.go` selects the sections that reach its function, from
`e2e/sections.index`, which `make e2e-index` regenerates (`make test` fails
while it is stale). Documentation, unit tests and `bench/` select nothing:
the script then prints nothing and exits 1, so an empty `-test.run` (which
would run everything) cannot slip through. `scripts/e2e-affected.sh --paths
< list` maps a list of paths instead of a diff.

**2. Per new check: the new checks, on the old build.** Every new check must
fail on the build that has the bug (AGENTS.md):

```sh
R=$(scripts/e2e-new-checks.sh main) &&
  ssh root@<server> ./cg-e2e -test.v -test.run "$(printf %q "$R")"   # old binaries installed
```

It finds the `h.check`/`h.assert`/`h.fail` calls the diff adds or changes
(and the helper calls that name checks after a string), and selects their
sections with only those checks reported and counted. The sections still run
whole, so this costs what the affected sections cost, not a full run;
`RESULT` should show the new checks failed. A check whose own call did not
change but whose inputs did is not found: use workflow 1 for those.

**3. At the end of a phase: the full suite.**

```sh
ssh root@<server> ./cg-e2e -test.v
```

This is still the gate before a phase is pushed, sequential, in the bash
suite's order. `CG_E2E_PARALLEL=1` is faster but adds load and interleaves
the sections; use it for a quick look, not for the gate.

Expected durations (estimates from the code and the 16-minute full run on
the test VPS; the per-section lines at the end of each run's log give the
measured ones, so replace these after the next full run):

| Workflow | Typical change | Estimate |
| --- | --- | --- |
| 1, one area | `internal/guard` → `wordpress safe push` (feature site and its staging copy, three pushes) | 3–4 min |
| 1, one area | `internal/agent/ops/db/console.go` → `woocommerce security`, `wordpress backups` (both fixtures) | 6–8 min |
| 1, one area | `internal/agent/ops/system/services.go` → `R-2.4 services` | under 1 min |
| 1, one e2e function | a change inside `feat.cronJobs` → `wordpress cron` | 2–3 min |
| 1, shared code | `internal/state`, `internal/api/panel_server.go`, the agent's wiring, `cmd/` → every section | full run |
| 2 | as workflow 1 for the sections of the new checks | same |
| 3, sequential | everything | about 13 min (was 16) |
| 3, `CG_E2E_PARALLEL=1` | everything | about 10 min |

Where the full run's time goes, and what was cut:

- Fixed by what is tested, not shortened: the 130 s request of `wordpress
  slow request`, the wait for cron's next minute in `wordpress cron` (up to
  a minute), the console statement `woocommerce security` lets run past the
  console's 60 s limit, and the tick 30 s after an API restart in `R-0.7
  reconcile` (one restart is kept, to check that a restart reconciles).
- `R-0.7 reconcile` ran a reconcile three times by restarting the API and
  waiting for the tick that follows (30 s, polled every 5 s); two of them now
  ask for the tick directly (`POST /api/system/maintenance`, admin only,
  audited), which answers when it is done. About a minute and a half less.
- The waits for a site to become active, a task to end and a site to be gone
  polled every 3 to 4 s; they poll every second, with the same timeouts. Some
  150 waits a run, about 2 s each.
- A few fixed sleeps became polls on the condition they waited for (the
  custom certificate being served, the access log showing a request, within
  the time the check always allowed). The ones left wait on something the
  suite cannot observe: nginx's 1 s log buffer before an offset is taken or
  read, `open_file_cache_valid` after a purge, the journal's one-second
  resolution around the R-0.7 window, and the timing of the agent-drain
  check.


## History

This suite replaced `scripts/e2e-verify.sh`. On 2026-10-09 both ran on the
same freshly upgraded test server and gave the same verdict, 211 passed and
0 failed, after which the bash script was deleted. `PARITY.md` keeps the
check-by-check mapping (bash line numbers refer to the script as of that
date) and the deliberate differences.

Every new check must fail on a build that has the bug and pass on the fix:
run it against the old binary before merging the fix (AGENTS.md).
