# `internal/agent/ops/dbstudio` — Quarry (specification)

Quarry is the panel's own database manager for a site (plan, Phase 4b). It
replaces Adminer, and with it the pinned PHP download, the claim-once wrapper
in the site's docroot, its sweep and the files hidden from the file manager.
The panel's API calls it over the agent's RPC; nothing of it is reachable
from a site. Since Fase 9b Quarry is a service of its own at
`/quarry` (internal/api SPEC §6.4) with two ways in: from the panel, for
a panel account that may reach the site (its roles and audit apply), or
with a database user's own name and password (§11).

It is built on `ops/db`: every SQL statement runs in a `db.Session` (a
scoped account granted on one of the site's databases only, or the
signed-in user itself, §11; one driver connection, `max_statement_time`,
`KILL` on the deadline), and export and import use the same dump and
sandboxed import as backups and restores. Between calls it holds only the
signed-in sessions of §11.

## 1. Methods

All take the site (validated centrally) and act on **its own managed
database only** (`ops/db` SPEC §2): anything else is `invalid` before an
account exists.

| Method | Params → Result | Table row |
|---|---|---|
| `DBStudioTables` | `{Site, Login?, Columns}` → `{Database, Tables, Routines?, Columns?}` | read-only, 1 min |
| `DBStudioStructure` | `{Site, Table}` → `DBStudioStructure` | read-only, 1 min |
| `DBStudioRows` | `{Site, Login?, Table, Page, PageSize, Sort, Filters, Search}` → `{Columns, PrimaryKey, Rows, Total, SQL}` | read-only, 2 min |
| `DBStudioEdit` | `{Site, Table, Changes, DryRun, ReadOnly}` → `{SQL, Affected, Executed}` | 3 min |
| `DBStudioAlter` | `{Site, Login?, Table, Kind, Column \| Index \| Confirm, DryRun, ReadOnly}` → `{SQL, Executed}` | 15 min |
| `DBStudioExport` | `{Site, Format, Tables}` → `{Name, SizeBytes, Rows}` | 2 h |
| `DBStudioFiles` | `{Site}` → `{Exports, Imports}` | read-only, 1 min |
| `DBStudioReadChunk` | `{Site, Name, Offset, Length}` → `{Data, Size}` | read-only, 2 min |
| `DBStudioUploadChunk` | `{Site, Name, Offset, Data}` → `{Size}` | 2 min |
| `DBStudioDeleteFile` | `{Site, Dir, Name}` → `{deleted}` | 1 min |
| `DBStudioImportCSV` | `{Site, Table, Name, ReadOnly}` → `{Rows}` | `LockLiveSite`, 3 h |
| `DBStudioWPAutoload` | `{Site}` → `{Table, Count, Bytes, Top}` | read-only, 2 min |
| `DBStudioWPSearchReplace` | `{Site, From, To, DryRun, ReadOnly}` → `{Replacements, Executed}` | `LockLiveSite`, 45 min |
| `DBStudioWPTransients` | `{Site, DryRun, ReadOnly}` → `{Expired, Deleted, Executed}` | 15 min |
| `DBStudioWPCleanup` | `{Site, KeepRevisions, DryRun}` → `{ExpiredTransients, ExcessRevisions, DeletedTransients, DeletedRevisions, AutoloadCount, AutoloadBytes, AutoloadTop, Executed}` | `LockLiveSite`, 30 min (§8.4) |
| `DBStudioQuery` | `{Site, Database, Login?, SQL, ReadOnly}` → `DBQueryResult` | 3 min |
| `DBStudioSimulate` | `{Site, Database, Login?, SQL, ReadOnly}` → `{Kind, Affected, Preview?, PreviewSQL?, Refused?, Message?, RolledBack}` | 3 min (§12) |
| `DBStudioSearch` | `{Site, Database, Login?, Term, Tables}` → `{Tables: [{Table, Matches}], Searched, Truncated}` | read-only, 3 min |
| `DBStudioOpen` | `{Site, User, Password}` → `{Login, Databases}` or `{Refused}` | 1 min (§11) |
| `DBStudioClose` | `{Site, Login}` → `{}` | 1 min (§11) |
| `DBStudioDatabases` | `{Site, Login}` → `{Databases}` | read-only, 1 min (§11) |
| `DBStudioImportSQL` | `{Site, Database, Login, Name}` → `DBImportResult` | `LockLiveSite`, 3 h (§11) |

