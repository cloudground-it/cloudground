# `internal/agent/ops/migration` — importing a site from another host (specification)

A migration replaces a site's code, uploads and (optionally) database with
what an operator brought from another host: a `.tar.gz` of the old site's
tree and a `.sql` dump, both uploaded into the site's own tree (SFTP or the
file manager). The agent runs as root, and both inputs are under the
tenant's control until the moment root has copied them, so everything here
treats them as hostile. The code is written from this file; the tests check
it.

## 1. Method

| Method | Params → Result | Table row |
|---|---|---|
| `ImportMigration` | `MigrationParams{Site, Archive, SQL?, OldDomain?, ReleaseID, MaxRatio?}` → `MigrationResult{ReleaseID, Files, Bytes, Skipped}` | Sites, **`LockLiveSite`**, 7 h |
| `PullEstimate` | `PullParams` → `PullEstimate{Files, Bytes, WordPress, Shell, HostKey, FreeBytes, Fits}` | Sites, read-only, 5 min |
| `PullMigration` | `PullParams` → `PullResult{Archive, SQL, Files, Bytes, HostKey}` | Sites, `LockLiveSite`, 7 h |
| `PullCleanup` | `PullCleanupParams{Site, ReleaseID}` → `{"removed": n}` | Sites, 1 min |

The server has validated the site and holds its lock, so no deploy, restore,
staging push or second migration runs on the site meanwhile. The handler
uses the request's context for the long steps (extraction, build, import);
the rollback runs to completion on a context that the request's deadline
does not end.

## 2. Inputs

Refused as `invalid` before any file is touched:

- `ReleaseID` is not a release ID (`site.ValidReleaseID`; `initial` is not
  one);
- `OldDomain`, when given, is not a valid domain;
- `Archive` does not end in `.tar.gz` or `.tgz`; `SQL`, when given, does not
  end in `.sql` (case-insensitive);
- `SQL` is given without `Database` and the site has no local managed
  database (`Config.Database.Enabled` and not `External`); or, for a
  WordPress site, `Database` names another database than the primary (its
  URLs and table prefix live there). `Database` (Fase 9b) is the site's
  database the dump is staged into ("" the primary); the agent resolves it
  against its record (ops/db SPEC §2) before staging anything;
- `MaxRatio` is below 0 or above `MaxRatioCeiling` (1000); 0 is the
  default limit (§3).

For a WordPress or WooCommerce site with `SQL`:

- the site's own `$table_prefix` (ops/wp `TablePrefix`, read from
  `shared/wp-config.php` as text) is read first, before anything is staged:
  it is what a rollback goes back to. One that cannot be read, or is not a
  prefix WordPress accepts (an empty one included), is `invalid`;
- the staged dump must hold at least one WordPress installation (§4.3),
  else `invalid`, before the archive is unpacked; which one, when it holds
  several, is decided once it is unpacked (§4.3).

`OldDomain` is lowercased and trimmed first. The migration also fails before
anything is touched when the site's `current` link does not name one of its
releases: the rollback target must be known before anything changes.

`Archive` and `SQL` are paths in the site's tree (`"/a/../b"` is `b`). Each
is **staged**: opened through the site's `sitefs.Root` with `O_NOFOLLOW`
(a symlink at the name is refused, a link on the way is confined beneath the
site), checked to be a non-empty regular file, and copied into a fresh
directory of the agent's root-only work directory (`WorkDir/migration-*`,
0700). From then on root reads only the copies, which the site user cannot
reach. A copy that grows past `Limits.MaxBytes` (a file the tenant keeps
appending to) is refused.

## 3. Extraction

The archive is unpacked into `<work>/tree`, a directory root alone can
reach, through a `sitefs.Root` opened on it: every entry is created with
`openat2(RESOLVE_BENEATH)` and the `*at` calls relative to a descriptor, so
nothing is ever created outside the tree, whatever a name or a link says.

