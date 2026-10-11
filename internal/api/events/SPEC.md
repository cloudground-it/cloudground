# internal/api/events — behaviour spec (F8-events)

The panel's in-process event bus. Whatever changes something a browser
shows publishes one typed event; `GET /api/events` streams them to signed-in
browsers as Server-Sent Events, filtered per user. The UI refreshes a view
when an event says it changed, instead of polling every view on a timer.

The bus knows nothing about users, sites or HTTP. It assigns IDs, keeps a
short history and fans out; the API decides who may see what (§5). The
persistent job queue (internal/jobs) publishes the `task.*` events here.

## 1. Event

| Field | Wire | Meaning |
| --- | --- | --- |
| `ID` | SSE `id:` line, `"<epoch>.<seq>"` | `seq` counts from 1 per process; `epoch` is random per bus, so an ID from before a restart is recognisably foreign |
| `Type` | `type` | one of the catalogue (§2) |
| `SiteID` | `site_id`, omitted when 0 | the site the event is about; **0 means server-wide** |
| `At` | `at`, RFC 3339 UTC | when it was published |
| `Data` | `data`, omitted when null | small JSON payload (§2); never a secret, never more than the event's audience can already `GET` |
| `Internal` | not sent | control events for the API itself (§2, last rows); never delivered to a browser |

An event's JSON is marshalled once, at publish, and the same bytes go to
every subscriber. `Publish` on a nil bus is a no-op, so code paths without a
bus (state tests, tools) need no guard.

## 2. Catalogue

Site-scoped events carry the site's ID; everything else has `site_id` 0.

| Type | Scope | Published by | `data` |
| --- | --- | --- | --- |
| `task.created` | site or server | the job queue, at enqueue (internal/jobs/SPEC.md §7; maintenance jobs publish nothing) | the task without its log |
| `task.progress` | site or server | the job queue: start and every progress step | `{id, kind, status, progress, message}` |
| `task.finished` | site or server | the job queue, after the store records the end (a cancel before the start included) | `{id, kind, status, progress, error}` |
| `site.changed` | site | `Store.CreateSite`, `MutateSite(Applied)`, `MarkApplied` | `{reason, status, staging_of, desired_generation, applied_generation}`; `reason` is `created`, `updated` or `applied` |
| `site.deleted` | site | `Store.DeleteSite` | `{staging_of}` |
| `deployment.changed` | site | `Store.CreateDeployment`, `SetDeploymentStatus` | `{id, status}` |
| `backup.finished` | site | `Store.CreateBackup` (a snapshot was recorded) | `{id, kind}` |
| `backup.verified` | site | `Store.SetBackupVerified` | `{id, status}` |
| `backup.removed` | site | `Store.DeleteBackupsBySnapshot` (retention) | `{count}` |
| `cron.changed` | site | `Store` cron writes (create, update, delete, run recorded) | `{id}` |
| `databases.changed` | site | `Store` writes of a site's databases and database accounts (create, delete, grants, password) | `{name}` |
| `cert.changed` | site | `Store.SetCertExpiry`, `SetCertError` (renewal job, issue, upload) | `{}` |
| `alert.raised` | site or server | `Store` alert writes, when an occurrence is raised (internal/api/SPEC.md §11) | `{id, level, kind, site, message}` |
| `alert.cleared` | site or server | `Store` alert writes, when one is cleared, replaced or dismissed | `{id}` |
| `drift.changed` | server | `Store.CreateDriftEvent`, `ResolveDriftEvent` | `{id}` |
| `service.changed` | server | the live monitor (§4), and a service restart | `{names}` |
| `system.status` | server | the live monitor | the agent's `SystemStatus` result (what `GET /api/system/status` answers) |
| `metrics.sample` | server | `Store.RecordMetric` (the `metrics.sample` job) | the sample |
| `settings.changed` | server | `Store.UpdatePanelSettings` | none |
| `user.changed` | server | `Store` user writes (create, delete, role, profile, site assignment, password) | `{user_id}` |
| `resync` | per stream | the SSE handler (§3) | none: refetch everything shown |
| `ping` | per stream | the SSE handler, every 25 s | none; carries no ID |
| `session.revoked` | internal | `Store` session deletions | `{user_id}` (0: unknown) |

