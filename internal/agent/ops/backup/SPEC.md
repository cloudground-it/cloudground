# `internal/agent/ops/backup` — backups, restores and restore tests (specification)

A site's backups live in one restic repository the operator configures
(local path, SFTP, REST, S3, B2). A **backup** is a snapshot of each of the
site's databases (zero or more, Fase 9b), then one of its files. The file
snapshot's ID is what the panel records; it names each database snapshot in
a tag. Every backup is restore tested before it counts (`VerifyBackup`),
every database in it included, and a restore is the same steps done for
real.

restic runs only through `host.Restic` (secrets in the environment, typed
exit codes, JSON decoders). Nothing here executes restic, mariadb or any
other tool itself.

## 1. Methods

| Method | Params → Result | Table row |
|---|---|---|
| `RunBackup` | `BackupParams{Site, Repository, Password, Kind, Env}` → `BackupResult{SnapshotID, SizeBytes, Databases}` | Sites, 7 h |
| `RestoreSnapshot` | `RestoreParams{Site, Repository, Password, SnapshotID, Mode, Env}` → `{"restored", "target", "mode"}` | Sites, `LockLiveSite`, 7 h |
| `VerifyBackup` | `RestoreParams` → `VerifyResult{Passed, RestoreMillis, Detail}` | Sites, 7 h |
| `TestBackup` | `TestBackupParams{Repository, Password, Env}` → `{"reachable": true, "snapshots": n}` | 15 min |
| `BackupRetention` | `RetentionParams{Site, Repository, Password, KeepWithinDays, Protect, Env}` → `RetentionResult{Forgotten, Kept}` | Sites, 4 h |
| `ForgetSnapshot` | `ForgetSnapshotParams{Site, Repository, Password, SnapshotID, Env}` → `ForgetSnapshotResult{Forgotten, Missing}` | Sites, 4 h |
| `PruneRepository` | `TestBackupParams{Repository, Password, Env}` → `{"pruned": true}` | 4 h |

Names and parameters are the API's; they do not change here.

A **snapshot ID** is 8 to 64 hex digits. Repository and password are
required by every method (`invalid` otherwise). **A site's databases** are
the ones the agent's record gives it (ops/db SPEC §2): only those are
dumped, restored or tested; an external database is not one of them.

## 2. Running restic

- **The repository** of every method is checked first (`repoOf`) by
  `internal/backuprepo` (its SPEC): local only under
  `/var/backups/cloudground/` or `/mnt/`, `s3:`/`rest:` on a public address
  (`rest:` https only) unless `CLOUDGROUND_BACKUP_PRIVATE_ENDPOINT=1`, `b2:`,
  `sftp:` with no user or host starting with `-`, `rclone:` only with
  rclone installed. A refusal is `invalid` with the rule's message, before
  restic runs.
- Every call has its own bound, inside the request's deadline: metadata
  (`cat config`, `init`, `snapshots`) 2 min; data (`backup`, `restore`,
  `dump`, `check`) 6 h; the retention plan 10 min, its prune 3 h; the
  `forget` of an abandoned snapshot 2 min. A hung repository fails; it does
  not hold the repository lock for every other site.
- **Cancellation.** The request's context is restic's: when the API goes
  away, or the method's deadline or a call's own bound passes, restic is
  sent SIGINT and given `host.ResticGrace` (20 s) to remove its lock from
  the repository and exit; then it and its process group (an SFTP backend's
  `ssh`, say) are killed (`host.OSExec`, host SPEC). A stopped backup leaves
  no lock behind and the next one runs. (It used to be SIGKILLed at once,
  which left its lock file in the repository.)
- **Errors.** restic's typed exits become messages a user may read:
  wrong password → `invalid` "the backup repository password is wrong";
  locked → `conflict` "the backup repository is locked by another restic
  process"; missing → `not_found` "the backup repository does not exist".
  Anything else keeps the method's `Fail` message; restic's stderr goes to
  the detail (logs), never to the browser.

## 3. The repository lock

