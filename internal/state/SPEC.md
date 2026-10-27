# internal/state — behaviour spec (state v1, R-2.1)

The store is the panel's single source of truth: what every site and the panel
itself *should* look like. The agent renders the system from it; nothing in
it is derived from the system except the observations named below
(certificate status, applied generation). The API is the only user.

## Entities

| Entity | Table | Notes |
| --- | --- | --- |
| Users, sessions, setup tokens, site assignments | `users`, `sessions`, `setup_tokens`, `user_sites` | see Accounts below; `users.locale` (migration 12) holds the chosen interface language; `users.whats_new_seen` (migration 28, #106, `SetUserWhatsNewSeen`) the release whose changelog entry the account last saw, `''` until one; migration 16 dropped `totp_secret` and `totp_pending`; migration 30 (#156) added `users.password_signin` (default 1) and `users.webauthn_id` (the account's random WebAuthn user handle, NULL until its first passkey) |
| Passkeys, recovery codes | `user_passkeys`, `user_recovery_codes` | migration 30 (#156, `passkeys.go`, api SPEC §2.10–2.11): a passkey row per credential (`credential_id` unique across the panel, COSE `public_key`, `sign_count`, `aaguid`, `transports`, `backup_eligible`, `backup_state`, `rp_id`, `name`, `created_at`, `last_used_at`); a recovery code row per code (`code_hash` the hex SHA-256, `used_at` NULL while unused); both go with their account (cascade) |
| Sites | `sites` | desired config (JSON `config` column, unchanged) plus `desired_generation`, `applied_generation`, `last_apply_error` |
| Jobs (the tasks of the UI) | `jobs` | migration 15 replaced `tasks` (R-4.2): status, bounded log, payload, locks, dedup key; see internal/jobs/SPEC.md. `error_key`, `error_args` (added to `tasks` by migration 14, carried over): a failure's translation key and its values (JSON), when the error was a `KeyedError` (an agent error with a key), beside the English `error` |
| Deployments | `deployments` | a state machine (R-4.3, `deployments.go`; the statuses and moves are internal/api/SPEC.md §4.2): `MoveDeployment(id, from, to, update)` takes a deployment from one of `from` to `to` in one `BEGIN IMMEDIATE` transaction and refuses (`*StateError`, `ErrDeploymentState`) a move the table lacks or a deployment no longer in `from`; becoming `promoted` (and `CreateDeployment` of a live record) makes the site's other promoted deployment `superseded` in the same transaction; `RecordRollback(site, release)` makes the live one `rolled_back` and the record of `release` `promoted`, refusing (`ErrNeverLive`) a release whose record was never live or is `regressed`; `RecordInitialRelease(site, status)` (a record of provisioning's release `initial` when the site has none, checked and inserted in one transaction; a site names each release once, `UNIQUE (site_id, release_id)` since the first schema), `RollbackTarget` (the live one's `rollback_to`, else the newest superseded), `SyncLive(site, release)` (the records follow what the server serves), `DeploymentMeta` (`rollback_to`, `regressed`, `note`). Migration 18: at most one `promoted` row per site (`idx_deployments_live`, unique, partial); of the rows promoted before it the newest stays, the others become `superseded`; a `guarding` row the old panel left becomes `created` (it had built its release) or `failed` (none); columns `rollback_to`, `regressed`, `note` |
| Backups | `backups` | `backups.go`: a record starts `pending` whatever the caller says, only `SetBackupVerified` moves it; `databases` (migration 23, #15) the names of the databases its snapshot holds, comma-separated, `''` for none, NULL (Go: nil) for a record from before; `LatestVerifiedBackup` (per site and repository), `UnverifiedBackups`, `DeleteBackup`, `DeleteBackupsBySnapshot` (IDs match when one is a prefix of the other, from 8 characters, case aside) |
| Cron jobs, audit, drift, metrics | `cron_jobs`, `audit_events`, `managed_files` + `drift_events`, `metric_samples` | unchanged tables; the Go side is specified under "Records" below (R-4.3) |
| Site databases | `site_databases`, `site_db_users`, `site_db_grants` | migration 22 (Fase 9b): a site has 0..N databases and 0..N database accounts; a grant is `(user, database, level)` with `level` `all` or `read` (CHECK). Names are UNIQUE across the server (MariaDB has one namespace each): `AddSiteDatabase`/`AddDBUser` answer `ErrDBNameTaken`. A user's `secret` is the name of the secret holding its password (`site.<id>.db_user.<name>` for new users; a user migration 22 made keeps `site.<id>.db_password`, sealed under that name, AEAD-bound). `AddDBUser` writes the row, its grants and its password in one transaction, refusing a grant on a database the site does not have (`ErrNotFound`) and a secret that is not one of the site's; `RemoveSiteDatabase` takes its grants (cascade); `RemoveDBUser` its grants and its secret; deleting the site takes everything (cascade). Every write publishes `databases.changed`. Migration 22 gives every site with a local managed `config.database` that database and account as its first rows, the account granted `all`, names and password unchanged; `config.database` stays the **primary** (what wp-config.php / .env use) |
| Site usage | `site_usage`, `site_usage_hourly` | migration 24 (#18, `site_usage.go`, api SPEC §11.5): `RecordUsage` inserts a round of samples (a site that no longer exists is skipped), folds each into its site's hour (sums, peaks, the last disk use measured) and deletes samples older than a day and hours older than 30 days, in one transaction; times are fixed-width UTC text so they sort; `UsageSince`, `SiteUsageSince`, `LatestUsage`, `SiteUsageHours`; deleting a site takes its rows (cascade) |
| Site vulnerabilities | `site_vulnerabilities` | migration 25 (#31, `site_vulnerabilities.go`, api SPEC §11.6): one row per site, `checked_at` (the last complete check, `''` before one), `attempted_at`, `error`, `report` (JSON: core version, components looked up, findings); `RecordVulnCheck` replaces the row, `RecordVulnCheckError` sets only the attempt and its error (findings kept); a site that no longer exists is skipped; `SiteVulnReport`, `VulnReports`; deleting a site takes its row (cascade) |
| Site malware | `site_malware_scans`, `site_malware_findings` | migration 26 (#76, `site_malware.go`, api SPEC §11.7): one scan row per site (`scanned_at` the last complete scan, `attempted_at`, `error`, `files`, `signatures`, `duration_ms`, `engine`, `database`); one finding per site, path and signature (UNIQUE), `status` `open`, `quarantined` or `gone` (CHECK). `RecordMalwareScan` (one transaction): every file found is open (`first_seen` kept while it stays open, new when it comes back), every other open one gone, at most 2 000 kept per site; `RecordMalwareScanError` keeps the findings; `SetMalwareFindingStatus` marks a file's open findings quarantined (with the quarantine's id) or gone; `SiteMalwareScan`, `LastMalwareScans`, `MalwareFindings`, `OpenMalwareFindings`, `GetMalwareFinding`; deleting a site takes its rows (cascade) |
| Block list | `blocklist`, `blocklist_settings` | migration 27 (#21, `blocklist.go`, internal/blocklist SPEC, api SPEC §7.4): one row per `(kind, prefix)`, `kind` `block` or `allow` (UNIQUE: `PutBlockEntry` of a pair already there replaces its source, note, failures, time and expiry); `expires_at` fixed-width UTC text (NULL: until removed), `PruneBlockEntries(now)` deletes the blocks ended by `now`; `BlocklistSettings` is one JSON row, the defaults while there is none |
| Git deploy | `site_git_deploys`, `site_git_deliveries` | migration 31 (#24, `git_deploy.go`, api SPEC §15): one `site_git_deploys` row per site (`site_id` PK, cascade; `enabled`, `repo`, `branch`, `build_command`, `public_key`, `hook_id` UNIQUE: what the webhook URL carries); the webhook secret and the deploy key's private half are the sealed secrets `site.<id>.git_webhook_secret` and `site.<id>.git_deploy_key`. `GitDeploy(site)` (`ErrNotFound` before it was turned on), `GitDeployByHook(hookID)` (one indexed read), `SaveGitDeploy` (an upsert; a `hook_id` another site holds is `ErrGitHookTaken`; a new hook id is how the URL is rotated). `site_git_deliveries`: one row per authenticated delivery (`site_id` cascade, `at`, `forge`, `event`, `ref`, `commit_sha`, `outcome`, `detail`, `deployment_id` NULL when none (SET NULL when the deployment goes), `task_id`); `AddGitDelivery` keeps the newest 30 of the site, `GitDeliveries(site)` newest first, `SetGitDeliveryTask`, `SetGitDeliveryCommit` (the commit a deployment was built from, for the deliveries that named none) |
| Panel settings | `panel_settings` | one typed row, `id = 1`; `vulnerability_check` (migration 25, default 1); `malware_scan` (migration 26, default 0); `mail` (migration 29, #77, api SPEC §14), JSON `{relay: {host, port, security, username, from}, alert_emails}`, default `{}` (the relay's password is the secret `mail_password`) |
| MCP server | `mcp_clients`, `mcp_grants`, `mcp_actions` | migration 32 (#70, `mcp.go`, api SPEC §16): a client per dynamic registration (`id` the `client_id`, `name`, `redirect_uris` as JSON, `created_at`, `last_used_at`); a grant per consent (`user_id`, `client_id`, `scopes` as JSON, `access_hash` and `refresh_hash` the SHA-256 of the current tokens, unique, with their expiries, `prev_refresh_hash` the one rotated out, `created_at`, `last_used_at`); a pending action per destructive tool call (`grant_id`, `user_id`, `tool`, `args` JSON, `summary`, `status`, `result`, `created_at`, `expires_at`, `decided_at`). Deleting a user deletes its grants and actions, deleting a client or a grant its grants and actions (cascade); `revokeAPITokens` deletes the user's grants too; `ClearMCP` empties the three tables in one transaction. `panel_settings.mcp_enabled` (default 0) is the switch |
| Secrets | `secrets` | encrypted values; `site_id` set for `site.<id>.*` names; `lookup` (migration 19): the hex SHA-256 of a connector token, unique, NULL for every other secret |
| Per-site observed state | `site_status` | `cert_expiry`, `cert_error` |
| Alerts | `alerts` | migration 19 (R-4.3), replacing `alert_dismissals`: one row per condition, see invariant 6 |
| Panel bookkeeping | `meta` | `rendered_version`, `secret_key_check` |
| API tokens | `api_tokens` | migration 13: owner, name, SHA-256 of the secret (`token_hash`, unique), a display `hint`, optional expiry, `last_used_at` (written at most once a minute). Resolving a token refuses an expired one, a disabled owner and an owner who is not an admin; revoking deletes the row; the writes that end a user's sessions (`UpdatePassword`, `SetUserPassword`, `DisableUserTOTP`, and `UpdateUserAccess` when it disables or demotes) delete their tokens in the same transaction; deleting the user deletes their tokens |
| Legacy key/value | `settings` | emptied by the v1 import; no Go API reads or writes it any more |

## Invariants

1. **No secret in plaintext at rest.** Every value in the table below lives
   only in `secrets`, sealed with AES-256-GCM. The `settings` table holds none
   of them after Open with a key, and the bytes they occupied are gone from
   the file (`secure_delete`, then `VACUUM` and a truncating WAL checkpoint
   after an import that moved anything).

   | Name | What |
   | --- | --- |
   | `backup_password` | restic repository password |
   | `backup_s3_secret_key` | object-storage secret key |
   | `cloudflare_api_token` | DNS token for wildcard certificates |
   | `notify_webhook` | alert webhook URL: whoever holds it can post as the panel |
   | `site.<id>.db_password` | the password of the site's primary database user (the one made with the site) |
   | `site.<id>.db_user.<name>` | the password of another of the site's database users (migration 22) |
   | `site.<id>.connector_token` | the site's connector token |
   | `site.<id>.staging_password` | a staging site's HTTP password |
   | `user.<id>.totp` | the account's active TOTP secret (migration 16) |
   | `user.<id>.totp_pending` | a TOTP secret being enrolled, not yet confirmed |
   | `backup_repository:<URL>` | the password in a repository URL's userinfo (`rest:https://user:pw@host/`); `<URL>` is the URL as stored, with `{sealed}` for it |

   `backup_repository` and `backups.repository` keep the URL, with the
   password of its userinfo, if any, replaced by `{sealed}`; the password is
   the secret `backup_repository:<that form>` (`repo_credentials.go`).
   Writes (`UpdatePanelSettings`, `CreateBackup`) seal it, reads put it
   back, and a lookup by repository (`LatestVerifiedBackup`, `CountBackups`,
   `DeleteBackupsBySnapshot`) compares the sealed form (`RepoKey`, also what
   `jobs.RepoLock` hashes): callers see and pass the URL they stored. Two
   URLs that differ only in the password are one repository, with the
   latest password. A store opened without a key reads the sealed form.
   Rows written before (in plaintext) are sealed on Open with a key, and the
   file compacted. A `user.<id>.` secret has no `site_id`; it goes in the
   transaction that deletes its account.

2. **The key.** 32 bytes from `crypto/rand`, hex in a file (default
   `/var/lib/cloudground/secret.key`, `CLOUDGROUND_SECRET_KEY`), mode 0600,
   created on first Open with `O_EXCL` and given the state database's owner
   when Open runs as root (so `recover-admin` as root cannot leave the API
   user a key it cannot read). The key is never logged or returned.
   `meta.secret_key_check` holds an HMAC of a fixed label under the key:
   - key file missing while the database has secrets or a check → Open fails
     (a new key would orphan every secret; it is never regenerated silently);
   - key present but the check differs → Open fails (wrong key).
   Each ciphertext is bound to its name (AEAD additional data), so a row
   copied under another name does not decrypt. Format: `0x01 || nonce(12) ||
   ciphertext+tag`.

3. **Secret API.** `Secret(name)` returns `""` and no error when the name is
   absent. `SetSecret(name, "")` deletes. `DeleteSecret`,
   `DeleteSecretsWithPrefix("site.<id>.")`. A `site.<id>.` name must refer to
   an existing site (foreign key); deleting the site deletes its secrets in
   the same transaction, plus its `site_status` and `alerts` rows
   (cascade). A Store opened without a key path (`recover-admin`) has no
   secret store: the secret calls return `ErrNoSecretKey`, and the legacy
   import of secrets waits for the next Open with a key.

4. **Panel settings** are one typed row (`panel_settings`, `CHECK (id = 1)`),
   read with `PanelSettings()` and changed with
   `UpdatePanelSettings(func(*PanelSettings) error)` inside one write
   transaction. Typed columns rather than a JSON blob: the row is small and
   changes rarely; a column per field makes a new field an explicit migration
   with a default, lets SQLite enforce types, and maps directly onto sqlc
   later. A JSON blob would bring back the untyped bag we are removing.
   Defaults: empty strings, `require_2fa_admin = false`,
   `require_2fa_all = false` (migration 30: the two make the requirement
   `off`, `admins` or `all`, api SPEC §1.2 step 7),
   `update_check = true`, `update_channel = 'stable'` (migration 21; a CHECK
   keeps it `stable` or `beta`). An empty `probe_target` means the built-in
   `https://127.0.0.1`.

5. **Desired versus applied.** `desired_generation` starts at 1 and
   `MutateSite` increments it when, and only when, `fn` changed the desired
   configuration — anything but `status`, timestamps and the generation
   fields themselves. Generation fields set by `fn` are ignored: the store
   owns them. `MarkApplied(id, gen, err)` records the outcome of rendering
   the site as it was at generation `gen`:
   - success: `applied_generation = gen`, `last_apply_error = ''`;
   - failure: `last_apply_error = err`, `applied_generation` unchanged;
   - a report older than `applied_generation` is ignored (a slow task cannot
     roll the record back).
   `MutateSiteApplied` is `MutateSite` for a change the agent has already
   made itself (SFTP on/off, a wildcard certificate): it carries
   `applied_generation` forward to the new generation only if it had caught
   up with the old one. The handlers' revert-on-failure stays until Phase 4;
   a revert is a desired change like any other, so after a failed render the
   site shows `desired > applied` with the error that caused it — which is
   the truth. A put-back after a failed object-cache switch, a cancel or a
   refused enqueue restores what was rendered and is marked applied
   (internal/api SPEC §3.5).
   Sites imported from before v1 start at desired 1, applied 1 if active.

6. **Alerts** (R-4.3) are rows of `alerts`, one per condition the API's
   evaluator found (internal/api/SPEC.md §11):
   - `key` (unique) is the condition's kind and subject (`task:42`,
     `certificate.expiry:site:7`, `service:nginx`, `drift`); `kind` the
     UI's kind (`certificate`, `task`, `service`, `drift`, `deploy`); `site_id` the
     site it is about, NULL for a server-wide one (`ON DELETE CASCADE`);
     `subject` what a server-wide alert is about (a deleted site's domain),
     read back as the alert's site.
   - `fingerprint` names the occurrence; `public_id` is the first 12 hex
     digits of SHA-256(`key` NUL `fingerprint`), what the API calls the
     alert's id (unique).
   - `level`, `message` (English), the text's translation (`text_key`,
     `text_count` (−1: no count), `text_args`, and for a task the error's
     own `detail_key`, `detail_args`, JSON arrays of strings), `at`,
     `raised_at`, `seen_at`, `dismissed_at`, `notified_at`.

   `RaiseAlert(a)` upserts by key in one transaction: a new key inserts; the
   same fingerprint refreshes level, message, text and `seen_at` and keeps
   dismissal and notification; a new fingerprint replaces the occurrence
   (new `public_id`, `raised_at` now, dismissal and notification cleared).
   `ClearAlert(key)` deletes. `SyncAlerts(kind, set)` raises every alert of
   `set` and clears every other row of `kind`, in one transaction.
   `DismissAlert(public_id)` sets `dismissed_at` (`ErrNotFound` for an
   unknown or already dismissed id). While `meta.alerts_upgraded` exists
   (set by migration 19 on a database with accounts), an inserted row is
   born notified; `EndAlertsUpgrade()` deletes the key after the first
   evaluation. `ListAlerts()` returns the undismissed
   ones, errors first and newest first, with the site's domain;
   `AlertsToNotify(since)` the undismissed ones not notified after `since`;
   `MarkAlertsNotified(ids)`. Events: a raised occurrence publishes
   `alert.raised`, a cleared, replaced or dismissed one `alert.cleared`,
   with its site; a refresh publishes nothing. An alert whose `site_id`
   names no site (a task of a site deleted since) is skipped, not raised.

7. **Connector tokens** are found by hash: `SetSecret` of a
   `site.<id>.connector_token` stores the token's SHA-256 in `lookup` with
   the sealed value (in the same statement), so
   `SiteForConnectorToken(token)` is one indexed read; the stored token is
   then opened and compared in constant time, and the site read. An unknown
   token is `ErrNotFound`. Rows sealed before migration 19 get their
   `lookup` at Open, after the secrets moved (idempotent; needs the key). A
   token two sites share (never minted so) never stops Open: the first keeps
   the index, the second is stored unindexed (the legacy move and the
   backfill both skip the conflict) and its purges are refused.

## Records (R-4.3)

- **Metrics**: `RecordMetric(m)` inserts a sample and keeps the newest 288
  (24 hours at the five-minute sampling), in one transaction, then publishes
  `metrics.sample`; `ListMetrics(n)` returns the newest `n` (1–288, else
  288), oldest first.
- **Cron jobs**: `CreateCronJob`, `GetCronJob`, `ListCronJobs(site)` (by
  id), `UpdateCronJob` (schedule, command, description, enabled; site, last
  run and creation stay), `DeleteCronJob` (`ErrNotFound` when absent),
  `UpdateCronRun(id, status)` (last run now; a job deleted meanwhile is not
  an error). Every write publishes `cron.changed` with the job's site.
- **Audit**: `Audit(actor, action, subject, detail)` appends;
  `ListAuditEvents(n)` the newest `n` (default 100).
- **Drift**: `RecordManagedFile(path, sha)` upserts, `ManagedFiles()` maps
  every path to its hash, `RemoveManagedFile(path)`; `CreateDriftEvent`
  opens one and `ResolveDriftEvent(id, restored|accepted)` closes it, each
  publishing `drift.changed` (a resolve only when it changed a row);
  `ListDriftEvents(includeResolved)` the newest 200, open ones only unless
  asked.

## Sites (`site_records.go`, R-4.3)

The `sites` row holds the scalar fields as columns and the aliases and the
`config` as JSON (an empty alias list is stored as `null` or `[]` and read
back as no aliases). The calls:

| Call | Does |
| --- | --- |
| `CreateSite(site)` | inserts the row with `created_at = updated_at = now` and answers it as read back (ID and generations assigned, desired 1, applied 0); a domain already in use → `ErrDomainTaken` (the API no longer reads the driver's message); publishes `site.changed` `created` |
| `GetSite(id)`, `GetSiteByDomain(d)` | one row, or `ErrNotFound` |
| `ListSites()` | every row, by domain |
| `ListSitesForUser(uid)` | the sites assigned to the user and the staging copies of those, by domain |
| `StagingSiteFor(prodID)` | the row whose `staging_of` is prodID, or `ErrNotFound` |
| `MutateSite(id, fn)`, `MutateSiteApplied(id, fn)` | invariant 5: `fn` runs on the row read inside a `BEGIN IMMEDIATE` transaction; its error aborts and is returned as is (not wrapped); the ID and the generation fields are the store's; `updated_at` is set; publishes `site.changed` `updated` |
| `MarkApplied(id, gen, err)` | invariant 5; publishes `site.changed` `applied` only when a row changed; a deleted site is not an error |
| `SiteJobs(siteID, kinds...)` | the site's jobs of those kinds, oldest first, without logs (the API reads later settings changes from their payloads) |
| `DeleteSite(id)` | the site's secrets and the row in one transaction (the rest cascades), `ErrNotFound` when there is no row; publishes `site.deleted` with `staging_of` |

Every `site.changed` event carries `reason`, `status`, `staging_of`,
`desired_generation` and `applied_generation`. What counts as a change of
the desired configuration (invariant 5) compares every field but the
status, the two timestamps and the three generation fields, an empty alias
list equal to none.

## Migrations

A numbered list of steps (`schema.go`, the SQL in `schema_*.go`), each a list
of statements run in one transaction with its `schema_migrations` row; a
step's position is its version (the runner refuses a list out of order).
Released steps are history: installed panels ran them, so their number and
the schema they leave never change, and a change is a new step at the end.
`testdata/schema.golden` pins the schema after every step (each table's SQL,
normalised, its columns, foreign keys and indexes), `schema_open.golden` the
same after an Open with a key; an upgrade from a database stopped at any
version must reach both. So a server that skips releases (v0.1.0 straight to
v0.3.0) runs every step it lacks, in order, at the first Open of the newer
build: the steps travel in the binary, not in the release.

Migrations only go forward. **A database at a version newer than this
build's last step is refused** (`SchemaNewerError`, R-6.5): Open fails with
the version it found, and the API does not start, rather than run on tables
it does not know. The way back from a release is the snapshot taken before it
first started (`internal/agent/ops/panel` SPEC §2.5), which a failed update
puts back on its own. `TestOpenRefusesANewerSchema`.

Open then sets the file and its `-wal`/`-shm`
sidecars to mode 0640. Migration **11** is the v1 schema: it creates `secrets`,
`meta`, `panel_settings`, `site_status` and `alert_dismissals`, adds the
generation columns, and copies out of `settings` everything that does not need
the key (panel settings, certificate state of existing sites, dismissals,
`rendered_version`), deleting what it copied and every orphan row of a site
that no longer exists.

The secrets need the key, so they move in Go at Open, after the migrations,
in one transaction: every legacy secret row with a non-empty value whose site
(if any) exists is sealed into `secrets` (an existing `secrets` row wins), and
every legacy secret row is deleted. It is idempotent: a second Open finds
nothing to move and changes nothing. The file is then compacted so the
plaintext is gone from it. No `state.db.pre-v1` copy is kept: it would be a
plaintext copy of every secret.

Migration **16** moves the users' TOTP secrets into the secret store the
same way. Its SQL copies every non-empty `users.totp_secret` and
`users.totp_pending` into a holding table, `totp_import (user_id, slot,
secret)` (`slot` is `active` or `pending`; the rows go with their user), and
drops both columns from `users`. At Open with a key, after the settings
secrets, each held row is sealed as `user.<id>.totp` or
`user.<id>.totp_pending` (an existing sealed value wins) and deleted from
`totp_import`, in one transaction. Whenever `totp_import` exists at an Open
with the key, the file is compacted (VACUUM, then a truncating WAL
checkpoint, retried while a reader holds the WAL and logged loudly if it
still cannot be emptied) and only then is the table dropped, so a crash
between the move and the compaction compacts at the next Open. Without a key the rows wait there, as the legacy settings
secrets do; turning a user's 2FA off or deleting the user removes its held
rows too, so a later Open does not bring a secret back.

Migration **22** (Fase 9b) creates `site_databases`, `site_db_users` and
`site_db_grants` and, in SQL alone, gives every site whose `config.database`
is enabled, not external and named (read with `json_extract`) its database
and account as rows, the account granted `all` and its `secret` the
existing `site.<id>.db_password` (a sealed value cannot be renamed in SQL:
the name is its additional data, so the row names it instead). Nothing else
changes: `config.database` is the site's primary. `TestMigration22MovesTheSiteDatabase`.

## Accounts

`accounts.go`, `sessions.go`, `totp.go`, `apitokens.go`; the rules above
them are in `internal/api/SPEC.md` §2.

- **Users.** `CreateUser`, `GetUser`, `GetUserByEmail` (each with
  `TOTPEnabled`), `ListUsers` (by id, with every operator's `SiteIDs`, read
  in one more query), `CountUsers`, `UpdateUser` (role, disabled, names and
  sites in one transaction), `UpdateUserAccess`, `UpdateUserProfile`,
  `UpdateUserLocale`, `SetUserSites` (a site id ≤ 0 or of no site is refused
  and nothing changes), `SetUserPassword` / `UpdatePassword`, `DeleteUser`.
  `ErrNotFound` for an id with no account.
- **The admin rule.** One query decides "would this leave no enabled admin"
  (`role='admin' AND disabled=0`), inside the transaction of every update,
  disable and delete; the answer is `ErrLastAdmin`. `CountActiveAdmins`
  counts the same set.
- **Access.** `UserCanAccessSite`: an assigned site, or the staging copy of
  one.
- **Sessions.** Stored by the SHA-256 of the token (`TokenHash`).
  `UserForSession` resolves and records a use (at most once a minute);
  `PeekSessionUser` resolves without recording. Either deletes and refuses a
  session that is expired, idle for over `SessionIdleTimeout` (2 h) or of a
  disabled account. `RevokeOtherSessions`, `DeleteSession`,
  `DeleteSessionByHash`, `ListSessionsForUser` (the sessions that still
  work: neither expired nor idle; newest first), `SetSessionClient`.
  `CreateSession` first deletes every expired or idle session of any
  account (`PruneSessions`), and after the insert ends the account's oldest
  sessions past `MaxSessionsPerUser` (20), publishing `session.revoked` when
  it ends any.
- **Setup tokens.** `CreateSetupToken` (hashed, with an expiry; it deletes
  every earlier token in the same transaction, so only the newest is valid),
  `ConsumeSetupToken` (one atomic update: unknown, expired or used →
  `ErrNotFound`).
- **TOTP.** `SetPendingTOTP` seals the pending secret; `PendingTOTP` opens
  it (`""` when none); `ActivatePendingTOTP(id, checked)` moves it to the active name,
  turns `totp_enabled` on and resets `totp_last_step` (`ActivatePendingTOTPAt(id, checked, step)` sets it to `step`, the confirming code's, which the accounts service uses), in one transaction,
  only if the pending secret is still `checked` (the one a code was verified
  against; else `ErrPendingChanged`; `ErrNotFound` without a pending
  secret); `UserTOTP` returns the active
  secret and whether 2FA is on: on with no readable secret is an error, never
  "off"; `DisableUserTOTP` turns it off, deletes both secrets and the
  account's API tokens, and its recovery codes when it has no passkey; `MarkTOTPStepUsed` moves the replay watermark
  forward only, and reports whether the step was fresh. All but
  `DisableUserTOTP`, `MarkTOTPStepUsed` and the enabled flag need the key
  (`ErrNoSecretKey`).
- **Passkeys and recovery codes** (#156). `User` carries `PasswordSignIn`,
  `Passkeys` and `RecoveryCodesLeft`, read with the account. `WebAuthnID`
  makes the account's user handle on first use (32 random bytes, kept);
  `UserByWebAuthnID`. `AddPasskey` (refuses a credential id already stored:
  `ErrPasskeyTaken`), `Passkeys(user)`, `PasskeyByCredentialID`,
  `RecordPasskeyUse(id, counter)`, `RemovePasskey(user, id)` (`ErrNotFound`
  for one of another account; `ErrLastPasskey` for the last one while
  password sign-in is off; when no factor is left, the recovery codes and
  API tokens go in the same transaction). `ReplaceRecoveryCodes(user,
  hashes)` swaps the whole set in one transaction; `SpendRecoveryCode(user,
  hash)` is one `UPDATE … WHERE used_at IS NULL` and reports whether it
  spent one. `SetPasswordSignIn(user, on)` (off refuses an account without
  a passkey: `ErrNoPasskey`). `ResetSecondFactors(user)` removes TOTP,
  passkeys and recovery codes, turns password sign-in on and ends the
  account's sessions and API tokens; it needs no key, so host recovery can
  run it. `SetUserPassword` also turns password sign-in on. None of these
  needs the secret key: nothing in them is secret.
- **Events.** Every change to an account publishes `user.changed` and
  `session.revoked` for it (its open streams re-check their session);
  ending sessions publishes `session.revoked`.

Migration **19** (R-4.3) creates `alerts`, drops `alert_dismissals`, and adds
`secrets.lookup` with a unique partial index. The old dismissals do not
carry over: they named hashes of computed alerts, and the next evaluation
raises each live condition afresh, so an alert dismissed before the upgrade
shows once more.

## Concurrency

One `*sql.DB` with one connection: one writer, and readers queue behind it.
Read-modify-write goes through a function run inside a `BEGIN IMMEDIATE`
transaction that reads the row it writes (`MutateSite`,
`UpdatePanelSettings`); the function must not call back into the Store. The
API's per-site lock (`freshSite`) serialises read → agent call → write
sequences; the store only promises that each write is whole.

## Not in v1 (later steps)

sqlc (Phase 4 rewrites the queries; hand-written SQL here keeps this diff
reviewable), `context.Context` on every call, typed site columns instead of
the `config` JSON, `jobs`/`job_log`, `managed_files.site_id`, retention jobs.