**Names.** A member name is cleaned (a leading `./` dropped). The tree
itself (`.`) is ignored. An absolute name (`/etc/shadow`, `//x`) or one that
climbs out (`../x`, `a/../../x`) aborts the whole migration as `invalid`:
no legitimate archive has one.

**Entries**, in archive order:

| Type | What happens |
|---|---|
| directory | made, 0755 (the archive's mode and owner are not kept) |
| regular file | made new (`O_EXCL`), 0755 if any execute bit was set, else 0644, owned by root; its content copied, exactly `Size` bytes |
| hard link | its target (a path from the archive root) must be a regular file this extraction has already written: an **independent copy** of it is made. A hard link is never created: the build hands the tree to the site user, and a shared inode would hand over whatever it is. A target that is missing, not regular, absolute or outside the tree: skipped |
| symbolic link | kept only if its target is relative and, joined to the link's directory, stays in the tree; otherwise skipped. Created **after** every file and directory (below) |
| character or block device, FIFO, anything else | skipped: not created, counted |
| pax global header | ignored (`git archive` writes one) |

**Conflicts.** Two entries for one name, a file where a directory is needed
or a directory where a file was (`x` then `x/`, `x/` then `x`), abort with
`invalid` naming the entry. The previous code failed there too, but with the
host path of the work directory in an internal error. A link whose name is
already taken is skipped.

**Links last.** No file or directory is ever written through a link the
archive planted: while files are extracted there are no links. Then each
kept link is made, only where its directory is a real one (no link on the
way: a link inside another link's target is skipped) and the name is free.

**Links that leave.** A target can stay in the tree on paper and still leave
it once the kernel follows the links on it (`a -> d/x/../../..` with
`d/x -> ../..`). After the source directory is chosen (below), every link in
it is resolved by the kernel beneath the **source directory**
(`openat2(RESOLVE_BENEATH)`); one that leaves is removed and counted as
skipped. A dangling link, a loop or a target through a file stays: it points
at nothing. (The previous code checked against the extraction directory, so
in an archive with one top-level directory a link `top/up -> ..` survived and
became `releases/<id>/up -> ..`, the site's `releases/`.)

**Source directory.** If the tree holds exactly one entry and it is a real
directory (`tar czf site.tgz public_html`), that directory is the site;
otherwise the tree is.

**Limits** (`Limits`; the defaults are in parentheses):

- `MaxFiles` (200,000) regular files and hard-link copies;
- `MaxEntries` (400,000) entries of any kind, skipped ones included: the
  previous code counted files only, so an archive of a few megabytes could
  make millions of directories (inodes) in root's work directory;
- `MaxBytes` (20 GiB) of file content;
- **decompression bombs**: the content may expand to at most `MaxRatio`
  (200) times the compressed archive, with a floor of `MinBudget` (64 MiB) so
  that a tiny archive of small files is never refused for its ratio. Real
  site archives expand 2–10×: images and fonts are already compressed. A
  gigabyte of zeros is a megabyte of gzip; before this, it was written out
  in full into root's work directory, and then once more into the release;
- **disk**: and at most the free space of the work directory's filesystem
  less `Reserve` (1 GiB), measured before extraction;
- the tar stream itself (headers, padding, the data of skipped entries) may
  be at most the content budget plus 1 GiB: entries skipped without being
  written still cost decompression time.

The content counted is each file's `Size` (a sparse file counts its full
size: it is written out in full), checked **before** the file is written.
Passing any limit aborts as `invalid`.

**The expansion limit is the request's** when it sets one: `MaxRatio` (1 to
`MaxRatioCeiling` = 1000) replaces `Limits.MaxRatio` for that migration.
The API lets only an administrator send it (the migration form's
"expansion limit" field). The ceiling: a gigabyte of zeros is a megabyte of
gzip, about 1000 times; above that a ratio no longer tells a site from a
bomb. `MaxBytes`, the floor and the disk bound do not change.

**Too large.** Past the content budget the error says which bound was
reached, as `Limits.TooLarge(compressed, avail)`: an `*agentrpc.Error`
whose `message` is the English text and whose `key`/`args` name it in the
API's catalogue (`internal/i18n`), so the panel shows it in the user's
language:

| bound (first that applies) | key | args | English |
|---|---|---|---|
| the free disk space less `Reserve` is below the other two | `migration.tooLargeDisk` | budget, reserve | "The archive expands to more than %s, the free disk space for it (%s is kept free). Free some disk space and try again." |
| the ratio (`max(MinBudget, compressed × MaxRatio)`) is below `MaxBytes` | `migration.tooLargeRatio` | budget, ratio, 1000 | "The archive expands to more than %s, %s times its compressed size, the limit. An administrator can raise the expansion limit in the migration form, up to %s times." |
| otherwise (`MaxBytes`) | `migration.tooLargeSize` | budget | "The archive expands to more than %s, the most a migration may unpack." |

Sizes are written as `humanSize` does (`2.0 GiB`, `812.0 MiB`).
An archive with no regular file is `invalid`. A corrupt gzip or tar stream
aborts; the work directory goes with everything in it.

## 4. Activation

1. **Uploads.** If the source has a real directory `wp-content/uploads`, it
   is copied (`sitefs.CopyTree`: nothing followed, owned by the site user)
   into `.cg-migrate-<random>/uploads`, a directory of root's (0700) in the
   site root, on the site's filesystem, and removed from the source (the
   build would only replace it with the link into `shared/`).
