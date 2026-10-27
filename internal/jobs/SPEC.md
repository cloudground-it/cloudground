# internal/jobs — behaviour spec (R-4.2)

The panel's persistent job queue and its scheduler. Every background
operation the API starts (provisioning, deploys, backups, renewals, the
periodic maintenance) is a **job**: a row in the state database, run by a
worker of this queue under the locks it declares, with progress, a bounded
log, cancellation, a drain on shutdown and a deterministic outcome after a
restart. It replaces the API's goroutine-per-task `runTask` and the single
five-minute scheduler loop.

The package knows nothing about sites, backups or HTTP. The API registers
the kinds (§8), enqueues jobs and maps them to its `/api/tasks` view (§7).

## 1. Job

| Field | Column | Meaning |
| --- | --- | --- |
| `ID` | `id` | increasing; the task ID the API answers with |
| `Kind` | `kind` | a registered kind (§2), e.g. `backup.run` |
| `SiteID` | `site_id` | the site it is about, 0 for server-wide |
| `Status` | `status` | `queued`, `running`, `succeeded`, `failed`, `canceled` |
| `Progress` | `progress` | 0–100, set by the job; 100 on success, kept as it was on failure |
| `Message` | `message` | the latest progress line (at most 2 KiB) |
| `Log` | `log_head`, `log_tail`, `log_dropped` | every progress line, bounded (§5) |
| `Error` | `error` | why it failed or was canceled; empty otherwise |
| `Payload` | `payload` | JSON the kind's handler runs from (§2); never a secret |
| `Locks` | `locks` | the lock keys it needs (§3), JSON array |
| `DedupKey` | `dedup_key` | optional; at most one queued or running job per key (§4) |
| `Quiet` | `quiet` | a maintenance job: not in the task list and no `task.*` events (§7) |
| `Background` | `background` | the scheduler's work: a person's jobs go first (§3) |
| `Attempts` | `attempts` | times it has started (a job resumed after a restart starts again; at most `MaxAttempts` = 3, §6) |
| `CancelRequested` | `cancel_requested` | a cancel arrived while it was running; never lost (§6) |
| `NotBefore` | `not_before` | a queued job held back until then (a rerun after a restart, §6) |
| `CreatedAt`, `StartedAt`, `FinishedAt` | `created_at`, `started_at`, `finished_at` | RFC 3339 UTC; `started_at` is the latest start |

Transitions: `queued → running → succeeded | failed | canceled`,
`queued → canceled` (a cancel before it started), `queued → failed`
(recovery, §6), and `running → queued` only through a resume (drain or
recovery, §6). A terminal job never changes again.

## 2. Kinds

A kind is registered once, at start-up, before any job of it is enqueued;
enqueuing an unknown kind is an error, and so is registering one twice.

| Field | Meaning |
| --- | --- |
| `Name` | the job's `kind` |
| `Quiet` | jobs of this kind are maintenance (§7) |
| `Run` | the handler that runs a job **from its payload**. A kind with `Run` is *resumable*: a queued job of it survives a restart |
| `Rerun` | a job of this kind that was interrupted while running may run again from the start (the handler is idempotent). Only meaningful with `Run` |
| `Canceled` | called after a job of this kind is canceled before it started, or found queued with its work gone at recovery (to undo what the request stored ahead of it) |
| `Interrupted` | called after a job of this kind that had started is failed by a drain or by recovery, or ends canceled at recovery (§6), to leave what it was building in a state a person can act on |
| `NoCancel` | a message: every cancel of this kind is refused with it (§6) |
| `ResolveLocks` | gives a queued job's locks as they are now; the dispatcher stores them before deciding (§3.6) |
| `RerunDelay` | how long recovery holds back a rerun of this kind (§6) |

A kind without `Run` takes its work as a Go function at enqueue
(`Spec.Func`). That function lives only in this process: such a job is not
resumable. This is how requests that carry data that must not be stored
(a WordPress admin password, a database password just generated) are run.
`Spec.Canceled` is the per-job form of `Canceled` for these.

The function or handler receives a context and a `Run`: `Progress(pct,
msg)` records progress and a log line (a negative `pct` only logs),
`Payload(&v)` decodes the payload, `Job()` is the job as it started.
Returning `ErrRequeue` puts the job back in the queue (§3.6).

## 3. Locks and concurrency

