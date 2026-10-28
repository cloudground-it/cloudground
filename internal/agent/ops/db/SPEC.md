# `internal/agent/ops/db` — a site's database (specification)

Everything the agent does to MariaDB on a site's behalf: create and drop the
site's database and account, the SQL console, the table list, export into
the site's tree, import from it, and the dump/import/replace helpers that
backup, restore, staging and migration share. It talks to the server only
through `host.MariaDB` (the driver as root for administration, the scoped
account for the console, `mariadb-dump` and the sandboxed `mariadb` client
for data). No SQL is ever built from text a person typed except the console
statement itself, which runs as a scoped account.

This package is also the base of Quarry (`ops/dbstudio`, plan Phase
4b), which replaced Adminer: its browse, edit, export and import run in a
`Session` (§3, `OpenSession`, with `Each` to stream rows and `Exec` for
statements without rows) and use the transfer helpers (§5).

## 1. Methods

| Method | Params → Result | Table row |
|---|---|---|
| `CreateDatabase` | `SiteDatabaseParams{Site, Name}` → `{database}` | Sites, 5 min |
| `DropDatabase` | `SiteDatabaseParams{Site, Name}` → `{dropped}` | Sites, **`LockLiveSite`**, 30 min |
| `CreateDatabaseUser` | `DBUserParams{Site, User, Password, Grants}` → `{user}` | Sites, 2 min |
| `DropDatabaseUser` | `DBUserParams{Site, User}` → `{dropped}` | Sites, 2 min |
| `SetDatabaseUserPassword` | `DBUserParams{Site, User, Password}` → `{user}` | Sites, **`LockLiveSite`**, 15 min |
| `SetDatabaseUserGrants` | `DBUserParams{Site, User, Grants}` → `{user}` | Sites, 2 min |
| `ListDatabases` | `DBListParams{Site}` → `DBListResult{Databases, Users, StatsError}` | Sites, read-only, 1 min |
| `DBQuery` | `DBQueryParams{Site, Database, SQL, ReadOnly}` → `DBQueryResult` | Sites, 3 min |
| `DBTables` | `DBTablesParams{Site, Database}` → `DBTablesResult{Tables}` | Sites, read-only, 1 min |
| `DBExport` | `DBExportParams{Site, Database}` → `DBExportResult{Path, SizeBytes}` | Sites, 2 h |
| `DBImport` | `DBImportParams{Site, Database, RelPath}` → `DBImportResult` | Sites, **`LockLiveSite`**, 3 h |

Every handler uses the request's context: a cancelled or expired call stops
the dump, the import or the query (the previous code ran them on
`context.Background()`, so an RPC that had timed out kept importing).

`DBImport` takes the site lock: it replaces the database, as a restore, a
migration and a staging push do, and those already hold it. Two of them
interleaved left the database in the state of neither.

## 2. Names, and whose each database and account is

A site has zero or more databases and zero or more accounts (Fase 9b); an
account is granted `all` (`ALL PRIVILEGES`) or `read` (`SELECT, SHOW VIEW`)
on some of the site's databases. `Config.Database` is the site's
**primary**: the database and account its application is configured with
(wp-config.php), made with the site.

- **Names are chosen**, within one shape (`internal/site.CheckDBName`):
  `^[a-z][a-z0-9_]{2,47}$` (well under MariaDB's 64 and 80; nothing in it
  needs quoting in a defaults file, a wp-config.php or a file name, and none
  starts with `-`), never a reserved name (`mysql`, `sys`,
  `performance_schema`, `information_schema`, `root`, `debian_sys_maint`,
  `mariadb_sys`, `mysql_sys`, `mariadb`, `mysqld`, `admin`, `test`,
  `phpmyadmin`, `cloudground`, `replication`, `repl`, `monitor`, `backup`),
  never a reserved prefix (`cg<letter>_`, the agent's own scoped accounts and
  scratch databases; `mysql_`, `sys_`, `mariadb_`; `test_`, which old
  installs grant every account). `ClaimableBy(name, siteID)` also keeps the
  panel's default shape `site_<n>` for site `n` alone (and any other
  `site_<digits>` for nobody): SR-R1's protection — a reserved or another
  site's name is never created, dropped, granted or opened — is kept, the
  tie to the site's id moving to the record below. `SiteDatabaseParams` and
  `DBUserParams` validate with it (run by the RPC server too), and
  `site.Validate` checks the primary's two names the same way.