2. **Build.** `release.Build` makes `releases/<ReleaseID>` from the source,
   with the work directory as the only allowed root: the layout links into
   `shared/`, the connector mu-plugin and the object-cache drop-in for
   WordPress, precompression and the checksum, exactly as a deploy.
3. **Table prefix** (WordPress and WooCommerce, with `SQL`). The dump's
   installations are read from its `CREATE TABLE` statements
   (`DumpTables`: the start of each line, a backquoted or bare name,
   optionally after a database name; a line of any length is read through,
   only its first 512 bytes looked at): each `p` for which `<p>options`,
   `<p>posts` and `<p>users` are all created, `p` matching
   `^[A-Za-z0-9_]{1,64}$` (what WordPress accepts). A multisite's sub-sites
   (`wp_2_…`) have no users table, so they do not count. The prefix the site
   will run on (`WordPressPrefix`): the one the archive's own
   `wp-config.php` names, when it is a regular file at the root of the
   unpacked site whose `$table_prefix` reads as one literal (ops/wp
   `ParseTablePrefix`, never run), and the dump holds that installation;
   else the dump's only installation. Several (a WP Staging copy
   `wpstg0_`, a leftover `wp2_`) and no `wp-config.php` choosing among them
   is `invalid`, before anything live changes. Before R-4.4 a dump with
   another prefix than the fresh install's (`wp_`) was imported beside its
   tables, and the site went on serving the empty install.
4. **Database: staged.** With `SQL`, the dump is imported into a scratch
   database beside the site's (ops/db `Stage`, SPEC §5: sandboxed, as an
   account scoped to the scratch database), while the site goes on serving
   from its own tables: the long part of a migration touches nothing live.
   For WordPress, the staged tables must hold the prefix's three core tables
   and a non-empty `siteurl` in `<p>options` (what WordPress reads to
   decide it is installed), else `invalid`. A failed import drops the
   scratch database.
5. **Maintenance** (WordPress and WooCommerce). A `.maintenance` file
   (`<?php $upgrading = time(); …`) is created, as the site user and
   through the site's root (`O_EXCL|O_NOFOLLOW`; a `.maintenance` already
   there is the site's and is left alone), in the live release and in the
   new one, after the files are recorded in a root-only journal under
   `WorkDir/migration-maintenance/`. wp-settings answers 503 to every request
   that loads WordPress until it is removed; `$upgrading` is `time()`
   itself, so the mode does not lapse after WordPress's ten minutes. It is
   removed after the commit (step 11), after a rollback, in the clean-up on
   any other way out, and by `Sweep` at the agent's next start when the
   agent died meanwhile: only a regular file holding exactly that text is
   removed, a missing one is fine. `wp-admin/install.php` (and the other
   `WP_INSTALLING` entry points) skip the maintenance check; what keeps the
   installer away is step 6: the tables the configured prefix names are in
   the live database at every moment, so WordPress finds itself installed
   and install.php answers "already installed".