`Database` names one of the site's databases ("" is its primary) for a
scoped account; with `Login` it is any database the signed-in user may
open, and it is required. The methods that take `Login` run every
statement as that user (§11) instead of a scoped account.

The SQL editor is `DBStudioQuery`: the console's rules (`ops/db` SPEC §3:
one statement, multi-statements off, a trailing `;` trimmed, at most 1
MiB, 5000 rows / 8 MiB, 60 s, the server's error as `Message`), in DB
Studio's session — a scoped account read-only unless the caller allows
writes, or the signed-in user (§11). An
SQL import is `DBImport` (`ops/db` SPEC §5), the sandboxed client with a
rollback point, reading the file Quarry's upload left in
`shared/imports`.

## 2. Who may write

`ReadOnly` is the caller's role as the API knows it (a `readonly` account,
or an operator who did not allow writes). The agent does not trust the API
to have refused: a write method called with `ReadOnly` opens a **read-only
session** (`SELECT, SHOW VIEW`), so the server itself refuses the statement
and its error comes back (`ERROR 1142 … command denied`). The WordPress
writes run as wp-cli, not as a scoped account, so they refuse a
`ReadOnly` call outright (`invalid`) unless it is a dry run. A dry run of
`DBStudioEdit` or `DBStudioAlter` reads the table's columns and builds the
SQL; it never sends a write.

## 3. Identifiers never come from input

A table or column name in a request is only ever **looked up**: the
session reads the table's real columns from `information_schema.COLUMNS`
(for `DATABASE()`, the only schema the account sees) and the name must
equal one of them, byte for byte. What goes into the SQL is the server's
own name, quoted with backticks and every backtick doubled. A name that
is not there is `not_found` ("no table x", "no column y in x"); nothing is
interpolated from the request. New names (an added column or index) must
match `^[A-Za-z_][A-Za-z0-9_]{0,63}$`.

Values are always bound (`?`) and sent apart from the statement; the
connection keeps multi-statements off.

## 4. Browse

- **Tables**: `information_schema.TABLES` for `DATABASE()`: name, type
  (`table` or `view`), engine, collation, rows (InnoDB's estimate), data and
  index bytes, auto-increment, comment; by name. With `Columns`, also
  `{table: [column…]}` in ordinal order, for the SQL editor's completion.
- **Structure**: columns (name, full type, data type, nullable, default,
  key, extra, collation, comment, whether binary), indexes from
  `STATISTICS` (name, unique, type, columns in order with their prefix
  length), foreign keys from `KEY_COLUMN_USAGE` joined to
  `REFERENTIAL_CONSTRAINTS` (columns, referenced table and columns, update
  and delete rules), the primary key, and `SHOW CREATE TABLE` (or `VIEW`).
- **Rows**:
  - `PageSize` 1–500 (0 means 50), `Page` from 0, offset at most 10 000 000.
    Browse asks for 25 rows a page unless the person picks 50, 100, 250 or
    500, and shows "rows X–Y of N" from `Total` (with `~` when
    `TotalEstimated`); its last page is the furthest within the offset
    limit.
  - Sort: up to 4 columns, each `asc`/`desc`; the primary key is appended
    so pages are stable.
  - Filters: up to 16, ANDed. Ops `eq ne lt le gt ge` (bound value),
    `contains`, `starts` (`LIKE ? ESCAPE '!'`, the value's `!`, `%` and `_`
    escaped with `!`, which reads the same with or without
    `NO_BACKSLASH_ESCAPES`), `null`, `notnull` (no value). Values up to 4 KiB.
  - Cells: a text or binary column other than the primary key is read as
    `LEFT(col, 4097)` so a LONGTEXT is never fetched whole; a cell longer
    than 4096 bytes is cut and its index listed in `Cut`. Binary columns
    (`binary varbinary *blob`) are shown as `0x` hex of their first 2048
    bytes, always as cut. NULL is `null`, never the text "NULL".
  - Total: `COUNT(*)` with the same filters; without filters, a table whose
    estimate exceeds 1 000 000 rows reports the estimate
    (`TotalEstimated`) instead of scanning it.
  - `SQL` is the SELECT as run, with its values shown as literals.
  - The page is cut at 8 MiB of cell data (`Truncated`).

- **Search** (`DBStudioSearch`, a search across the whole database): the
  columns of every table (or of the named ones, each of which must exist)
  from `information_schema.COLUMNS`; for each table with text columns
  (`char varchar *text json enum set`), one `SELECT COUNT(*) … WHERE c1
  LIKE ? ESCAPE '!' OR c2 LIKE ? …`, the term (1–256 bytes) escaped as for
  `contains`. Tables with matches are listed with their count. It stops
  after 200 tables or 150 s (`Truncated`). A `Rows` request with `Search`
  ANDs the same clause to its filters (a table without text columns then
  matches nothing), so Browse opens a table on the rows found.
- **Routines**: the database's procedures and functions
  (`information_schema.ROUTINES`, name and type) come with `Tables`. DB
  Studio lists them; it never runs, shows or changes one.

## 5. Edit

`Changes` (1–100) run **in one transaction** on one write session:
`START TRANSACTION`, each change, `COMMIT`. Any failure — a server error,
or a change that did not match exactly one row — is followed by
`ROLLBACK`, and nothing of the batch stays. The server's message (a
duplicate key, a denied command) comes back as `invalid`.