1. A job declares lock keys at enqueue. A job starts only when **none of
   its keys is held** by a running job; it then holds all of them until it
   ends. Keys are taken all at once, never one by one, so two jobs can never
   each hold what the other waits for.
2. **Queued jobs wait; they never fail** for want of a lock or a slot.
3. **Order and reservations.** The dispatcher considers a person's jobs
   first and then the scheduler's (`Background`), oldest first within
   each. A job that cannot start reserves keys for the jobs after it:
   - the keys it waits for (held by a running job), against every later
     job, so a stream of short jobs cannot starve it;
   - its other keys (free now), only against the scheduler's jobs: a
     person's job waiting for the repository does not hold the site's
     lock against the person's own edits, but no scheduled job takes it;
   - a scheduler's job reserves all its keys, against later scheduler's
     jobs only.
   So a person's restore waits behind the scheduled backup that is
   running, not behind the ones queued, and blocks neither edits of its
   site nor anything else while it waits; a job holds a key it does not
   run under only while it waits for it.
4. **Limits.** At most `Limit` non-quiet jobs run at once (default 4) and,
   apart from them, at most `QuietLimit` maintenance jobs (default 2): the
   maintenance never waits behind four long backups.
5. The keys the API uses: `site:<id>` for anything that changes a site (at
   most one such job per site), `repo:<hash>` for anything that runs restic
   against a repository (backup, restore, verify, retention: one restic
   process per repository, which takes the repository's lock), `panel` for
   the panel's own certificate, `maintenance:render` for the re-render after
   an upgrade and the drift scan (the scan must not hash files the reconcile
   is rewriting). `<hash>` is the first 16 hex digits of SHA-256 of the
   repository URL, which can carry credentials.
6. **Locks that change.** A kind whose lock follows a setting (the backups
   against "the configured repository") resolves it at each dispatch
   (`ResolveLocks`), and the job checks at its start, after reading the
   setting, that the lock it holds is the one it needs; if the setting
   changed in between it returns `ErrRequeue` and is queued again under
   the new key. It never runs restic against a repository it does not
   hold the lock of.

## 4. Enqueue and deduplication

- `Enqueue(Spec)` stores the job as `queued`, publishes `task.created`
  (§7) and wakes the dispatcher. It answers with the job.
- With a `DedupKey`, if a job with the same key is queued or running, no
  job is created: the existing one is returned (`existing = true`). A unique
  partial index on `dedup_key` over queued and running rows enforces it.
  Used for schedules (one sweep at a time) and for per-subject work the
  scheduler or several requests may ask for twice (a site's scheduled
  backup, a snapshot's verification).
- While the queue drains (§6), `Enqueue` refuses with `ErrDraining`; the
  API answers 503.

## 5. Progress and the bounded log

- `Progress(pct, msg)` sets `progress` (clamped to 0–100) and `message`,
  and appends `msg` and a newline to the log. A line longer than 2 KiB is
  cut there, with `…`.
- The log keeps its **head** (the first 16 KiB, whole lines) and its
  **tail** (the last 48 KiB, whole lines). Once the head is full, new lines
  go to the tail; when the tail would pass 48 KiB its oldest lines are
  dropped and counted in `log_dropped` (bytes). The log as read is
  `head + "… <n> bytes of log omitted …\n" + tail`, the marker present only
  when something was dropped. So a job's log is at most about 64 KiB
  whatever it does, and both how it started and how it ended are kept.
- The marker is written in the log only, never in `message`.

## 6. Cancellation, drain and recovery

**Cancel** (`Cancel(id)`):

- A kind with `NoCancel` refuses, queued or running, with its message (the
  API answers 409). Restore tests are such a kind: a backup counts only
  once a restore of it has been tested, so a test is never skipped.
- A queued job is canceled at once: `canceled`, error `Canceled`,
  `finished_at` set, and its kind's (or spec's) `Canceled` hook run
  **before any other job can start** (the hook runs under the queue's
  lock, so nothing queued behind renders what it is undoing; a hook must
  not call the queue), then `task.finished` published. It never starts.
- A running job gets `cancel_requested` and its context is canceled. Agent
  calls that change something are never abandoned mid-call (agentrpc SPEC:
  a mutating call is detached from the caller's cancellation). A job checks
  its context **before every step that changes something** (the API's
  `checkpoint`): there it stops, logs "Stopped before <step>; the steps
  above were done", and ends `canceled`. If it completes anyway it ends
  `succeeded`. What a job does once a step is sent (a recorded snapshot
  still gets its restore test; a promoted release is still verified) is
  the job's to finish.
- A terminal job cannot be canceled: `ErrFinished` (409).

**Drain** (`Drain(ctx)`, on SIGTERM, after the HTTP servers stopped):

1. From the first instant, nothing new starts and `Enqueue` refuses.
2. Running jobs continue. Drain waits until none is running or `ctx`'s
   deadline (the panel's drain timeout, 30 s by default).
3. At the deadline the context of every job still running is canceled
   with the reason *shutdown*, and Drain waits 5 s more. A job that returns
   meanwhile ends `canceled` if a person had canceled it; otherwise it is
   **resumed** if its kind is `Run` + `Rerun` and it has started fewer than
   `MaxAttempts` times (back to `queued`, a log line `Interrupted by a panel
   shutdown; it runs again after the restart`), and **failed** with
   `interrupted by panel shutdown` and its `Interrupted` hook otherwise.
4. Jobs still running after that stay `running` in the database: the
   process exits, and recovery settles them.

A job's work tells a drain's cancel from a person's with
`ShuttingDown(ctx)`: after a drain the queue reruns or settles the job
(above), so the work must not record a cancellation of its own (the
deployments do not become `canceled`).

**Recovery** (`Start`, before the first dispatch). The panel runs one API
process, so at start-up no job can be running anywhere: every row still
`running` belongs to a process that is gone.

| Row at start-up | Kind | Becomes |
| --- | --- | --- |
| `running`, `cancel_requested` | any | `canceled` (the cancel is never lost, the job never rerun); `Interrupted` hook |
| `running` | `Run` + `Rerun`, started fewer than 3 times | `queued`, log line `Interrupted by a panel restart; it runs again`, not before now + the kind's `RerunDelay` |
| `running` | `Run` + `Rerun`, started 3 times | `failed`, `interrupted by panel restarts 3 times; not run again`; `Interrupted` hook |
| `running` | anything else | `failed`, `interrupted by panel restart`; `Interrupted` hook |
| `queued` | `Run` | stays `queued` and runs |
| `queued` | no `Run` (its function died with the process) | `failed`, `not started: the panel restarted before it could run`; `Canceled` hook (it never ran), else `Interrupted` |
| `queued`/`running` | unknown kind | `failed`, `unknown job kind after a panel restart` |

**A rerun and the dead process's agent call.** A process that exits hard
may leave an agent call running on the agent. A rerun of a site-changing
kind needs no delay: the agent takes its own lock of the site for those
calls, so the rerun's call waits there behind the orphan. A rerun of a
repository kind (backup, restore test, retention) is held back 15 minutes
(`RerunDelay`); restic's own repository locks make an overlap that still
happens harmless (backups and tests take shared locks; a prune takes the
exclusive one and fails cleanly, to be retried the next day).

No job is ever left `running` by a restart. Recovery publishes nothing
(no browser is connected yet). After it, the API's `Recovered` callback
runs (§8: unverified snapshots get their verification queued).

**Waiting.** `Wait(ctx, id)` returns the job once it is terminal (or the
context ends). The maintenance run-now uses it.

## 7. Task view and events (what the UI sees)

The UI and the e2e know *tasks* (`GET /api/tasks`, `GET /api/tasks/{id}`,
`task.*` events). A non-quiet job is shown as a task with the same JSON
fields as before (`id`, `kind`, `site_id` omitted when 0, `status`,
`progress`, `message`, `log`, `error` omitted when empty, `created_at`,
`started_at`, `finished_at`), plus `job_status` (the real status) and
`cancel_requested` (when set). `status` keeps the task vocabulary the UI
renders:

| `job_status` | `status` |
| --- | --- |
| `queued` | `pending` |
| `running` | `running` |
| `succeeded` | `succeeded` |
| `failed` | `failed` |
| `canceled` | `failed` (the error says `Canceled`) |

Events, published only for non-quiet jobs, with the job's site:

| Event | When | `data` |
| --- | --- | --- |
| `task.created` | enqueued | the task view without `log` |
| `task.progress` | started (progress as it stands, empty message) and each `Progress` | `{id, kind, status, progress, message}` |
| `task.finished` | after the store has the terminal row | `{id, kind, status, job_status, progress, error}`; the UI shows a canceled task as a neutral toast, not a failure |

A job resumed by a drain publishes nothing (it is `queued` again and the
browser's stream is about to end); a job canceled while queued publishes
`task.finished`. `GET /api/tasks` lists the newest 100 non-quiet jobs.
Quiet jobs are read by their ID only, by an admin.

**Retention.** `jobs.prune` deletes quiet jobs finished more than 24 hours
ago and every job finished more than 30 days ago.

## 8. The API's kinds

Locks as in §3. "Resumable" = has `Run`; "rerun" = `Rerun`.

| Kind | Site | Locks | Resumable | Quiet | Notes |
| --- | --- | --- | --- | --- | --- |
| `site.create` | new site | `site` | no | | canceled before it ran: the record goes (nothing was provisioned); interrupted: a site still `provisioning` goes to `error` (a delete cleans it up, R-3.13) |
| `site.delete` | site | `site` (+ its staging's) | no | | dedup `site.delete:<site>` (a second delete joins the first); canceled before it ran: the site gets its previous status back; canceled between the staging copy and production: production gets its status back; interrupted: a site still `deleting` goes to `error` |
| `site.update` | site | `site` | rerun | | `PATCH /api/sites/{id}` (internal/api/SPEC.md §3.5): payload carries the request's fields, their previous values and the generations around the change; renders the site, and switches the object cache when that changed (alone, it is not rendered); a failure or a cancel before it starts puts the fields back where no later change replaced them, and a cancel that leaves the site as it was rendered marks it applied |
| `site.apply-config`, `site.php`, `wp.object_cache` | site | `site` | rerun | | as `site.update`; queued only by the routes removed in R-8.2 (`PUT /config`, `PUT /php`, `POST /wp/object-cache`), still registered so a job queued before an upgrade runs; `wp.object_cache` only switches the object cache |
| `certificate.issue` | site | `site` | no | | |
| `functions.sweep` | — | — | rerun | quiet | invokes the functions whose schedule is due this minute (internal/api/SPEC.md §3.22) |
| `runtime.install` | — | `apt` | no | | payload `{runtime, version}`; `RuntimeInstall` (internal/api/SPEC.md §3.21) |
| `staging.create` | production | `site` ×2 (production, new staging) | no | | canceled before it ran: the staging record goes; interrupted: the staging copy, if still `provisioning`, goes to `error` |
| `staging.recreate` | production | `site` ×2 (production, old staging) | no | | stops between removing the old copy and cloning again; interrupted: a new copy left `provisioning` goes to `error` |
| `staging.database-push` | production | `site` ×2, `repo` | no | | a cancel stops it before the tables are promoted |
| `deploy.create` | site | `site` | yes | | payload `{deployment_id, source_dir}`; the deployment state machine (internal/api/SPEC.md §4): canceled before it ran, the deployment is `canceled`; interrupted, `failed` |
| `deploy.promote` | site | `site` | rerun | | payload `{deployment_id, from}`; a promote is idempotent; interrupted for good, the release goes back to `from` |
| `deploy.rollback` | site | `site` | yes | | payload `{site_id, target}` (the release chosen at the request); never rerun; interrupted: the site is flagged for a reconcile |
| `deploy.reconcile` | site | `site` | rerun | | background; dedup `deploy.reconcile:<site>`; queued after recovery for flagged sites and sites with a deployment left promoting or verifying (internal/api/SPEC.md §4.6) |
| `deploy.safe-push` | production | `site` ×2 | rerun | | payload `{deployment_id, staging_id, only, paths, force}`; a rerun resumes from the deployment's status (measures, promotes or verifies again); a cancel during the measurement stops it before production is touched; after the build it runs to its end (a cut verification would roll back) |
| `deploy.git` | site | `site` | no | | payload `{deployment_id, branch}`; fetch and build as the site user (`GitSync`), then build the release and promote it as `deploy.create` and `deploy.promote` do (internal/api/SPEC.md §15.4); at most one waiting per site; canceled before it ran: the deployment is `canceled`; interrupted: `failed`, or the site is flagged while it was promoting |
| `wp.cleanup` | site | `site`, and the repository's | no | | background when the schedule queues it; dedup `wp.cleanup:<site>`; a dry run, a tested snapshot before the site's first cleanup, the cleanup (internal/api/SPEC.md §3.17) |
| `wpcleanup.sweep` | — | — | rerun | quiet | enqueues the due `wp.cleanup` jobs |
| `cloudflare.ranges` | — | — | rerun | quiet | fetches Cloudflare's ranges while a site takes visitor addresses from Cloudflare, stores and sends them to the agent (internal/api/SPEC.md §3.20) |
| `traffic.collect` | — | — | rerun | quiet | the agent reads what every site's access log gained into its traffic aggregates (internal/api/SPEC.md §11.8) |
| `wp.update` | site | `site`, `repo` | no | | payload `{site_id, domain, what}`; a tested snapshot, the update, a health check and a rollback when it fails (internal/api/SPEC.md §6.2.3); a cancel stops it before the update |
| `wp.update.scheduled` | site | `site`, `repo` (the repository as configured when it starts) | yes | | background; payload `{site_id, domain, what}`; dedup `wp.update.scheduled:<site>`; as `wp.update`; never rerun (the next slot is); canceled before it ran: the site's schedule key is cleared |
| `database.import`, `dbstudio.import`, `migration.import` | site | `site` | no | | |
| `database.export`, `dbstudio.export`, `cache.warm` | site | — | no | | read the site; nothing to serialise |
| `panel.certificate` | — | `panel` | no | | |
| `apt.update`, `apt.upgrade` | — | `apt` | no | | dedup by kind; `apt-get update` and the list, or the upgrade of the server's packages (internal/api/SPEC.md §7.5); a cancel stops an upgrade only before it is sent |
| `backup.run` | site | `repo` | rerun | | snapshot, then its restore test in the same job |
| `backup.scheduled` | site | `repo` | rerun | | background; dedup `backup.scheduled:<site>`; canceled before it ran: the site's schedule key is cleared, so the next sweep queues it again |
| `backup.verify` | site | `repo` | rerun | | payload `{backup_id}`; dedup `backup.verify:<id>`; **cannot be canceled** |
| `backup.restore` | site | `site`, `repo` | no | | safety snapshot first, restore-tested; a cancel stops it before the restore |
| `backup.retention` | site | `repo` | rerun | | payload `{site_id}`; dedup `backup.retention:<site>`; never forgets a snapshot whose restore test has not run |
| `backup.prune` | — | `repo` | rerun | quiet | daily; prunes the configured repository when a deletion left forgotten data |
| `backup.forget` | site | `repo` (of the record's repository) | rerun | | payload `{backup_id}`; dedup `backup.forget:<id>`; deletes one snapshot (R-4.3); refuses, again under the lock, the site's newest verified snapshot and one whose restore test has not run |
| `maintenance.reconcile` | — | `maintenance:render` | rerun | quiet | the re-render after an upgrade or a change in the number of sites |
| `metrics.sample` | — | — | rerun | quiet | |
| `backup.sweep` | — | — | rerun | quiet | enqueues the due `backup.scheduled` jobs |
| `retention.sweep` | — | — | rerun | quiet | enqueues the due `backup.retention` jobs |
| `security.sweep` | — | — | rerun | quiet | enqueues the due `wp.vulncheck` and `site.malwarescan` jobs (internal/api/SPEC.md §11.6, §11.7) |
| `devmode.sweep` | — | — | rerun | quiet | ends the development mode of the sites whose time has passed (internal/api/SPEC.md §3.15) |
| `wp.update.sweep` | — | — | rerun | quiet | enqueues the due `wp.update.scheduled` jobs (internal/api/SPEC.md §6.2.3) |
| `site.malwarescan` | site | `security:malwarescan` | rerun | | background when the sweep queues it; payload `{site_id}`; dedup `site.malwarescan:<site>`; one at a time on the server; updates the signatures (at most daily), scans the site's tree in its sandbox, changes nothing on it |
| `wp.vulncheck` | site | `security:vulncheck` | rerun | | background when the sweep queues it; payload `{site_id}`; dedup `wp.vulncheck:<site>`; one at a time on the server; reads the site, changes nothing on it |
| `certificate.sweep` | — | `panel` | rerun | quiet | reads every certificate's expiry, enqueues `certificate.renew` for the due ones, renews the panel's |
| `certificate.renew` | site | `site` | rerun | quiet | |
| `drift.scan` | — | `maintenance:render` | rerun | quiet | |
| `alerts.notify` | — | — | rerun | quiet | evaluates the alerts, then sends the webhook (internal/api/SPEC.md §11) |
| `cache.rewarm` | site | `cache:rewarm` | rerun | quiet | background; payload `{site_id}`; dedup `cache.rewarm:<site>`; the re-warm after a promote, a rollback or a migration (internal/api/SPEC.md §6.2) |
| `jobs.prune` | — | — | rerun | quiet | §7 |

Every job a request starts reads its site again at its start (the work
receives the site as stored then, not the request's copy); one deleted
meanwhile fails the job without running it, and only `site.delete` runs on
a site being deleted. Every kind's backup, retention, renewal and verify
the scheduler queues is background (§3.3).

**Verification is always queued.** A snapshot the panel records is
restore-tested in the job that took it. If that job is canceled or the
panel stops between the two, the snapshot's `verify_status` is still
`pending`: at every start-up, after recovery, each pending snapshot gets a
`backup.verify` job (deduplicated). It is never skipped and never left
pending.

## 9. Scheduler

Recurring work is a schedule that enqueues a job:

| Schedule | Every | Last run kept | In the run-now tick |
| --- | --- | --- | --- |
| `maintenance.reconcile` | 5 min | memory | yes |
| `metrics.sample` | 5 min | memory | yes |
| `backup.sweep` | 5 min | memory (each site's own `backup.site.<id>` decides when it is due: hourly, daily, weekly) | yes |
| `retention.sweep` | 5 min | memory (each site's `backup.retention.site.<id>`: daily) | yes |
| `security.sweep` | 5 min | memory (each site's `vulncheck:site:<id>`: daily; `malwarescan:site:<id>`: weekly, at the site's own hour) | yes |
| `devmode.sweep` | 1 min | memory | yes |
| `functions.sweep` | 1 min | memory (each function's minute) | no |
| `wpcleanup.sweep` | 5 min | memory (each site's `wpcleanup:site:<id>`: weekly, at the site's own slot) | yes |
| `wp.update.sweep` | 5 min | memory (each site's `wp.update.site.<id>`: daily or weekly, at the site's hour) | yes |
| `traffic.collect` | 5 min | memory | yes |
| `drift.scan` | 5 min | memory | yes |
| `alerts.notify` | 5 min | memory | yes, after the others |
| `certificate.sweep` | 12 h | `schedule_state` key `cert.renew` | only when due |
| `jobs.prune` | 6 h | memory | no |
| `backup.prune` | 24 h | memory | no |
| `cloudflare.ranges` | 24 h | memory | no |

- The scheduler wakes 30 s after start (the agent may still be coming up)
  and then every 30 s, and enqueues each schedule that is due, with the
  dedup key `schedule:<name>`, so a sweep still running or queued is never
  doubled. A schedule kept in memory is due at the first wake after a start,
  as the old loop ran its first tick 30 s after the start.
- **Run one tick now** (`POST /api/system/maintenance`, admin) enqueues
  every schedule of the tick (deduplicated: one already queued or running
  is waited for instead), waits for them, then enqueues `alerts.notify` and
  waits for it (which evaluates the alerts and sends the webhook), and answers
  `{"ran": true, "duration_ms": …}`. If that takes more than 10 s it
  answers `202 {"ran": false, "jobs": [ids]}` instead and the tick goes on;
  the caller polls `GET /api/tasks/{id}` (an admin reads maintenance jobs). The scheduled backups the sweep
  started are not waited for, as before. The jobs run as the scheduler's
  own, so a tick already in progress finishes first.
- A schedule's job that fails is retried at its next due time; the sweeps
  record the per-site keys exactly as the old loop did (a failed scheduled
  backup clears its key, so it is due again at the next sweep; a retention
  run is marked before it runs and not cleared).

## 10. Not done here

- Killing restic on cancel (SIGINT then SIGKILL) is the agent's side
  (plan, Fase 4 item 6); until then a running backup, restore or verify runs
  to its end when canceled.

## 11. Jobs that hold off an update or an uninstall (R-6.5)

`BlocksPanelChange(kind)` names the kinds whose **running** job must finish
before the panel's services are restarted (`POST /api/panel/update`, `cgctl
update`) or removed (`cgctl uninstall`): every `backup.` kind (a backup, a
verification, retention, a prune or a restore holds restic's lock on the
repository and leaves a partial snapshot or a half-restored site when cut),
and the kinds that are not resumable and would fail half done: `site.create`,
`site.delete`, `migration.import`, `staging.database-push`,
`database.import`, `database.export`, `database.create`, `database.drop`,
`dbstudio.import`, `dbstudio.export`, `wp.update`, `wp.update.scheduled`, `apt.upgrade`. A queued job does not
block: it waits for the panel to come back. The callers name the job's id
and kind, so the operator knows what to wait for.