One lock per repository URL, in the agent, mirroring restic's own: `init`,
`check` and `forget --prune` are **exclusive**; `backup`, `restore`, `dump`,
`snapshots` and the retention plan are **shared**. Waiting for it honours
the request's context, and a waiting exclusive holder goes before new shared
ones (a stream of backups cannot starve a prune). Two concurrent `restic
init` against one fresh path corrupt it (measured on a test server: every
later call failed "ciphertext verification failed"); this lock is what
prevents it on one server.

**ensureRepo** (exclusive): `cat config`; success means initialised. A wrong
password stops there (initialising over it is refused by restic anyway, and
says nothing useful). Anything else runs `init`; if that fails too, the
error of `cat config` is returned (it says why the repository could not be
read), with the init failure joined. restic before 0.17 (Ubuntu 24.04's
0.16) exits 1 for everything, so "anything else" cannot be narrowed to
"does not exist" there.

## 4. RunBackup

`Kind` is `files`, `database` or `full` (empty is `full`); anything else is
`invalid`. A files snapshot is always taken: it is the restore point.

1. ensureRepo, then the shared lock.
2. **Databases** (kind `database`/`full`): each of the site's databases in
   turn (none: none). Its table count is read first (`TableNames`: tables and
   views), then `mariadb-dump` (through `ops/db`) writes into a pipe whose
   other end is restic's stdin: `backup --stdin --stdin-filename database.sql
   --tag site:<domain> --tag kind:<kind> --tag part:database --tag
   dbname:<database> --tag tables:<count> --json --quiet`. One that fails forgets
   the database snapshots made before it, and the backup fails. The
   dump is never written to disk, least of all into the site's tree, which
   its user owns (N-2: they could swap the file between the dump and the
   backup, and a restore imported what they put there). If the dump fails,
   restic has stored a truncated stream: that snapshot is forgotten, and the
   backup fails. A restic that dies closes its end; the dump gets `EPIPE`
   rather than blocking.
3. **Files**: `backup --json --quiet --tag site:<domain> --tag kind:<kind>
   [--tag db:<database snapshot id>]… --exclude <root>/tmp <root>`, one
   `db:` tag per database snapshot.

   **What is left out** (`TransientDirs`): the site's `tmp/` alone. It is
   PHP's `upload_tmp_dir` and holds its sessions (`session.save_path =
   <root>/tmp/sessions`, internal/phpfpm): files PHP creates and deletes at
   any moment (its session GC runs inside requests), which the site needs
   only while they last. One that vanished between restic listing the
   directory and reading it failed the whole backup (exit 3). What a site
   root holds, from the code that makes it: `current` and `releases/` (the
   code), `shared/` (wp-config.php, uploads, the object-cache drop-in),
   `.ssh/` (SFTP keys), `logs/` and `tmp/sessions` (sites/create
   `makeTree`). Its nginx and PHP logs are not in it (they live in
   `/var/log/cloudground/<domain>`), so the panel writes nothing to `logs/`:
   what is there is the tenant's own, and stays in the backup. A file the
   tenant deletes there mid-backup still fails it, as below: exit 3 is not
   accepted for any path, since a backup that is not whole does not count.
   **Where it runs** (R-10.5): this is the one restic call that walks a
   tree the tenant controls while it runs, and restic runs as root. A
   directory the tenant swaps for a link in the middle of the walk would
   otherwise be resolved by root with the whole machine in view, and what
   it led to would be in the snapshot, which a restore hands to the
   tenant. So it runs through `Restic.RunSandboxed` (host SPEC: restic,
   the sandboxed files backup): a transient root service whose mount
   namespace starts empty and holds only the site's root (read-only, at its
   own path), restic's cache, a local repository (read-write) and the
   read-only system files restic needs. Whatever a swapped link names
   outside that is not there. Running restic as the site user instead was
   rejected: it would need the repository's password and credentials,
   which open every site's backups, and could not read the root's own
   entries of the tree. The database dump (step 2), `restore` (into the
   root-only work directory, then `sitefs.CopyTree`), `snapshots`,
   `forget`, `check` and `init` walk no tenant tree and run directly.
4. If the files snapshot fails (including restic's exit 3, some files
   unreadable: a backup that is not whole does not count), the database
   snapshots of step 2 are forgotten (orphans nothing names), and so is a
   files snapshot restic still made.
5. Result: the files snapshot's ID, the bytes every snapshot processed, and
   the names of the databases it holds (empty for none; the panel records
   them, and a restore may name some of them, #15).

A **reverse-proxy site** is never backed up (the API's: internal/api SPEC
§5.3): its tree holds no content of its own and it has no database, so its
restore test could only find an empty tree and fail at every slot.

## 5. RestoreSnapshot

`Mode` is `full` (default), `files` or `database`; anything else is
`invalid`, before the repository is touched. The server holds the site's
lock. Work happens in a fresh root-only directory under the agent's
`WorkDir`, removed at the end.

**The snapshot is the site's** (SR-T6): before anything is restored, in
every mode and in the restore test (§6), the file snapshot must carry
`site:<domain>`, must have been recorded by this server (restic's
`hostname` equal to `os.Hostname()`, the name retention scopes to, §8), and
must not be a `part:database` snapshot; otherwise `invalid` ("snapshot … cannot
be restored here: it is not a backup of <domain>" / "it was made on …").
Before, a files restore checked nothing of the snapshot's own tags and
swapped in whatever tree the ID named, another site's included. A snapshot
of a server that has since changed its hostname is refused too: the
repository is still readable with restic by hand.

**Database source.** Each `db:<id>` tag of the file snapshot names a
database snapshot (`databaseParts`). Each must exist, carry `site:<domain>`
and `part:database`, have this server's hostname; its `dbname:` tag names
the database it holds (one without, from before Fase 9b, is the site's
primary; a site without a primary cannot take it: `invalid`), the same name
twice is `invalid`; its `database.sql` is dumped (`restic dump`) into
`<work>/<database>.sql`. A snapshot without a `db:` tag predates R-0.2: the
`logs/db-latest.sql` inside its files may be the site user's, so `full` and
`database` restores of it (for a site that has databases) are refused
("restore it in files mode"). A `database` restore of a site with no
database is `invalid`.

**Which databases** (`restorePlan`): `Databases` names some (each must be in
the backup and still a database of the site, else `invalid`); empty is every
database of the backup the site still has. A database the backup holds and
the site no longer has is skipped and named in the result (`skipped`):
create it again to restore it. The result names what was restored
(`databases`). A `files` restore that names databases is `invalid`.

**Database import**: for each database, `ops/db.Replace(…, Fresh)`: the
current contents are saved to a root-only rollback point, the database is
dropped and recreated (tables created since the backup do not survive), the
dump is imported through the sandboxed client (ops/db SPEC §5). A failed
import rolls back that database and every one replaced before it, the last
first, before the error returns.

**Mode `database`**: import, commit, then refresh the site's PHP
(`refreshSiteState`, ops/wp SPEC §7: `wp cache flush` writes a new object-cache
generation that PHP-FPM picks up, then a reload; a restart when that fails)
and empty the page cache. Without
the refresh a WordPress site with the object cache on kept serving the
options it had cached before the restore.

**Modes `files` and `full`**:

1. `restic restore <id> --target <work>/files`.
2. The restored tree must contain the site's root as a directory, and its
   `current` link must name a release (`release.ParseLink`), else `invalid`.
3. A staging directory `.cg-restore-*` (root's, 0700) is made beside the
   site root, on the same filesystem; its name starts with a dot, which no
   domain can. The restored root is copied into `<stage>/new` with
   `sitefs.CopyTree` (nothing followed, the site user's from the start),
   absolute links of the pre-R-2.3 layout are rewritten relative,
   `tmp/sessions` is made (0750, the site user's) when the snapshot has
   none (§4: backups leave `tmp/` out, and PHP cannot start a session
   without it), the **SSH keys are the live site's**, and
   `release.LockSkeleton` makes the root, `releases/` and `.ssh` with every
   file in it root's (release SPEC §1.1).
   The panel keeps no key list of its own: the live site's
   `.ssh/authorized_keys` and `.ssh/shell_keys`, which only the `SSHKeys` RPC
   writes, are the list. So the restore reads them from the live root
   (`sitefs`, the final name not followed, regular files only, 1 MiB at most)
   before the swap, removes the snapshot's `.ssh` from `<stage>/new` whole,
   and writes the live files back (root:<site group>, 0750 / 0640). A live
   site without `.ssh` gets none; a key revoked after the snapshot stays
   revoked, and a key file planted in the snapshot never comes back. The
   restore test (§6) does not need this: it never goes live.
4. `full` with databases: the imports (above).
5. **Swap**: `renameat2(RENAME_EXCHANGE)` of `<stage>/new` and the live root,
   through `sitefs` on their parent: one syscall, no moment without a site
   root. The old tree is now `<stage>/new`.
6. The site's PHP master is **restarted** (its mount namespace binds the
   directory that was at the root's path when it started, and a restart also
   empties OPcache and APCu), nginx is reloaded, and the live `current` must
   resolve to a release directory (`release.Live`).
7. If 6 fails: exchanged back, the database rolled back, PHP restarted and
   nginx reloaded again; the error says what failed. If the exchange back
   itself fails, the staging directory is kept and the error names it.
8. Success: the database rollback point is discarded; the page cache is
   **emptied, not removed**: nginx creates a `fastcgi_cache_path` root only
   at start or reload, and www-data cannot recreate it under the root-owned
   cache directory (N-3; it used to be removed, and the site stopped
   caching). The staging directory, holding the old tree, is removed.

## 6. VerifyBackup — the restore test

It does everything a restore does short of touching the site, and it is
never weaker than one: a backup it passes can be restored.

1. The shared lock; the clock starts.
2. `restic restore <id> --target <work>/files`.
3. Databases: every database snapshot the backup names (as §5); a site that
   has databases and a backup with none fails; each dump is read into the
   work directory. A site with no database is tested on its files alone.
4. The restored tree must pass the restore's own checks (§5.2: the site root
   and a `current` link naming a release that is a real directory in the
   snapshot) and hold at least one regular file, counted by a `sitefs.Walk`
   that follows nothing. (A snapshot whose `current` was broken used to
   pass, while a restore of it was refused.)
5. **Every dump**, one after the other: its first 4 KiB must look like SQL
   (`CREATE TABLE` or the `MariaDB dump` header; or, for a PostgreSQL
   database, which may have no table, a whole `pg_dump`: it starts with
   the lines `--` and `-- PostgreSQL database dump` exactly and its last
   4 KiB hold the closing `-- PostgreSQL database dump complete`, which
   pg_dump writes only once it has dumped everything, so a truncated dump is
   refused); only the two ends are read, never the whole file. Then `ops/db.VerifyDump`: it is imported into a
   throwaway `cgv_<16 hex>` database through the same sandboxed import a
   restore uses, must create at least as many tables as the database had
   at the dump (its `tables:` tag; an empty database passes with none; a
   snapshot without the tag needs at least one), the primary of a WordPress
   site an options table, and is always dropped. Any one failing fails the
   test, naming the database. The detail names each with its table count
   (`<n> files restored, <k> databases imported (<db>: <n> tables; …) and
   verified`).
6. The elapsed time is the recovery estimate.
7. `restic check --read-data-subset=10%` under the exclusive lock.

A failure at any step is `Passed: false` with a `Detail` the panel shows; a
restic failure's detail is its user-safe message (§2), the full error is
logged. A cancelled request returns the cancellation, not a verdict.

## 7. TestBackup

ensureRepo, then `snapshots --json` (shared lock), decoded strictly: output
that does not decode is an error, never "reachable, 0 snapshots".

## 8. BackupRetention

`KeepWithinDays` ≥ 1; every `Protect` entry a snapshot ID. Exclusive lock
throughout.

1. Plan: `forget --dry-run --json --tag site:<domain> --host <hostname>
   --keep-within <n>d`; another site, or another server sharing the
   repository, is never in scope. Decoded with `host.DecodeForgetPlan`.
2. A group with nothing to keep is not acted on (keep-within always keeps a
   group's newest: an empty keep list means the output is not what it looks
   like). A planned removal with an invalid ID fails the run.
3. Removals matching a protected ID (either may be a prefix of the other:
   the panel may hold a short ID) are kept.
4. Every kept files snapshot keeps the database snapshot its `db:` tag
   names (they are taken a moment apart and can fall in different groups at
   the window's edge).
5. The rest: `forget --prune <ids…>`. Nothing to remove runs nothing.

## 9. ForgetSnapshot (R-4.3)

Deletes one backup of a site, which the panel asks for by its files
snapshot's ID (full or short). Exclusive lock throughout (a prune).

1. `SnapshotID` must be a snapshot ID (`invalid` otherwise, before the
   repository is touched; also `rpc` validation); repository and password
   are required.
2. The site's snapshots are listed: `snapshots --json --tag site:<domain>`,
   decoded strictly (output that does not decode fails). A snapshot of
   another site is never in that list, whatever ID is asked for.
3. The ID is matched against the list, either being a prefix of the other.
   None: the result is `Missing: true` (the panel then drops its record),
   and every database snapshot of the site that no files snapshot of the
   list names in its `db:` tag is forgotten (the files half went without
   it; nothing can restore from it; under the exclusive lock no backup is
   between its two snapshots); they are the result's `Forgotten`. Two: `invalid` (`ambiguous`): a short ID that
   names two snapshots names neither.
4. A snapshot tagged `part:database` is the database half of a backup:
   `invalid`, delete the backup it belongs to.
5. The database snapshot its `db:` tag names goes with it, when the list
   has it as a `part:database` snapshot of the site; one the tag names that
   is gone is not an error.
6. `forget <files id> [<database id>]` (full IDs), without `--prune`, on
   the plan's bound (§2): the data goes at the next `PruneRepository`. restic's typed failures keep their messages (§2: wrong
   password `invalid`, locked `conflict`, missing repository `not_found`).
7. Result: the full IDs forgotten.

## 10. PruneRepository (R-4.3)

`restic prune` under the exclusive lock, on the prune's bound (§2): the
data of snapshots forgotten without a prune (deletions). The panel runs it
from a daily job, only when a deletion left something to prune.

## 11. Tests

`backup_test.go` drives the real adapter (`host.ResticCLI` on `host.OSExec`)
against a shell stand-in for restic on `PATH`, and `ops/db` on
`host.FakeMariaDB`; it runs as root in the container suite. It covers the
streamed dump and its tags, a failed dump or files snapshot leaving no
orphan, the database never read from the site's tree, foreign and legacy
database snapshots refused, the exchange and its undo, the cache root kept,
the database-mode refresh, the restore test's scratch import and its
parity with the restore's checks, retention's protection and pairing, the
lock, cancellation killing restic and its children, a deletion taking
the database half with it and refusing a database half, an ambiguous or
invalid ID (`forget_test.go`), a cancelled or timed-out
backup leaving no lock (the stand-in removes its lock on SIGINT, as restic
does), `tmp/` left out and `tmp/sessions` made again by a restore.

e2e (`e2e/features_test.go`, `backups`): R-0.2, R-0.3, R-0.6, R-0.9, R-0.10,
R-3.6, and a database-mode restore read back through the object cache.
`R-4.6 backup interruption` (`e2e/backup_interrupt_test.go`): a backup
stopped mid-run (its RunBackup call to the agent's socket cut, which
cancels the request's context) leaves `restic list locks` empty and the next
backup succeeds; files appearing and vanishing under `tmp/` during a backup
do not fail it, and the snapshot holds nothing of `tmp/`; restic on the
server has typed exit codes (≥ 0.17).