- `insert`: `INSERT INTO t (cols…) VALUES (?…)`; columns not given take
  their default. Must insert one row.
- `update`: `UPDATE t SET col = ?… WHERE pk1 = ? AND … LIMIT 1`. Must match
  one row (the connection reports matched rows, `CLIENT_FOUND_ROWS`, so an
  update to the same values still counts).
- `delete`: `DELETE FROM t WHERE pk1 = ? AND … LIMIT 1`. Must delete one row.
- `update` and `delete` need a primary key, and `Key` must give every
  primary-key column and nothing else. A table without one is
  `invalid` ("has no primary key; edit it with SQL").
- A binary column cannot be given a value here (it is shown as hex).
- NULL is a `null` value; a NOT NULL column refuses it on the server.

The SQL returned is built **together with** the statement that runs: each
identifier and each bound value is appended to both, the value as an
escaped literal for display. So a dry run returns exactly the statements a
real run executes (and returns), in the same order.

## 6. Guided ALTER

- `add_column`: `ALTER TABLE t ADD COLUMN name TYPE [UNSIGNED] [NOT] NULL
  [DEFAULT …] [AFTER col]`. `TYPE` is one of a fixed list, with `Length`
  for `CHAR VARCHAR BINARY VARBINARY` (1–65535; required for the `VAR`
  ones) and `DECIMAL` (precision 1–65, scale 0–30 and ≤ precision). The
  default is `NULL`, a number for numeric types, `CURRENT_TIMESTAMP` for
  `DATETIME`/`TIMESTAMP`, or a quoted string (no backslash, no control
  characters, single quotes doubled); `AFTER` names an existing column.
- `add_index`: `ALTER TABLE t ADD [UNIQUE] INDEX name (col[(len)]…)`, 1–16
  existing columns, prefix lengths 1–3072.
- `truncate` and `drop` (Operations): `Confirm` must be the table's name
  as the server has it, byte for byte (`invalid` otherwise, before any
  statement). `TRUNCATE TABLE t` (a view: `invalid`), `DROP TABLE t` or,
  for a view, `DROP VIEW t`. Whether it runs is the grants': a read-only
  session's is refused by the server.
- The statement is DDL and commits implicitly; it runs alone, as built,
  with a 10-minute statement time.

## 7. Export and import

Files live in the site's tree, created through its `sitefs.Root`, owned by
the site user, never through a symlink:

- `shared/exports` (0750): what Quarry and `DBExport` write. Names
  `<db>-<UTC yyyymmdd-hhmmss>[-n].sql`, `<db>-<table>-<stamp>[-n].csv`.
