# `internal/agent/ops/cron` — a site's scheduled jobs (specification)

The agent owns each site's schedule. The API stores the jobs and sends them
as typed values (`cronjob.Job`: id, schedule, command, enabled); the agent
validates them, writes the site's root-owned cron.d file and its runner
spec, and runs every job, scheduled or now, in the site's sandbox (§3). No
shell text crosses the socket.

## 1. Methods

| Method | Params → Result | Table row |
|---|---|---|
| `ApplyCron` | `CronParams{Site, Jobs}` → `CronResult{Entries}` | Sites, `LockLiveSite`, 2 min |
| `RunCron` | `RunCronParams{Site, Job, MaxSeconds}` → `RunCronResult{Status, Output, ExitCode}` | Sites, no lock, 7 min |
| `CronRuns` | `CronRunsParams{Site}` → `CronRunsResult{Runs: {<job id>: {At, Status, ExitCode}}}` | Sites, read-only, 1 min |

`RemoveSite(ctx, systemUser)` is called by site deletion, not over the
socket.

## 2. Validation (`internal/cronjob`, shared with the API)

The API runs the same checks before it stores a job, so the operator gets a
400 instead of a job that breaks the site's crontab later. The agent checks
again: it does not trust the API.

- **Schedule:** exactly five fields separated by spaces or tabs, only the
  characters `0-9 * , - /` and blanks. A field is a comma list of `*`, `N` or
  `N-M`, where `*` and a range may carry a step `/S`, `S ≥ 1` (cron refuses
  `/0`; a step beyond the range is legal and runs once).
  Ranges: minute 0–59, hour 0–23, day 1–31, month 1–12, weekday 0–7. `N-M`
  needs `N ≤ M`. Refused: names (`mon`, `jan`), `@daily` and friends, `N/S`
  (Debian's cron rejects it, although other crons accept it), anything with
  a newline. The rendered schedule has single spaces.
- **Command:** not blank, valid UTF-8, no control characters (tab allowed).
- **Length:** the rendered command field must be at most **998 bytes**.
  Measured on Ubuntu 24.04 and 26.04 (cron 3.0pl1): crontab(1) installs 998
  and refuses 999 with "command too long" — refusing the **whole crontab**,
  which stopped every job of the site. Overlong jobs are refused one by one
  instead, with the number of bytes to cut.
- **Job id:** positive; ids are unique within a request.

A failure is `invalid` with a message that names the job.

## 3. The schedule: a root-owned cron.d file and a runner

A job's command is the site's code: it runs where the site's PHP runs, in
a transient systemd service with the PHP master's sandbox
(`phpfpm.Sandbox`) in the site's slice — scheduled or run now. Cron only
says *when*. (R-4.5 review: scheduled jobs used to run from the site user's
crontab, outside any sandbox, as a dumpable process of the site's uid.)

`Apply` writes two files, both root's, through `rootfile` (temporary file,
rename):

- **the spec**, `/etc/cloudground/cron/<user>.json` (0600, its directory
  0700): `{user, domain, root, wordpress, jobs: {id: command}}`, the enabled
  jobs' commands verbatim. Written first, so a new line finds its job;
- **the cron.d file**, `/etc/cron.d/cloudground-<user>` (0644; the name has
  only letters, digits and dashes, as cron requires there):

```
# CloudGround manages this file: change the site's cron jobs in the panel.
MAILTO=""
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
M-59/5 * * * * root /usr/local/bin/cg-site-cron <user> wp      (WordPress, WooCommerce)
<schedule> root /usr/local/bin/cg-site-cron <user> <id>         (each enabled job, by id)
```

No job text is in the file, so cron's `%` rule and its line length no
longer touch it (the API still applies the validation of §2).
`/usr/local/bin/cg-site-cron` is a link to `panel-agent`, made by Apply.
A per-user crontab from before (`/var/spool/cron/crontabs/<user>`) is
removed on every Apply; the API re-applies every site's schedule after an
upgrade, which migrates them. No `crontab(1)` runs at all, and nothing
here depends on `cron.deny`.

- **WordPress's own events** run because provisioning sets
  `DISABLE_WP_CRON`; the minute is staggered by site id (`M = id mod 5`).
- **Disabled jobs** are not scheduled; nothing left to schedule removes
  both files.

**The runner** (`ClientMain`): cron starts `panel-agent` as `cg-site-cron
<user> <id|wp>`, as root. It reads the spec (no link followed), checks the
user, that the root is `/srv/sites/<domain>`, and the job, and becomes
(`exec`; for a job, it runs it as a child instead and, when it ends, records
`{at, status: succeeded|failed, exit_code}` in
`/var/lib/cloudground-agent/cron-runs/<user>-<id>.json`, root's 0600 in a
0700 directory, then exits with its status: `CronRuns` reads those files
back, the API shows them as the job's last run, and site deletion removes
them; a scheduled run used to leave no record the panel could see)
`systemd-run --wait --collect --service-type=exec
--unit=cg-cron-<user>-<id|wp>-<unix time>.service --slice=<user>.slice
--uid/--gid=<user> --working-directory=<root>`, the sandbox lines, 
`RuntimeMaxSec=21600` (6 h: past it systemd stops the run with everything
it started), environment `PATH`, `HOME=<root>`, `SHELL=/bin/sh`,
`TMPDIR=/tmp`, and:

- a job: `/bin/sh -c <wrapper> cg-cron <command> logs/cron-<id>.log`, the
  wrapper as before (`cronjob.Wrapper`: the command in a shell of its own,
  both streams appended to the log between start and end markers, the last
  500 lines kept, the job's status);
- WordPress: `/bin/sh -c 'cd "$1" && exec wp cron event run --due-now
  --quiet >/dev/null 2>&1' cg-cron <root>/current`.

## 4. Run now

`RunCron` runs the job's command once, now, the same way: `systemd-run
--wait --pipe` (not `--quiet`) for `cg-cron-<user>-<job id>.service`, the
sandbox, the slice, `RuntimeMaxSec` = `MaxSeconds`, default and ceiling
**300 s**, the job's environment, and `/bin/sh -c 'exec 2>&1; /bin/sh -c
"$1"' cg-cron <command>`: both streams, interleaved, on stdout, whose last
**64 KiB** are returned. Only that tail is kept as the output arrives
(`host.NewTailBuffer`): a job that prints without end costs the agent
64 KiB, not its memory. systemd-run's own `Finished with result: <r>`
line, on its stderr, tells the end (the line may start with blanks:
systemd 259 right-aligns its summary's labels):

- `succeeded` (exit 0); `timeout` (the limit ran out); `failed` with the
  exit status for `exit-code`, with -1 for anything else (a signal);
- no result line and a failure: the run never started, an error.

When the main process exits, or the limit runs out, systemd stops the
unit, and that kills **every process in its cgroup**. A run of a job that
is still running (its unit active) is `conflict`.

## 5. Site deletion

`RemoveSite` stops every `cg-cron-<user>-*` unit still running (scheduled
and run now) and removes the cron.d file, the spec and a per-user crontab
from before.

## 6. Why cron.d plus a runner, not one systemd timer per job

Timers would need unit files and a daemon reload per change, and cron's
schedule semantics (day-of-month **or** day-of-week) map onto
`OnCalendar` only by two calendars per job. cron.d keeps cron's semantics
exactly; the runner gives every run what a timer would (sandbox, slice,
cgroup kill, a time limit).

## 7. Not here

The `DISABLE_WP_CRON` setting is written by provisioning. Job storage,
descriptions, the last run's status and the audit trail are the API's.
