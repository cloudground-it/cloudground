# ops/panel — the panel's own certificate and self-update

Sources: the API callers (`internal/api/ops.go` handlePanelCertificate and
handleSelfUpdate, `internal/api/scheduler.go` panel renewal), the installer
(bootstrap vhost, `/etc/cloudground/certs`, unit files, the release check),
`docs/install-update-uninstall.md`, `scripts/release-sign`, `internal/version`
(signed manifest, Phase 0 downgrade refusal).

Adapters only: `host.ACME` (certbot), `host.Nginx` (test, reload that waits
for a new worker generation, `nginx -V`), `host.Systemd` (D-Bus). No shell
script, no `sleep`, no `systemctl`, `install`, `cp` or `chgrp`.

## 1. PanelCertificate

Params `{domain, email}`; result `{"domain": d, "status": "issued" | "unchanged"}`.
The method table gives it no site lock; it takes `CertMu`, then the nginx lock
(the shared order).

1. `domain` must pass `site.ValidateDomain`; `email` must be an address not
   starting with `-`. Otherwise `invalid`.
2. **The panel vhost** `<NginxSites>/cloudground-panel.conf` is rendered by
   `engine/nginx.PanelVhost` (tested with the renderer, no longer a
   `Sprintf` in the agent):
   - `:80` answers `/.well-known/acme-challenge/` from the ACME webroot and
     redirects everything else to https;
   - `:443 ssl` with the panel certificate and key, `server_tokens off`, HSTS,
     `include <panel-access.conf>*` (operator rules that survive a re-render;
     the wildcard keeps nginx happy when the file does not exist),
     `client_max_body_size 16m`, and everything proxied to the API's Unix
     socket with `Host`, `X-Real-IP`, `X-Forwarded-For` and
     `X-Forwarded-Proto https`;
   - HTTP/2 as `http2 on;` when nginx is 1.25.1 or newer, as the `listen`
     parameter before. The version comes from `Nginx.BuildInfo` (`nginx -V`,
     stderr); an unreadable version means the old form, which every nginx
     accepts.
   Under the nginx lock: atomic replace, `Nginx.Test`; if nginx refuses it,
   the previous file is put back (or the new one removed) and the error
   returned. Then `Nginx.Reload`, which returns once a new worker
   generation runs. A panel vhost nginx refuses would block every later
   reload on the host.
3. The nginx lock is released. The HTTP-01 preflight (as in ops/certs) runs,
   then `ACME.CertOnly{Name: "cloudground-panel", Domains: [domain],
   Webroot}`. Neither holds the nginx lock. The old code held it across both,
   so every site render on the server waited for certbot (minutes).
4. The lineage's `fullchain.pem` and `privkey.pem` must form a pair. If the
   fullchain is byte-identical to the installed `PanelCert`, the result is
   `unchanged` and nothing is written or reloaded. certbot does nothing for a
   certificate that is not yet due, and the scheduler calls this every 12
   hours.
5. Otherwise, under the nginx lock: the previous key and certificate are
   kept in memory, the key (`PanelKey`, 0640) and the certificate (`PanelCert`,
   0640), both `root:<PanelGroup>`, are replaced atomically (fchown/fchmod on
   the temp file before the rename). The API reads them through its group.
   `Nginx.Test`: if it fails, both previous files are restored. Then
   `Nginx.Reload`, so nginx serves the new certificate when this returns.
6. **The API is not restarted.** It serves plain HTTP on the socket that
   systemd passes it, and nginx terminates TLS. The old code restarted it
   through `systemd-run … sh -c "sleep 2; systemctl restart cloudground-api"`
   on every issuance. That dropped every open SSE stream and in-flight
   request, and the `sleep` was the only thing between the restart and the
   RPC's own reply. The API's fallback TLS listener (no socket activation)
   re-reads the certificate files when they change (`cmd/panel-api`).

## 2. SelfUpdate

Params `{version, channel}`; result `{updated_to, restarted}`.

### 2.1 Verify (nothing live is touched)