## 3. Subscribers and history

1. `Subscribe(lastID, buffer)` registers a subscriber with a channel of
   `buffer` events and, atomically with the registration, returns the events
   after `lastID` from the history ring (default 1024 events). Nothing
   published in between is lost or duplicated.
2. `lastID` empty: no replay. `lastID` from this bus and still in the ring
   (or the newest event): replay, `resumed = true`. Anything else (another
   epoch, older than the ring, newer than the newest, malformed):
   `resumed = false`, and the caller sends `resync`.
3. **Publishers never block.** A subscriber whose buffer is full when an
   event arrives is dropped there and then: removed from the bus, its
   channel closed, `Dropped()` true. The SSE handler answers a drop with a
   `resync` event and subscribes again; the browser refetches.
4. `Close()` unregisters and closes the channel; it is idempotent and safe
   after a drop.

## 4. Watchers in the API

- **Alert watcher.** Alerts are rows of the store (internal/api/SPEC.md
  §11). The watcher evaluates the conditions once when it starts and again,
  at most once a second, after any `task.finished`, `site.*`,
  `cert.changed`, `drift.changed` and `service.changed`; the `alerts.notify`
  job evaluates every five minutes for the time-based conditions (a
  certificate entering its warning window). The store publishes
  `alert.raised` and `alert.cleared` for what an evaluation or a dismissal
  changed, with the alert's site as the event's site.
- **Live monitor.** While at least one stream with a server-wide view is
  open, every 15 s it reads `SystemStatus` and `ServiceStatus` from the
  agent once (not once per browser), publishes `system.status`, and
  publishes `service.changed` when a service's state, activity or main PID
  differs from the previous read. Nobody connected, no agent calls.
- **Drift.** The `drift.scan` job (every five minutes) scans for drift (it used to run only when
  the dashboard fetched `/api/system/drift`, every 30 s per open tab). The
  scan publishes only when it creates or resolves a drift event, so a
  `drift.changed` → refetch → scan round ends after one pass.

## 5. Delivery rules (`GET /api/events`)

- Session cookie, through the same guard as every route (2FA requirement
  included). Same origin only: a request whose `Sec-Fetch-Site` is present
  and not `same-origin`/`none`, or whose `Origin` names another host, is
  refused with 403.
- `admin`, `readonly`: every non-internal event. Both may `GET` everything
  server-wide (`global` routes), and all sites.
- `operator`: only events with a `site_id` it may access: its assigned
  sites and their staging copies. Never a server-wide event (task without a
  site, `settings.changed`, `service.changed`, `system.status`,
  `metrics.sample`, `drift.changed`, `user.changed`, a server alert).
- The viewer (user, role, accessible sites) is re-read from the session:
  at most 5 s old when an event is delivered; at once after a
  `session.revoked`, a `user.changed` about the viewer, or a `site.changed`
  of reason `created` (a new staging copy of an assigned site); and at
  every heartbeat. A session that has ended, a disabled or deleted user,
  or a newly unmet 2FA requirement ends the stream.
- `site.deleted` is checked against the viewer's site set *before* the
  deletion is applied to it, so an operator learns that its site is gone.
- `Last-Event-ID` (header, or `last_event_id` query parameter for a client
  that reconnects with a new `EventSource`) resumes from the ring (§3.2);
  otherwise `resync`.
- Heartbeat: a `ping` message every 25 s. The handler clears any read
  deadline (the panel's `http.Server` has no `ReadTimeout`, api SPEC §1.6)
  and extends the write deadline before each write, so the stream outlives
  the server's `WriteTimeout` (10 min). Behind nginx it relies on `proxy_buffering off`
  and `proxy_read_timeout 1h` (both panel vhosts) and also sends
  `X-Accel-Buffering: no`.