- **The record** (`registry.go`): the agent's own file of which database
  and which account is which site's, with each account's grants:
  `Paths.DBRegistry` (`/var/lib/cloudground-agent/databases.json`, beside the
  site index `sites.json`, agentrpc SPEC §3.5, which guarantees a request's
  site id is the site the agent provisioned), root's, mode 0600, written
  atomically (temp file, fsync, rename) on every change, changed under one
  mutex and put back in memory when the write fails. A name is recorded
  before it is made (a crash leaves a recorded name, which a drop removes;
  never a made name nobody owns) and forgotten after it is dropped. **Every
  handler that names a database or an account checks the record before any
  SQL, session or file**: `SiteDatabase(site, name)` ("" is the primary) is
  the name only when the record gives it to `site.ID`, else `invalid` (`"x"
  is not a database of <domain>`); accounts the same (`ownedUser`).
  `Exists`, `TableNames`, `Replace` and `Stage` take only a recorded name;
  `ValidName` (a site name's shape, or `cgv_`/`cgm_`/`cgh_` + 16 hex) is the
  shape check of the internal helpers. The host adapter also never dumps,
  imports or grants `mysql`, `sys`, `performance_schema` or
  `information_schema`, whatever its caller checked.
- **Rebuild**: an install from before the record has no file. At start
  (`LoadDatabaseRegistry`, and on first use when the server was not up) the
  record is built from the server: every database and every `localhost`
  account named `site_<n>` is site `n`'s (the only names the panel made
  before), the account granted `all` on its namesake. A file that exists but
  cannot be read is **not** rebuilt over: every request naming a database is
  refused until an operator looks (`errNoRecord`).