6. **Database: swap.** In order, each one statement:
   a. `Swap`: one `RENAME TABLE` moves every dump table into the site's
      database; a live table of the same name moves, in the same statement,
      into a holding database `cgh_<16 hex>`. Live tables the dump does not
      have stay, so with a new prefix the site keeps running on its old
      tables;
   b. when the prefix differs, `$table_prefix` is set to it (ops/wp
      `SetTablePrefix`: `shared/wp-config.php` rewritten in one rename, so
      PHP reads the old file or the new one, never a half-written one);
   c. `Retire`: one `RENAME TABLE` moves the remaining live tables to the
      holding database. The site's database is now the dump and nothing
      else, as after a restore.
   Before, the database was emptied (`DROP DATABASE`) and the dump imported
   into it while the site served: for the length of the import WordPress
   found no tables, redirected to install.php, and anyone could complete
   the installer and write a backdoor into `shared/wp-config.php`.
7. **Uploads swap.** `shared/uploads` and the staged uploads are exchanged
   in one `renameat2(RENAME_EXCHANGE)`: every request sees the old uploads or
   the new ones. When the site has no `shared/uploads`, the staged one is
   moved there (`RENAME_NOREPLACE`).
8. **Promote.** `release.Promote`: `current` is swapped to the new release,
   the site's PHP is reloaded and old releases are pruned.
9. **URL rewrite.** For WordPress and WooCommerce, when `OldDomain` is given
   and differs from the site's domain (case-insensitive),
   `wp search-replace https://<old> https://<domain>` across the site's
   tables. A site moved under its own domain, the usual case, has nothing
   to rewrite, and ops/wp refuses a no-op.
10. **Commit.** The holding and scratch databases are dropped. For
   WordPress the site's state is refreshed (ops/wp SPEC §7): the import and
   the rewrite ran outside FPM, whose APCu still holds the old options.
11. **Maintenance off** (step 5).
12. **Clean up.** The previous uploads (now in the staging directory) and the
   staging directory go. The two inputs are removed from the site's tree
   (as links if they have become links): a full database dump should not
   stay there, and on a small server the archive is several percent of the
   disk. Removal is the last step: until then a failure rolls back and the
   operator can retry without uploading again.

## 5. Rollback

Any failure after the build undoes everything that has happened, in reverse:

- `current` points back at the release it named before (and the site's PHP
  is refreshed, below);
- the uploads are exchanged back (or moved back to the staging directory);
- the database, keeping at every moment the configured prefix's tables in
  the live database: the retired tables come back (`Unretire`), then
  `$table_prefix` is set back when it was changed (or a change was
  attempted), then one `RENAME TABLE` takes each dump table back to the
  scratch database and puts the table it displaced back (`Unswap`), and the
  staging databases are dropped. Nothing is ever emptied;
- the new release is removed;
- the site's state is refreshed (ops/wp `Refresh`: OPcache and APCu dropped
  for this site alone), since both code and rows have changed under FPM;
- the site leaves maintenance.

Each failure of the rollback itself is joined to the cause. A database step
that fails stops the database's undo there, so the live tables stay those
the configured prefix names, and the error names the holding database with
the previous tables (the agent's start sweep keeps a `cgh_` database, it
drops only `cgm_` scratch ones). The inputs
stay in the site's tree. Before the build, a failure leaves nothing but the
work directory, which is always removed.

## 6. Result