- The origin (`version.UpdateURL`) and the release keys come from the build
  (internal/version SPEC §3.2, §4). Without an origin or a well-formed key →
  `invalid` "self-update is not available in this build: <why>", which also
  covers an origin `version.CheckOrigin` refuses (anything but https, or
  plain http to this machine's loopback for the upgrade test; R-6.5); a
  `channel` other than `stable`, `beta` or `""` (stable) → `invalid`.
- The release: `version` when given (a missing `v` is added; anything but
  `vX.Y.Z` or `vX.Y.Z-beta.N` → `invalid`), else the newest the feed lists
  for the channel (`version.Feed.Latest`). Its files come from
  `version.ReleaseBase(<tag>)`.
- One update at a time: a second call while one runs, or while the guard unit
  (§2.3) is still active, → `conflict`.
- `SHA256SUMS` (≤ 1 MiB) and `SHA256SUMS.sig` (≤ 4 KiB) are downloaded. The
  client refuses redirects off https and follows at most 5. They go through
  `version.VerifyManifest`, then `version.CheckUpdate(manifest, <tag>,
  running)` (so a manifest signed for another release than the tag it was
  served as is a `conflict`), then `version.CheckChannel` (a beta on the
  stable channel → `conflict`), then the floor: a manifest not newer than the newest release
  ever installed here (`<StateDir>/highest-release`) → `conflict` "refusing
  to downgrade".
- Artifacts: `panel-api`, `panel-agent`, `cgctl` (required: a manifest
  without one → `invalid`), and the units `cloudground-agent.service`,
  `cloudground-api.service`, `cloudground-api.socket` when the manifest lists
  them (the release signs them, and the installer installs them). The old
  self-update ignored the units, so a release that changed one ran its new
  binaries under the old unit.
- Each artifact is streamed (≤ 256 MiB) into a staging file **in its
  destination directory** (`<BinDir>/.<name>.cg-update`,
  `<SystemdDir>/.<name>.cg-update`), opened `O_EXCL|O_NOFOLLOW`, and must
  match its signed sha256. A binary must also be an ELF executable for this
  machine's architecture. Any failure removes every staged file and returns.

### 2.2 Swap (agent side, by rename)

- `<BinDir>/panel-agent` must exist: the guard runs from its backup.
- `cg-site-shell` (R-10.5) is updated when the manifest lists it and the
  live name is a regular file or missing; while it is still the link to
  `panel-agent` an older install made, the self-update leaves it (the
  installer replaces it, and a rollback has nothing to put back).
- For each binary: `<live>.bak` is removed, then **hard-linked** to the live
  file, which is the running version. Then the staged file is renamed over
  the live name. The rename is atomic, so a reader sees the old binary or the
  new one, never a partial file. `install` and `cp -a` are gone.
- For each unit whose bytes differ from the installed file, the installed
  content is saved under `<WorkDir>/update-backup/` and the staged file is
  renamed in. An identical unit is left alone.
- Any failure restores what was swapped and returns.
- The plan is written to `<WorkDir>/update-plan.json` (0600): the version,
  the version it replaces, the paths, the binaries with a backup, the
  changed units, and whether the socket unit changed.

### 2.3 Hand-over to the guard

- The guard is a mode of the agent binary, `panel-agent update-guard
  <plan>`, started as the transient unit `cloudground-update-guard.service`
  over D-Bus (`StartTransientUnit`, `Type=exec`,
  `CollectMode=inactive-or-failed`). Its program is `<BinDir>/panel-agent.bak`:
  the code running now, known to work, not the release being tried. A
  transient unit is outside the agent's cgroup, so it outlives the agent's
  restart. It is not under the agent's sandbox either.
- If the unit cannot be started, the swap is undone and the error returned.
  Nothing was restarted, so nothing else needs undoing.
- The RPC then returns `{updated_to, restarted: true}`. The reply cannot be
  lost to the restart. The agent drains on SIGTERM (`agentrpc.Server.
  Shutdown`, 5 s ceiling), and the API drains its own requests, so a restart
  the guard issues waits for the reply in flight. No `sleep` is involved.

### 2.4 The guard

**Changed (R-6.5):** the guard stops the panel and snapshots its state before
the new release first starts, and a rollback puts the snapshot back.

1. Record both services' main PIDs.
2. **Stop** `cloudground-api.socket`, then `cloudground-api.service` (the
   socket first: with it listening, the next request would start the API
   again, on the new binary), then `cloudground-agent`. The API drains its
   requests in flight, the update's own reply included; the agent then
   finishes the RPCs the API had made (one the API stopped waiting for still
   runs to its end in the agent and may write a record), so nothing writes
   the state or the records while the snapshot is taken. (Before this review
   the agent kept running through the snapshot.) From here until step 5 the panel answers
   502 through nginx; the sites are not touched.
3. **Snapshot** (§2.5). If it cannot be taken, the update is **aborted**:
   the previous files go back (`restoreFiles`), the panel is started as in
   step 5 and waited for, and the result says `rolled_back` with
   "…; the update was not applied". The new release never ran.
4. If units changed: `DaemonReload`. `ResetFailed` both services.
5. `Restart cloudground-agent`, `Start cloudground-api.socket`, `Start
   cloudground-api`. Each D-Bus job is waited for. (Before R-6.5 the API was
   restarted with its socket listening, and stopped only when the socket
   unit changed.)
6. **Healthy** means, all within 60 s, polled every 250 ms:
   - both units `active` with a main PID different from before;
   - each main PID's executable (`/proc/<pid>/exe`) is the **same inode** as
     the live binary, which proves the new release is what runs;
   - `GET /healthz` over the API's Unix socket answers `200`. The API
     answers 200 only when the agent answers its `Ping`, and it does not
     start at all on a state database it cannot migrate.
7. Healthy: `<StateDir>/highest-release` is set to the new version, if it
   is newer than the recorded one (a release that was rolled back never
   raises the downgrade floor); old snapshots are pruned (§2.5). The outcome
   is written to `<StateDir>/update-result.json`, and the guard exits 0.
8. Not healthy: **rollback**. The panel, the new agent included, is
   stopped as in step 2; each `.bak`
   is renamed back over its live binary and each changed unit gets its saved
   content back; the snapshot is put back (§2.5); then steps 4–6 again,
   against the restored binaries. The result says `rolled_back` and why,
   `state_restored` (the snapshot went back) and `restored_healthy` (the old
   release came back healthy), and the guard exits 1. Both runs log to the
   journal of `cloudground-update-guard`.

The result file (`update-result.json`, root 0600): `version`, `previous`,
`ok`, `rolled_back`, `restored_healthy`, `state_restored`, `error`,
`snapshot` (its directory), `started` (RFC 3339 with nanoseconds, when the
guard began: a waiting `cgctl update` tells this run's result from an older
one by it) and `finished`.

### 2.5 The pre-update snapshot (R-6.5)

Migrations of the state database only go forward (internal/state SPEC,
Migrations): the new API migrates it when it first starts, and an older API
refuses a database a newer one migrated. So the way back from a release is a
copy of the state taken before it ran:

- **What**: the API's state database (`<StateDB>`, `/var/lib/cloudground/
  state.db`) and its `-wal`, and the agent's records (`sites.json`, the site
  index; `databases.json`, the database registry). The `-shm` index is never
  copied (SQLite rebuilds it) and is removed on a restore. Nothing under
  `/srv/sites`, no site database, no certificate, no backup: an update does
  not write site data, so a snapshot holds none and a rollback restores none.
  `secret.key` is not copied either: it is created once and never rewritten.
- **When**: with both services stopped (step 2), so no process writes the
  files: the database and its WAL copied together are a consistent database.
- **Where**: `<StateDir>/update-snapshots/<UTC yyyymmddThhmmssZ>-<from>-to-<to>/`,
  root `0700`, one file per path (`0600`) and `snapshot.json`: for each path
  its file name, mode, uid, gid, size and SHA-256, or `absent` when it did not
  exist. Every source is opened `O_NOFOLLOW` and must be a regular file (the
  API's user owns `/var/lib/cloudground` and could plant a link there): a
  link, or any copy error, removes the partial directory and aborts the
  update (step 3).
- **Restore**: each file is copied to a temporary name beside its path,
  checked against its recorded SHA-256 (a mismatch leaves the live file
  alone and fails), given its mode and owner, and renamed over the path; an
  `absent` path is removed. Every file is tried even when one fails.
- **Retention**: the newest **3** are kept (`KeepSnapshots`), pruned after
  each update, healthy or not. Only directories named like a snapshot are
  touched. A snapshot can be put back by hand, with the panel stopped, by
  copying its files over the paths in `snapshot.json`.

The snapshot runs in the guard, that is in the code of the release being
replaced (`panel-agent.bak`): an update *to* a release with this section
snapshots only when the release it starts from has it too.

### 2.6 The system packages a release needs (#113)

A release can need an Ubuntu package the one before did not (nftables for
the block list, #21). The installer installs them, but neither the
self-update nor the guard of an older release knows a newer release's list,
so the new release brings them in itself, before it starts:

- **The list** is `internal/syspkg`: `Release()` is the installer's base and
  data-plane packages but PHP's (`syspkg.Base` and `syspkg.System()`; the
  installer's `packages.base` and `packages.main` read the same package, so
  the lists cannot drift). PHP's packages are left out: a server's PHP
  versions are its own.
- **When**: `cloudground-agent.service` runs `panel-agent ensure-packages`
  as its `ExecStartPre`, on every start of the agent, the one after a
  self-update included: the self-update installs the new unit with the new
  binaries (§2.2), the guard reloads systemd and restarts the agent (§2.4
  steps 4–5), and the check runs before the new agent does.
- **What**: `dpkg-query -W` of the list; a package not `install ok
  installed` (or `hold ok installed`) is missing. Nothing missing: nothing
  else runs (the common start). Otherwise `apt-get install -y -qq` of the
  missing ones only, as a transient root service (`systemd-run --wait --pipe
  --collect`, `RuntimeMaxSec=600`, `DEBIAN_FRONTEND=noninteractive`, apt
  waiting up to 60 s for another apt's lock), the pattern of the PHP
  extensions (ops/sites SPEC §9): `/usr` and dpkg's database are read-only in
  the agent's sandbox. A failed install is tried once more after `apt-get
  update` (package lists older than the archive).
- **Bound**: 100 s in all (`EnsureBudget`), under the 2-minute wait for the
  restart job of the guard (of this release and of the ones before it,
  `host.DefaultJobTimeout`); the unit's `TimeoutStartSec=3min` is above both.
  An apt run the check stopped waiting for finishes on its own, within its
  `RuntimeMaxSec`.
- **A failure** while the guard's unit (`cloudground-update-guard`) is
  `active` or `activating` exits 1: the agent's start fails, the guard's
  restart job fails, and the update is **rolled back** like any failed
  update (§2.4 step 8; the previous release's unit has no such check). At
  any other start (a boot without the network, say) it is logged as a
  warning with the `apt-get install` that fixes it, and the agent starts
  without the package, as before #113.

### 2.7 The kernel settings a release needs

The self-update installs binaries and units (§2.2), never the installer's
files. The installer's kernel settings (`/etc/sysctl.d/60-cloudground.conf`,
install SPEC §3 step 17) are a release's too, so the same `ensure-packages`
start brings them in, before the packages:

- **What**: the file as this release's installer renders it
  (`install.Render`). The same content: nothing runs (the common start), and
  a value changed at run time stays, as doctor reports it. A file that
  differs or is missing is written (`0644`, by rename), then loaded with
  `sysctl -p <file>` as a transient root service (`systemd-run --wait
  --pipe --collect`, `RuntimeMaxSec=60`): `/proc/sys` is read-only in the
  agent's sandbox (`ProtectKernelTunables`).
- **Why it matters**: v1.0.0 moved `ip_local_port_range` to `32768 65535`
  and refuses a new app port inside it (internal/api SPEC, `PUT
  /api/sites/{id}/app`); a server updated from the panel kept `1024 65535`
  until `cgctl install`, so no TCP port was left for an app.
- **A failure** to write or to load is a warning in the journal with the
  `sysctl -p` that fixes it, never a failed start, during an update
  included: a kernel that refuses a value does not stop the install either.

## 3. Paths

All from the agent's `Paths` (no hard-coded `/usr/local/bin`,
`/etc/cloudground/certs/panel.*`, `/run/cloudground/api.sock` in this
package): `NginxSites`, `ACMEWebroot`, `CertLiveDir`, `PanelCert`, `PanelKey`,
`PanelAccess`, `APISocket`, `BinDir`, `SystemdDir`, `WorkDir`, `StateDB`,
`Records`; `StateDir` is `dir(WorkDir)` (`/var/lib/cloudground-agent`, where
the installer reads `highest-release`), and the snapshots live in
`<StateDir>/update-snapshots`.