- **A lost record** (`adopt.go`): a file deleted or moved away after an
  install whose names were chosen cannot be rebuilt from `site_<id>` names
  alone. `panel-agent adopt-databases [--state <state.db>] [--dry-run]` is
  run by root on the box **with the agent stopped** (a running agent holds
  the record in memory and would write its own over it; the command refuses
  while the agent's socket answers). It is a command of the box: no RPC and
  no API route reach it. It reads the panel's state **read-only**
  (`state.ReadDatabaseClaims`: `mode=ro`, `query_only`, no migration; a state
  from before migration 22 is refused with what to do), lists the server's
  databases and `localhost` accounts, and adopts into the record each
  claimed name that passes `ClaimableBy` for its site (never a system or
  reserved name, never another site's `site_<n>`), that the server has, that
  no other site claims (in the claims, or in the record already), and each
  grant on a database adopted for the same site. What the record already
  gives the same site stays (kept). It creates, drops and grants nothing on
  the server; it prints one line per name (adopted, kept, refused with the
  reason) and a count. `--dry-run` writes nothing. Tests:
  `adopt_test.go`, `state/database_claims_test.go`. The operator's procedure
  belongs in the docs' recovery section (cloudground-docs, later).
- **Conflicts**: creating a name the record gives another site, or one the
  server has and the record gives nobody (a database made outside the
  panel), is `conflict`; it is never taken over. One already the site's is
  made again if missing (an incomplete earlier attempt).

## 3. Console (`DBQuery`) and sessions

A **session** is a scoped account (`host.MariaDB.ScopedAccount`: `cgq_<16
hex>@localhost`, granted on the one database; read-only = `SELECT, SHOW
VIEW`) and one driver connection as that account over the Unix socket. The
server enforces both boundaries — other databases are not granted, writes
are not granted in read-only — so nothing inspects the SQL for them.

- One statement: the statement is sent alone over the protocol with
  multi-statements off, so `select 1; drop table x` is a syntax error, and
  the client commands of the `mariadb` CLI (`system`, `\!`, `\T`, `\.`) do
  not exist. A trailing `;` and blanks are trimmed; empty is `invalid`; more
  than 1 MiB is `invalid`.
- Limits: the first **5000 rows** and at most **8 MiB** of cell data are
  returned, then `Truncated` with a message. The cap on bytes keeps a
  `SELECT *` over LONGBLOBs under the RPC result limit (64 MiB), which the
  row cap alone did not.
- Time: the console gives a statement **60 s**. The connection sets
  `max_statement_time` a little below that, so the server aborts a long
  statement itself; when the deadline (or the caller) ends the call anyway,
  the agent **kills the connection as root** (`KILL <id>`). Dropping a
  scoped account does not end its running statement: before this, a
  `SELECT SLEEP(600)` or a heavy `BENCHMARK()` kept running on the server for
  its whole length after the console had given up and dropped the account.
- Errors from the server come back as `Message` (`ERROR <n> (<state>):
  <text>`), not as an RPC error; the driver's connection details never do.
  A timeout reads "the statement ran longer than 60s and was stopped".
- Close: the connection is closed and the account dropped, also when the
  request was cancelled.

`DBTables` is a read-only session running one fixed query on
`information_schema.TABLES` for `DATABASE()`: name, rows (estimate), data
and index bytes, engine, collation, largest first.

### 3.1 Sessions as a signed-in user (Fase 9b, `login.go`)

Quarry's signed-in users (ops/dbstudio SPEC §11) work in a session of
their own:

- `SiteUser(site, user)`: the record must give the user to the site
  (`invalid` otherwise), so only a site's own accounts sign in.
- `LoginSession(login, database, timeout)`: one connection over the socket
  **as the user itself** with its password (`DialSpec.Login`; no account
  is made or granted, no file registered with the driver), on `database`
  (a valid, non-system name; "" none), with the session's
  `max_statement_time` and the root `KILL` of §3 on its deadline. The name
  must have a site's shape; then `SELECT CURRENT_USER()` must be exactly
  `<user>@localhost`. A refused sign-in (`ERROR 1045`, `1044`, `1698`) or
  any other user is `ErrLoginRefused`. `Close` closes the connection only.
- `DumpAsLogin` and `ImportAsLogin`: `mariadb-dump` and the sandboxed
  client signed in as the user (host `DumpAs`, `ImportAs.Login`). The
  import stages and scans the file as `DBImport` does (§5), with no
  rollback point.

## 4. Export (`DBExport`)

`mariadb-dump --single-transaction` of the site's database, streamed into a
new file `shared/exports/<db>-<UTC yyyymmdd-hhmmss>.sql` created through the
site's `sitefs.Root`: owned by the site user, mode 0640, never through a
symlink (`O_EXCL|O_NOFOLLOW`), and `shared/exports` is created (0750) only if
missing and refused if it is anything but a real directory. A second export
in the same second gets `-2`, `-3`… instead of failing on the existing name.
A failed dump removes its partial file. The result is the absolute path and
the size.

## 5. Import and the shared helpers

`DBImport` copies `RelPath` (must end in `.sql`; a non-empty regular file,
never followed if it is a symlink) through the site's root into a fresh
directory under the agent's work dir, then `Replace(…, Over)` and commits.
Before the import it reads the copy for statements that read or write a
file (`LOAD DATA|XML … INFILE`, `INTO OUTFILE|DUMPFILE`): each kind found is
a warning in the result (`Warnings`, which the API's task log shows) and in
the agent's log, and in the error when the import fails. It is a warning,
not a filter: the refusal is the client's and the server's (below, §7).

Helpers (exported for backup, restore, staging and migration):

- `Dump(ctx, db, w, tables…)` — `host.MariaDB.Dump`.
- `DumpFile(ctx, db, path, tables…)` — into a new 0600 file, synced.
- `ImportFile(ctx, db, path)` — `host.MariaDB.Import`: the sandboxed client
  (`--sandbox --local-infile=0 --one-database`, R-0.1, SR-T1), as the
  account `importAs(db)` names (SR-T7):
  - a site's database: **as an account of that site with `all` on it** (the
    record says which: the database's namesake first, then the first by
    name), granted only the site's databases (never SUPER or SET USER), so
    the site's triggers, views and routines keep their owner; none found: a
    scoped account without `TRIGGER` (a dump with a trigger then fails,
    rather than leave a trigger whose definer is dropped). On the **box's
    own server** the agent has not the account's password, so `host`
    borrows the account, giving it a second, random password beside its own
    for the import (`ALTER USER … IDENTIFIED VIA <its plugin> USING <its
    stored hash> OR mysql_native_password USING PASSWORD(<random>)`, in a
    root-only defaults file) and putting its own authentication back alone
    when the client exits, success or not. The site keeps signing in with
    its password throughout; one borrow per account at a time; only
    `mysql_native_password` and `ed25519` accounts (whose stored string is
    the whole credential) are borrowed, others fail the import. On an
    **external server** borrowing needs `mysql.global_priv`, which a managed
    server rarely grants, so the panel sends the account's own password in
    the request (`DBImportParams`/`RestoreParams` `accounts`, user →
    password, from the secret store, only for a database on an external
    server); the agent signs in as the account the record picks, using the
    password only for that user, never one the request names otherwise. The
    administrator is never used to run a dump — only to create and drop a
    throwaway account. Without a password for that account the import falls
    back to a throwaway account scoped to the one database (with `TRIGGER`),
    `DEFINER` CURRENT_USER, and the task result/log says plainly that any
    triggers, views or routines it imported will not run until they are
    recreated;
  - the restore test's `cgv_` database: a scoped account with `TRIGGER`
    too (the database and its objects are dropped with it);
  - a staged migration's `cgm_` database: a scoped account without
    `TRIGGER`, as before (its tables are renamed into the site's database,
    which a table with triggers cannot be).
  Every `DEFINER=` clause of the dump is rewritten on the way in
  (`host.RewriteDefiners`, a streaming filter that follows quotes and
  comments and treats `/*! … */` as code) to the importing account:
  `` `site_<id>`@`localhost` `` for a borrowed account on the box's own
  server, or `CURRENT_USER` (the account signed in as) for the site account
  on an external server, a scratch database, or the external fallback.
  Before, the import ran as a scoped account without `TRIGGER`: a dump with
  a plugin's trigger (or a view of another definer) failed, and with it the
  restore, the staging push and the rollback that import such dumps. A
  `DEFINER` of a privileged account is never kept: the filter replaces it,
  and the importing account cannot set another (no SET USER), so one the
  filter missed is refused by the server. It is the only place a `mariadb` process
  is started, and its argv is the adapter's, unchanged. `--local-infile=0`:
  the client runs as root, and before it a `LOAD DATA LOCAL INFILE
  '/root/…'` in a tenant's dump imported that file into a table (measured
  on MariaDB 11.8). The driver connections (root's pool, the sessions) keep
  `AllowAllFiles` false and register no file or reader, so a `LOAD DATA
  LOCAL` sent on them reads nothing either; unit tests pin both configs.
- `Replace(ctx, db, dump, mode)` → `*Replacement`: save the current contents
  to `<work>/rollback/<db>-*.sql` (the rollback point), then import `dump`.
  `Over` imports on top of what is there (a user's import, a migration's
  SQL, a staging push of some tables). `Fresh` empties the database first
  (§6) — a restore: the database must be the snapshot, not the snapshot
  plus every table created since.
  - On a failed import, it rolls back and returns the import's error.
  - `Rollback(cause)` empties the database and imports the rollback point.
    Importing it on top, as before, left every table the failed import had
    already created (measured in e2e: `cg-evil.sql` created
    `cg_e2e_import`, then failed; the table stayed). If the rollback fails,
    the rollback point is **kept** and its path is in the error.
  - `Commit()` deletes the rollback point.
  - Rollback runs to completion even when the request's context has ended.
- `VerifyDump(ctx, path, wordpress, want)` — the restore test: create
  `cgv_<16 hex>` as root, `ImportFile` into it, count its tables (and, for
  WordPress, require an `…options` table), always drop it. Returns the count.
  `want` is how many tables (and views) the database had when it was dumped
  (the backup records it): the import must make at least that many. An
  empty database is a valid dump of an empty database (0 of 0 passes; it
  failed every time before with "the dump imported no tables", and the
  schedule retried it every five minutes); a dump that brings back fewer
  tables than its database had fails. `want < 0` (a backup from before the
  count) keeps the old rule: at least one table.
- **Staged replacement** (`Stage`, the migration's; `staged.go`): a
  database replaced without a moment where it lacks the tables it serves
  from, in moves each of which is one atomic `RENAME TABLE`:
  - `Stage(ctx, live, path)`: create `cgm_<16 hex>` (scratch) with the live
    database's character set and collation, `ImportFile` into it, list its
    base tables. A dump that makes no table, or a view (a view cannot be
    renamed into another database), is `invalid`, and the scratch database
    is dropped. The live database is not touched. `Value(ctx, table, column,
    key, value)` reads one staged row (the migration's `siteurl` check).
  - `Swap`: create `cgh_<16 hex>` (hold); one statement moves each dump
    table into the live database, a live table of that name to hold first
    (`live.t TO hold.t, scratch.t TO live.t`).
  - `Retire`: one statement moves the live tables the dump lacks to hold.
  - `Unretire`, `Unswap`: the reverse, one statement each (`live.t TO
    scratch.t, hold.t TO live.t`); `Unswap` then drops scratch, and hold
    when nothing is left retired in it.
  - `Commit`: drop hold and scratch.
  Every table name is quoted with backquotes doubled (a dump's names are
  the tenant's). Verified against MariaDB 11.8 in
  `staged_integration_test.go`.

## 6. Emptying a database

`DROP DATABASE` and `CREATE DATABASE` with the character set and collation
it had (read from `information_schema.SCHEMATA`, checked against
`^[a-z0-9_]+$`; `utf8mb4`/`utf8mb4_unicode_ci` if unknown). The site
account's grants are on the name and survive. Views, routines, triggers and
events go with it, which dropping tables one by one would leave behind.

## 7. Provisioning (`accounts.go`)

- `CreateDB`: the name checked and the conflicts of §2; `Tune` (below; a
  WooCommerce site or a commerce profile raises durability); recorded; then
  `CREATE DATABASE IF NOT EXISTS … utf8mb4 utf8mb4_unicode_ci` (a failure
  forgets a name it had just recorded).
- `DropDB`: the site's, never its primary (`conflict`); every recorded
  account of the site with a grant on it gets `REVOKE ALL PRIVILEGES` on it
  first (a database made later with the name must not inherit them; "no
  such grant", 1141/1147, is not an error); `DROP DATABASE IF EXISTS`; then
  forgotten, with every grant on it. The site's backups of it stay (the API
  leaves them to retention).
- `CreateUser`: name, password and grants checked (each grant on a database
  of the site, once); the conflicts of §2; recorded; `CREATE USER IF NOT
  EXISTS ?@'localhost' IDENTIFIED BY ?`, then `ALTER USER ?@'localhost'
  IDENTIFIED BY ? WITH MAX_USER_CONNECTIONS 50`; then the grants. A failure
  drops an account it made and forgets it.
- Grants (`applyGrants`): from what the record holds to what is asked, one
  database at a time — a grant that changes or goes is revoked first, then
  the new one granted (`ALL PRIVILEGES` or `SELECT, SHOW VIEW`); the record
  keeps what was reached, also when a step fails. Every database name in a
  GRANT or REVOKE goes through `host.GrantPattern`, which escapes `_` and `%`
  (SR-T2): they are LIKE wildcards there, and before the fix `site_1`.* also
  granted `site01`, `siteA1`, … — databases of other sites with such names,
  and any the account created for itself (CREATE was granted on the
  pattern). The scoped accounts' grants are escaped too. `SetGrants` keeps
  the primary account's `all` on the primary database (`invalid` otherwise).
- `DropUser`: the site's, never its primary (`conflict`); `DROP USER IF
  EXISTS`; forgotten.
- `SetPassword`: `ALTER USER ?@'localhost' IDENTIFIED BY ?`. The primary
  account of a WordPress site has its password in wp-config.php: its stored
  authentication is read first (only `mysql_native_password` and `ed25519`,
  whose stored string is the whole credential), the password changed, then
  `PrimaryPassword` (`wp config set DB_PASSWORD --prompt`, the value on
  stdin, as the site user in its sandbox); when that fails the account gets
  its previous authentication back (`IDENTIFIED VIA <plugin> USING <hash>`)
  and the error says so. A Laravel site's `.env` is its owner's: the panel
  never wrote one. The method takes the site's lock, as every import does:
  an import borrows the account (§5) and puts its authentication back at the
  end, which would undo a password set meanwhile.
- Passwords are bound by the driver, never in a statement's text or a log:
  12–256 characters, no control characters, none of `'` `"` `\` (the API
  generates 32 URL-safe base64 characters) → `invalid`. The three
  characters, because wp-cli writes the primary account's password into
  wp-config.php and quotes none of them right (measured, wp-cli 2.x):
  `config create` writes `"` as `\"` (WordPress gets another password),
  `config set` writes `\'` as `\\'`, which closes the PHP string and runs
  the rest of the password as code, as the site user.
- `List`: the record's databases and accounts with their grants, and one
  bound query on `information_schema.TABLES` for each database's table count
  and data + index bytes (an empty database has no row: 0 and 0); a failed
  query leaves `-1` and `StatsError`.
- `ProvisionSite` (sites.Create): the primary database, then its account
  granted `all` on it. `DropSite` (sites.Delete and the undo of a creation):
  every account, then every database the record gives the site, each
  attempted, errors joined, each forgotten once dropped.
- `Exists(ctx, name)`: one bound query on `information_schema.SCHEMATA`.
- `Tune(ctx, commerce)`, under one mutex (concurrent provisioning waits for
  the restart instead of sending SQL to a server that is starting):
  - no tuning file yet: render `tune.CalculateMariaDB` from the probed RAM
    and CPUs (shared role), write it atomically, restart `mariadb.service`
    through `host.Systemd`, and wait (driver `SELECT 1` every 250 ms, 60 s)
    until the server answers. A failure to probe, render, write or restart
    is logged and provisioning goes on with the server's defaults; a server
    that does not answer within the 60 s fails `Create` (its SQL could not
    run anyway).
  - the file exists, `commerce`, and its `innodb_flush_log_at_trx_commit`
    is not `1`: re-render it with `commerce=true` (a field of the config, not
    a text substitution) and `SET GLOBAL innodb_flush_log_at_trx_commit = 1`
    (dynamic, no restart). A failure here **fails** `Create`: a store must not
    run with a second of orders at risk without anyone knowing.
  - otherwise nothing.
- `Harden(ctx)`, at start, under the same mutex: `SET GLOBAL local_infile =
  0` on the running server; every site account still granted the old way
  (a `mysql.db` row whose `Db` equals the account's name, with a `_` or `%`
  in it) gets the escaped grant, then the pattern grant is revoked, in that
  order so the site never loses its own database; an escaped row no longer
  equals its account's name, so a second start finds nothing. Then, when a tuning file exists, append the
  security settings it lacks (`tune.HardeningBlock`, tune SPEC §4) — a file
  written by an older version is otherwise never rewritten. The tuning
  values in it are left as they are. No file yet: nothing (the first
  tuning writes them). Errors are logged by the caller; the agent serves.
  When what it appended holds a setting read only at start
  (`skip-symbolic-links`, SR-T3), it returns `restart`, which the agent
  runs in the background once: under the tuning mutex, restart
  `mariadb.service` and wait for it (5 min each). The agent serves
  meanwhile: a synchronous restart at start could outlast the self-update's
  60 s health wait and roll the update back.

## 8. Startup sweep

`Sweep(ctx)` runs once, before the agent serves (nothing can be in flight):
drops every `cgq_<16 hex>@localhost` account (exact match, each dropped by
a bound statement), every `cgv_<16 hex>` and `cgm_<16 hex>` scratch database
(exact match), and removes `<work>/.dbq-*.cnf`. It also puts back their own
authentication alone on the site accounts an import cut short left with a
second password (`host.MariaDB.Unborrow`: a `site_<id>` account whose
`mysql.global_priv` entry has more than one `auth_or` method gets its first,
its own, alone; an account of a site is one the record gives a site, or
one named `site_<n>`). Kept rollback points in
`<work>/rollback` are data and are never swept; nor is a `cgh_<16 hex>`
holding database, which after a migration cut short may be the only copy of
a site's previous tables: it is logged by name for the operator.

## 9. External servers (#30, `servers.go`)

A database or an account may be on a registered **external** MariaDB or MySQL server instead of
the box's own (internal/api SPEC §6.5.1). Server 0 is the box's; any other id is one the panel
pushed.

- **The table.** `SetDBServers{Servers: [{id, host, port, tls, ca_cert, admin_user,
  admin_password}]}` replaces the agent's list, in memory only (never written to disk; the
  passwords are secrets of the panel's store). A server that disappears or changes loses its
  adapter and open pool. `TestDBServer{Server}` connects with the credentials given, runs `SELECT
  VERSION()`, and checks the administrator can create accounts (`SHOW GRANTS` names `ALL PRIVILEGES`
  with `GRANT OPTION`, or `CREATE USER` and `CREATE`): `{version}`, or `invalid` with the reason.
  A call that names a server the agent has no connection for is `unavailable`, `unknown database
  server <id>` (the panel pushes and repeats).
- **The record** gains the server of each database (`servers`: database → id, absent for the
  box's) and of each account (`server`). Names stay one namespace across servers: a name any
  site has anywhere is a conflict, as before. `CreateDatabase{ServerID}` and
  `CreateDatabaseUser{ServerID}` record the server with the name; every later handler finds it
  from the record, so `DropDatabase`, grants, the console, tables, export, import, backups,
  restores and Quarry need no new parameter. A user's grants may name only databases of its own
  server (`invalid`).
- **Routing.** `Ops.maria(ctx, database)` is the adapter of the database's server; `mariaOfUser`
  the account's; `scratch` databases (the staged replacement's `cgm_` and `cgh_`) are made on, and
  routed to, the server of the database they stage (`Staged.maria`). Calls that name no database
  (the startup sweep, `SET GLOBAL`, the tuning, the record's rebuild) are the box's.
- **The adapter** is `host.MariaDBAdmin` with `Remote` set (host SPEC, mariadb): the same SQL, an
  account's host part `'localhost'` read as `'%'`, TLS as the server's setting says, `REQUIRE SSL`
  on every account made when TLS is required. An import into a database of an external server runs
  the sandboxed client as a throwaway account scoped to exactly that one database (never the
  administrator: `--one-database` is a client filter, not a boundary), with every `DEFINER` clause
  rewritten to `CURRENT_USER`; a managed server rarely lets the agent read `mysql.global_priv`,
  which borrowing the site's own account needs, so the site's triggers and views are owned by that
  throwaway account for the import's life rather than kept as the site's.
- **Not on an external server:** the startup sweep (its accounts and scratch databases are dropped
  by the code that made them, also when it fails), `Unborrow`, tuning, the primary database.

Tests: `servers_test.go` (routing by the record, the grants rule, a name in use on another server,
an unknown server), host `TestRemoteAdapter…` (the host rewrite, `REQUIRE SSL`, the client
arguments, the defaults file).

### 9.1 PostgreSQL servers (#81, `postgres.go`)

A server pushed with `kind: "postgres"` is PostgreSQL (TLS is always required). Everything §9 says
about the record and routing holds; what differs is the SQL (`host.PostgresAdmin`, host SPEC):

- **A database** `<db>` is made with an owner role `cgo_<db>` (`NOLOGIN`; the prefix is one
  `ClaimableBy` reserves, so no site can name it): `CREATE ROLE`, the administrator made a member of
  it, `CREATE DATABASE <db> OWNER cgo_<db>`, `REVOKE ALL … FROM PUBLIC` on the database and on schema
  `public`, which `cgo_<db>` owns. Dropping it revokes the accounts' grants, drops the database
  (`WITH (FORCE)`) and the role.
- **An account** is `CREATE ROLE <user> LOGIN PASSWORD … CONNECTION LIMIT 50` (a password is quoted
  as a literal; the password rule already bans `'` `"` `\`). Grants: **all** makes it a member of
  `cgo_<db>` and sets `role = cgo_<db>` for its sessions on that database (`ALTER ROLE … IN DATABASE`),
  so everything it creates is the owner's and every all-user sees it; **read** gives `CONNECT`, `USAGE`
  on `public`, `SELECT` on its tables and sequences now and, through default privileges of
  `cgo_<db>`, later. Revoking undoes each. The same record-first rules and conflicts as §2.
  The administrator is also made a member of each account (a non-superuser may only end the
  sessions of roles it is a member of), and an account's sessions are ended before it is dropped.
  The administrator needs only `CREATEROLE` and `CREATEDB`; `DROP DATABASE … WITH (FORCE)` is tried
  again for a few seconds when it answers `42501` (an autovacuum worker, which gives way to the lock).
- **Dump** is `pg_dump`; **import, restore and `empty`** run the dump through `Restore` as the owner
  role, after `empty` has dropped and recreated schema `public` (owned by `cgo_<db>`) and re-applied
  every read grant of the record: dropping the database instead would lose its `CONNECT` grants.
  **`TableNames`** counts the tables of schema `public`. **`VerifyDump`** names the database: for
  PostgreSQL the dump is restored into a scratch database `cgv_<16 hex>` on the same server, owned
  by a scratch owner role, and its tables counted.
- **List** reads `pg_database_size` and the table count of each database.
- **Not for PostgreSQL yet:** the SQL console, the table list and Quarry's sessions
  (`invalid`, "not available for PostgreSQL databases yet"); staging and migration (primary
  databases are never on an external server).
- **Backups.** A server pushed with `backup_dumps: false` (the provider backs it up) has its databases
  left out of a site's backup, its restore plan and its restore test; with `true`, the dump and the
  restore test above.

## 10. Not here

Quarry (`ops/dbstudio`). The backup's restic plumbing, the restore's
file swap, staging's URL rewrite and the migration's tree are their own
domains; they call §5.