`Files` regular files written (copies of hard links included), `Bytes` their
total size, `Skipped` the entries not created (unsafe links, special files,
links removed because they leave the source). The API reports the skipped
count with the task.

## 8. Pulling from another host (#82)

`PullParams{Site, Host, Port, User, Password | PrivateKey, HostKey, Path,
DB, SQLPath, ReleaseID}` (internal/api SPEC §10.1). The agent connects with
`golang.org/x/crypto/ssh` (password, keyboard-interactive answering the
password, or the key, held in memory: never written to a file) and
`github.com/pkg/sftp`, with a 30 s dial timeout. `HostKey`, when given, must
be the SHA-256 fingerprint of the key the host shows (`ssh.FingerprintSHA256`),
else the call fails before authenticating; the fingerprint seen is always
in the result.

- **Walk.** `Path` is resolved once (`RealPath`, so a `current` link names
  its release) and walked with `Lstat`: directories, regular files and
  symlinks (kept as links; the import decides what to do with them, §3);
  anything else is skipped. The counts stop at `Limits.MaxEntries`/
  `MaxFiles`/`MaxBytes` (→ `invalid`, "the old site is larger than a
  migration takes").
- **PullEstimate** returns the counts, whether `wp-config.php` is in the
  folder, whether a shell answers (`DB` `dump` only: `exit 0` run in a
  session), the free space of the site's filesystem and whether the files
  and twice their size for the import fit with `Limits.Reserve` left.
- **PullMigration** checks the same space first, then writes
  `tmp/cg-pull-<release>.tar.gz` through the site's `sitefs.Root` (the site
  user's, created new: a name already there is refused), a gzip'd tar of
  the walk with names relative to the folder; with `DB` `file` it copies
  `SQLPath` to `tmp/cg-pull-<release>.sql`; with `dump` it reads
  `wp-config.php` over SFTP (at most 1 MiB), takes `DB_NAME`, `DB_USER`,
  `DB_PASSWORD` and `DB_HOST` from its `define()` lines (missing → `invalid`)
  and runs `mysqldump --defaults-extra-file=/dev/stdin --single-transaction
  --quick --no-tablespaces --default-character-set=utf8mb4 -h <host> [-P
  <port>] <name>` in a session, the `[client]` user and password written to
  its standard input, its output streamed to that `.sql` (the arguments
  single-quoted for the remote shell). A failure removes what was written.
  The result names the two paths, relative to the site's root.
- **PullCleanup** removes `tmp/cg-pull-<release>.*` of the site (a missing
  one is no error).

## 7. Tests

`extract_test.go` covers §3 against a real `sitefs.Root` (Linux, as root in
the container suite): traversal and absolute names, escaping links absolute
and relative and through chains, a link in a single top directory, hard
links as copies, devices and FIFOs, conflicts, every limit and a gzip bomb.
`migration_test.go` drives `Import` with the real `release.Ops` and fakes for
the database and WordPress: success, the inputs removed, the uploads
swapped, maintenance on during the switch and off after, and a rollback from
each step that can fail. The fake database checks at every move that the
configured prefix's options table is live. `prefix_test.go`: the dump's
prefix found (quoted, bare, multisite, after a line longer than the read
buffer), chosen by the archive's wp-config.php among two, or refused (none,
two without a choice, unusable); the site's prefix read first; a staged dump
without siteurl, a failed swap or retire rolled back; the sweep of a dead
migration's maintenance; the request's expansion limit and the too-large
errors' keys. ops/db's `staged_test.go` and `staged_integration_test.go`
(a real MariaDB) cover the renames. e2e: `e2e/e2e_test.go` (`migration
import`): a dump with prefix `xyz_` serves its marker post, wp-config.php
says `xyz_`, no `wp_` table is left; while a slowed switch runs,
wp-login.php gets 503 and no request reaches the installer; a bomb's
failure reads in the reader's language naming the limit and how to raise
it; an operator cannot move the limit; a raised limit lets a 380× archive
through.