- `shared/imports` (0750): what the panel uploads.

A file name in a request matches `^[A-Za-z0-9][A-Za-z0-9._-]{0,150}$` and
ends `.sql` or `.csv`; anything else is `invalid`.

- **Export** `sql`: `mariadb-dump` of the database, or of the named tables
  (each must exist), streamed into the file (the dump's stdout is the
  file: nothing passes through the agent's memory). `csv`: exactly one
  table, `SELECT *` streamed row by row through a read-only session into
  `encoding/csv`, a header row of column names, NULL written as `\N`.
  Either stops at **16 GiB** and removes the partial file; a failure
  removes it too.
- **Files**: both directories' regular files (name, size, modified),
  newest first.
- **ReadChunk**: `Length` 1–4 MiB of an export at `Offset`; the API loops
  over it to stream a download of any size with a constant buffer.
- **UploadChunk**: at most 8 MiB per call. `Offset` 0 creates the file anew
  (a previous file of that name is replaced); otherwise the file must exist
  with exactly `Offset` bytes and the data is appended. The file may grow
  to **16 GiB**. The API streams a browser upload as a sequence of these.
- **DeleteFile**: one file in `exports` or `imports`.
- **ImportCSV**: the file in `shared/imports` (opened without following a
  symlink, regular), read with `encoding/csv`; the first record names the
  columns (each must exist; not binary), every record has as many fields,
  `\N` is NULL. Rows go in batches (500 rows or 4 MiB) of one bound
  multi-row `INSERT`, all in one transaction: a failure rolls back every
  row. It takes the site lock, as `DBImport` does.

## 8. WordPress

The site must be WordPress (`wordpress` or `woocommerce`); else `invalid`.

- **Autoload**: the table prefix comes from wp-cli (`wp db prefix`, must
  match `^[A-Za-z0-9_]{1,32}$`); in a read-only session, the `options` table
  of that prefix (must exist): count and total bytes of the options loaded
  on every request (`autoload` in `yes on auto auto-on`), and the 20
  largest.
- **Search and replace**: `wp search-replace <from> <to>
  --all-tables-with-prefix --precise --skip-columns=guid --format=count`,
  with `--dry-run` for the preview. wp-cli unserialises and re-serialises
  PHP values, so lengths in serialized data stay right, which a SQL
  `REPLACE` breaks. `from` and `to`: 1–1000 bytes, different, no leading
  `-` (it would read as an option), no control characters. After a real
  run the site's caches are refreshed (`ops/wp` Refresh).
- **Transients**: counts the expired ones (`_transient_timeout_%` and
  `_site_transient_timeout_%` older than now) in a read-only session; a
  real run is `wp transient delete --expired`, which also clears an
  object cache, then the count again, and a refresh.

### 8.4 Cleanup (#78)

`DBStudioWPCleanup`, for the scheduled cleanup of internal/api SPEC §3.17: `KeepRevisions` is 1–100
(else `invalid`; it goes into the statement as a number), the site must be WordPress.

1. **Revisions**, in a read-only session on `<prefix>posts`: those beyond the newest `KeepRevisions`
   of each post, ranked per parent (`ROW_NUMBER() OVER (PARTITION BY post_parent ORDER BY post_date
   DESC, ID DESC)`, `post_type = 'revision'`, rank above the number kept), counted.
2. **Transients**: as §8, a dry run or the real deletion.
3. **Deletion of revisions** (not on a dry run): the IDs of 500 surplus revisions at a time, as the
   server answers, go to `wp post delete <ids> --force` (wp-cli removes a revision's meta and terms
   with it, and runs the site's hooks), until none is left or 100 batches have run (50 000
   revisions; a larger backlog goes on at the next run). `DeletedRevisions` is the difference
   between the count before and after, not the number asked for. The site's caches are refreshed.
4. **Autoload**: the options WordPress loads on every request, measured as §8 after the deletions
   (before, on a dry run); `AutoloadTop` keeps the five largest.
5. A real run appends one line (time, what was deleted, the autoload size) to
   `<LogRoot>/<domain>/cleanup.log`, written by the agent as root with `O_NOFOLLOW`
   (0640, in the root-owned log directory a site user cannot write); the panel's site log
   viewer reads it as the `cleanup` kind. A line that cannot be written is logged and does not fail
   the cleanup.

## 9. Timeouts

`DBStudioRows`, `Structure`, `Tables`, `Autoload`: the console's 60 s. Edit:
60 s per statement. Alter: 10 min. Export and CSV import: 2 h 50 min per
statement. Each session's `max_statement_time` is a little below; when the
deadline passes, the connection is killed as root (`ops/db` §3).

## 10. Not here

The SQL editor's history is the browser's, per user and site: statements
can carry secrets, and the panel does not store them. The audit trail is
the API's: each write is recorded with its action, table and primary key,
never row contents.

## 11. Signed-in database users (Fase 9b)

Quarry is also a service of its own (internal/api SPEC §6.4): a person
signs in with **a database user's own name and password**, without a
panel account, and sees and changes exactly what MariaDB grants that user.

- **Who may sign in** (`DBStudioOpen`): only an account the record gives
  the request's site (ops/db SPEC §2, `SiteUser`). Never `root` (the agent
  runs as root, and root's `unix_socket` sign-in over the socket would
  pass whatever password was typed), never the agent's `cgq_` accounts,
  never the server's own. The agent connects as `<user>@localhost` over
  the socket with the password, no default database, and requires
  `CURRENT_USER()` to be exactly `<user>@localhost` (an anonymous account
  the server matched instead is a refusal). A wrong password, a name of
  another site, an unknown name, a refused match: all the same
  `{Refused: true}`, and nothing is kept. Then `SHOW DATABASES` as the
  user, without `mysql`, `sys`, `performance_schema`,
  `information_schema`, is the list the tree shows.
- **The session** is a handle (32 random bytes, hex) in the agent's
  memory: the site, the user and its password, **sealed** (AES-256-GCM
  with a key drawn at start and never written; the handle is the
  additional data). The password is opened for one connection at a time
  and is nowhere else: not on disk, not in the panel's store, not in a log
  (the API keeps only the handle, §6.4 of its SPEC). A session ends after
  35 minutes unused (the API's own idle time is 30), 12 h 5 min after it
  began, on `DBStudioClose`, when the agent restarts, and when the server
  stops taking the password. At most 32 per site and 256 in all
  (`rpc.DBStudioMaxSiteLogins`, `rpc.DBStudioMaxLogins`, the bounds the API
  refuses a sign-in at); past either, the one unused the longest ends,
  which is then a session the API no longer holds. `CheckOnly` (the API has
  no room) checks the name and password the same way and keeps nothing.
- **Every statement as the user**: `Tables`, `Structure`, `Rows`, `Edit`,
  `Alter`, `Query`, `Search`, `Export` (CSV through the session, SQL as
  `mariadb-dump` signed in as the user, host `DumpAs`) and `ImportCSV`
  open a new connection as the user, on `Database`, with the same
  `max_statement_time` and root `KILL` on the deadline as a scoped session
  (ops/db §3), and close it after the call: nothing (a transaction, a
  `LOCK TABLES`, a session variable) lasts from one request to the next.
  No scoped account is made; `ReadOnly` adds nothing the server could
  enforce, so it is the grants that refuse a read-only user's write
  (`ERROR 1142`). `LOAD DATA LOCAL` stays off (no file is registered with
  the driver, ops/db §5), and the multi-statement and statement limits of
  §1 hold.
- **What the server refuses**: a database the user may not open (`ERROR
  1044`) is `invalid` ("<user> has no access to the database <db>") and
  the session stays, after one probe without a database confirms the
  password still works. A password the server no longer takes (it was
  changed in the panel, the user was dropped) ends the session:
  `unauthorized` ("…sign in again"), which the API turns into the end of
  its own session. A handle given with another site is unknown.
- **SQL import** (`DBStudioImportSQL`): the uploaded `shared/imports`
  file (the name rule of §7, `.sql`), staged and scanned for statements
  that read or write a file as `DBImport` does, then the sandboxed client
  (`--sandbox --local-infile=0 --one-database`) as the user, with its
  password in a root-only defaults file that is removed afterwards (host
  `ImportAs.Login`), every `DEFINER` rewritten to the user. There is **no
  rollback point**: making one and putting it back would take privileges
  the user may not have. A failure stops at the statement the server
  refused, and the message says that what ran before stays.
- **Files**: they are the site's (`shared/exports`, `shared/imports`); the
  API shows a signed-in session only the files it made itself (internal/api
  SPEC §6.4).

## 12. Simulate

`DBStudioSimulate` is phpMyAdmin's "simulate query": one write statement
run in a transaction that is **always rolled back**, to see how many rows it
would change before running it for real. It has the SQL editor's limits
(§1: one statement, 1 MiB, 60 s, multi-statements off, `max_statement_time`
and root `KILL` on the deadline) and session (the signed-in user, or a
scoped account, read-only when `ReadOnly`).

- **What may be simulated**: a statement whose first word is `UPDATE`,
  `DELETE`, `INSERT` (also `INSERT … SELECT`) or `REPLACE`. Everything else
  is `Refused` with the reason, before any connection: a read (`SELECT`,
  `WITH`, `SHOW` … "changes nothing: run it, or use Explain"), and every
  statement that commits by itself or cannot be undone (`CREATE ALTER DROP
  TRUNCATE RENAME LOCK UNLOCK SET GRANT REVOKE START BEGIN COMMIT ROLLBACK
  SAVEPOINT XA LOAD CALL DO HANDLER …`). The statement is lexed three times,
  as each sql_mode reads strings: a backslash escaping in `'…'` and `"…"`
  (the default), in neither (`NO_BACKSLASH_ESCAPES`), and in `'…'` only
  (`ANSI_QUOTES`, where `"…"` is a name), so that whatever the server's
  mode: a `;` outside a string in any reading
  ("one statement at a time"), an executable comment (`/*! … */`, `/*M! …
  */`, whose content runs), an unclosed string, name or comment, or
  unbalanced parentheses refuse it.
- **Shapes are an allowlist**: `UPDATE` and `DELETE` of one table or
  several, `INSERT` and `REPLACE` `[INTO] <table> [PARTITION (…)]
  [(columns)]` followed by `VALUES`, `VALUE`, `SELECT`, `(SELECT …)` or
  `SET`, with or without `RETURNING`. Refused, in any reading: another
  `INSERT` form (`TABLE`, `WITH`), `DELETE HISTORY`, an `INTO` other than the
  `INSERT`'s (a variable, `OUTFILE`, `DUMPFILE`), `PROCEDURE`, a user
  variable assigned (`:=`), a locking read (`FOR UPDATE`, `FOR SHARE`, `LOCK
  IN SHARE MODE`: it locks rows the statement does not change), and a
  `SET STATEMENT … FOR` prefix.
- **Calls are an allowlist** (`simulate_shape.go`), so a trick nobody
  listed is refused by default: in every reading, each word, `` `name` ``
  or `"name"` a `(` follows must be a clause's word (`IN`, `VALUES`,
  `EXISTS`, `USING`, `JOIN`, `OVER` …), the table an `INSERT`/`REPLACE`
  writes (its column list follows), or an unqualified built-in of the
  list: the string, number, date, JSON, dynamic-column, cast, control-flow,
  aggregate and window functions, and those that tell who the session is.
  `SLEEP`, `BENCHMARK`, the `*_LOCK` functions, `LOAD_FILE`, `NEXTVAL`,
  `LASTVAL`, `SETVAL`, `LAST_INSERT_ID`, `MASTER_POS_WAIT`,
  `MASTER_GTID_WAIT`, `BINLOG_GTID_POS` and `UUID_SHORT` (it advances a
  counter of the server's) are not on it, nor is any stored, package or
  loadable function or a qualified name (`db.f(`): `Refused` "Simulate
  supports built-in functions only". The routine and UDF lookups below stay
  as a second line.
- **The session's sql_mode**: once connected, `SELECT @@SESSION.sql_mode`;
  one with `ANSI_QUOTES` (also as part of `ANSI`, `ORACLE`, `MSSQL` …,
  which the server lists expanded) or `NO_BACKSLASH_ESCAPES` is `Refused`
  before anything else runs: the checks then need only the default reading
  (the other two stay, as a second line). The statement cannot change the
  mode (`SET` and functions that could are refused).
- **What it may reach**: every word, `` `name` `` and `"string"` of 64
  characters or less of any reading is a name to check (at most 1000), as
  written; a bare number is not, but digits after a name's dot are
  (`db.123`) and a quoted name of digits is (`` `123` ``). The server
  lowers both sides of every comparison (`LOWER(TABLE_NAME) IN
  (LOWER(?), …)`): Go's lower case differs from the server's for some
  capitals, and a comparison without `LOWER` may look the name up on disk,
  case-sensitively.
  In the session, `information_schema.TABLES` for those names in
  `DATABASE()` and in any schema so named: no table at all, a view, a
  sequence or a system view, or a table whose engine is not InnoDB
  (MyISAM, Aria, MEMORY …: their changes are not rolled back) is
  `Refused`, naming it; so are names that match more than 10000 tables or
  databases (the check never looks at only some). Then **as root**, because a scoped account lacks
  `TRIGGER` and a user may not see a trigger that runs as its definer: a
  trigger on any of those tables, a stored routine of any type so named in
  those schemas (or another the user sees so named; a package's functions
  are called through the package's name, `pk.f()`), or a loadable function
  (UDF, `mysql.func`) so named is `Refused`. A column called
  like a MyISAM table refuses too: the check errs on the safe side.
- **One budget**: once connected, the whole simulation (checks, preview,
  statement; not the rollback, which has its own) has 60 s. The preview is
  sent with `max_statement_time` set to what is left less 5 s (in its `SET
  STATEMENT`), the statement after `SET SESSION max_statement_time` to what
  is left then less 5 s, so the server stops either itself; with under a
  second left the next one is not sent and `Message` says the budget was
  used up (rolled back, nothing changed). Past the budget the agent kills
  the connection, as for any statement.
- **The run**, on the session's one connection: `SET SESSION
  innodb_lock_wait_timeout = 5, lock_wait_timeout = 5` (a simulation waits
  seconds, not the server's 50 or a day, for a lock the live site holds:
  while it waits it keeps the rows it has locked, and the site waits on
  them); `START TRANSACTION`; then
  `SELECT @@in_transaction` must be 1, else nothing more runs (`Refused`);
  for a one-table `UPDATE` or a `DELETE FROM` (no `USING`, no multi-table
  form) the **preview** `SELECT * FROM <its tables> <its WHERE / ORDER BY /
  LIMIT>` (cut out of the statement where every reading agrees, never with an
  `INTO`), at most 25 rows and 1 MiB, the rows as they are before the
  statement (a preview the server refuses is left out). It is sent as
  `SET STATEMENT sql_select_limit = 26, max_statement_time = <left> FOR
  <preview>` and read to its end,
  the rows past what is shown dropped: cutting a result off drops the
  connection, and with it the transaction. Then the statement, whose
  matched-rows count (`CLIENT_FOUND_ROWS`) is `Affected` (with a top-level
  `RETURNING`, which answers rows and no count, the rows it returns are
  counted instead); then `ROLLBACK`.
- **Always rolled back**: the `ROLLBACK` runs deferred, with a context of
  its own, on every path — the statement's error (its message is `Message`),
  a timeout or a client that went away (the connection is killed and the
  call answers the context's error), a panic. The session is closed after
  it in every case, and a server rolls back the open transaction of a
  closed connection; a failed `ROLLBACK` is an error saying so and is
  logged. `COMMIT` is never sent. `RolledBack` says the server took the
  `ROLLBACK`.
- **What stays**: nothing in the tables. As with any rolled-back write, an
  `AUTO_INCREMENT` counter may have moved on, and so may a sequence a column
  takes its default from (`DEFAULT NEXTVAL(s)`, not named by the statement:
  measured on MariaDB 11.8, the sequence stays advanced after the
  rollback; a sequence the statement names is refused, being one).
- The API audits it like a write (internal/api SPEC §6.4,
  `database.simulate`, redacted) and needs no `allow_writes`, since nothing
  stays.
