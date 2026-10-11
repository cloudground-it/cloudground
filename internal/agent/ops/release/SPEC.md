# `internal/agent/ops/release` — a site's releases (specification)

A site's code lives in immutable releases; the relative link `current`
names the live one, and nginx and PHP serve through it. This package builds
releases, swaps `current`, rolls back and prunes. Provisioning, staging and
migration use its layout helpers (`Live`, `ParseLink`, `LockSkeleton`,
`Precompress`, `Build`, `Promote`).

## 1. Layout

```
<root>/                         root:<site group> 0750 (OpenSSH chroot)
├── current -> releases/<id>    root:<site group>, relative
├── releases/                   root:<site group> 0750
│   ├── initial/                provisioning's release
│   ├── <id>/                   the site user's, 0750 at the top
│   │   ├── wp-config.php       -> ../../shared/wp-config.php          (when shared/ has one)
│   │   ├── wp-content/uploads  -> ../../../shared/uploads             (when the tree has wp-content)
│   │   └── wp-content/object-cache.php -> ../../../shared/object-cache.php  (WordPress, cache on)
│   └── .build-<id>/            root 0700, only while <id> is being built
└── shared/
```

`releases/` belongs to root: a site user owning it could swap a release for
a symlink and point `current`, and nginx's document root, anywhere.
`LockSkeleton` re-asserts the root, `releases/` and `.ssh` ownership before
every build, promote and rollback.

### 1.1 The skeleton

`LockSkeleton` (also run by provisioning, staging and a restore) makes:

- the root and `releases/`: root:<site group> 0750 (`releases/` created
  when missing; a non-directory there is an error);
- `.ssh`, when it is a real directory (`LockSSH`): root:<site group> 0750,
  and **every** entry in it root:<site group> 0640 when it is a regular file
  with one link (`authorized_keys`, `shell_keys`, anything else). Any other
  entry (a symlink, a directory, a FIFO, a regular file with more than one
  link, which is a hard link to a file elsewhere) is removed: sshd reads only
  the agent's regular files there. The modes are the ones the sftp ops write
  (its SPEC §4). A `.ssh` that is not a directory is left alone (the sftp ops
  refuse it with a message).

A restore copies a snapshot's tree as the site user's (`sitefs.CopyTree`), so
before R-10.5 its `.ssh/shell_keys` stayed the site user's: a tenant could then
add its own shell key.

**Release IDs** (`internal/site`): a new release is a **ULID** (R-4.3),
made by the API (`site.NewReleaseID`): 26 characters of Crockford base32
in upper case, the creation time in milliseconds then 80 random bits,
increasing within the API process. The two forms releases had before stay
valid names, so a site deployed before keeps its releases:
`YYYYMMDD-HHMMSS.mmm-xxxxxx` (the UTC time to the millisecond and 24 random
bits) and `YYYYMMDD-HHMMSS` (with it, two deploys started in the same
second got the same ID, and the second failed). `initial` is
provisioning's. Releases order by age: `initial` first, then by the time
their name carries, whatever its form (a second-resolution name before the
millisecond ones of its second, an older-form name before a ULID of the
same millisecond); never by their text, which would put every ULID before
every older name and send a rollback forward.
Anything else in `releases/` is not a release: never listed, promoted or
pruned.

## 2. Methods

| Method | Params → Result | Table row |
|---|---|---|
| `DeployRelease` | `DeployParams{Site, SourceDir, ReleaseID, SourceSite?, Overlay?}` → `DeployResult{ReleaseID, Path, Checksum, Managed}` | Sites, `LockLiveSite`, 45 min |
| `PromoteRelease` | `ReleaseParams{Site, ReleaseID}` → `{"current": id}` | Sites, `LockLiveSite`, 10 min |
| `RollbackRelease` | `ReleaseParams{Site, ReleaseID?}` → `{"current": id}` | Sites, `LockLiveSite`, 10 min |
| `CurrentRelease` | `CurrentReleaseParams{Site}` → `CurrentReleaseResult{ReleaseID}` | Sites, read-only, 1 min |
| `RemoveReleases` | `RemoveReleasesParams{Site, ReleaseIDs}` → `RemoveReleasesResult{Removed}` | Sites, `LockLiveSite`, 10 min |

Migration calls `Build` and `Promote` directly, holding the lock itself.

## 3. Deploy

1. **ID**: a valid release ID, not `initial`; an existing `releases/<id>` is
   a `conflict`.
2. **Source confinement (S-C1)**: `SourceDir` is absolute and lies, once
   cleaned, in the site's own root or, with `SourceSite`, in the root of
   the site's own staging copy (`SourceSite.StagingOf == Site.ID`); the
   in-tree links on it are followed beneath that root (a deploy from
   `<root>/current` copies the live release). Anything else is `invalid`:
   the copy runs as root, so a source elsewhere would be readable back
   through the file manager. The site root, `releases/` and a build
   directory are refused as sources (they contain the release being built).
   Migration passes its own scratch directory as the only root.
3. **Build**: `releases/.build-<id>` is made root's, mode 0700, and the
   tree is copied into `releases/.build-<id>/<id>` with `sitefs.CopyTree`
   (no link followed, content copied, the site user's from the start). The
   site user cannot reach anything under the 0700 directory, whoever owns
   the files, so root works there undisturbed; nothing needs re-owning
   afterwards. Leftover build directories (an interrupted deploy) are
   removed first: the caller holds the site's lock, so none is in use.
4. **Layout**: the links of §1 are (re)made, whatever the source had:
   configuration always comes from this site's `shared/`, never from the
   deployed tree, so a release cloned from staging does not carry staging
   credentials. WordPress and WooCommerce get the connector mu-plugin
   (`wp.InstallConnector`) and the object-cache drop-in link agreeing with
   `Config.ObjectCache` (`wp.LinkDropin`); the connector's path and hash
   are returned in `Managed` so drift is measured against it.
5. **Overlay** (selective push): each path, a plain relative one, not
   `wp-config.php` and not under `wp-content/uploads`, is replaced by the
   same path of staging's live release. Both sides are confined to their
   release: a link that leads out is refused.
6. **Precompress and hash** (§4, §5), and group and other write permission
   are taken off every file and directory.
7. **Publish**: the release's top directory becomes 0750 and is renamed to
   `releases/<id>` (`RENAME_NOREPLACE`); the build directory goes. A
   failure at any step leaves no release: only the build directory, which
   the deferred cleanup (or the next deploy) removes.

## 4. Precompression

For nginx's `gzip_static`: every `.css .js .mjs .svg .json .xml .html .txt
.map` (extension case-insensitive) of 1 KiB to 10 MiB gets a `.gz` next to
it, gzip level 9, mode 0644, written with `ReplaceUnsynced` (temporary
`O_EXCL` file, rename; no fsync: a lost `.gz` costs only on-the-fly
compression). Kept only when smaller than the source.

- An existing regular `.gz` at least as new as its source is kept; an older
  one is rebuilt, or **removed** when the source no longer compresses (it
  used to stay, and gzip_static served the old asset to every browser).
- A `.gz` name taken by a directory or a link is left alone.
- Files are compressed on `Workers` goroutines (default one per CPU) after
  a single walk; each file is read once for both its digest and its gzip.
- `Precompress` does the same for provisioning's fresh tree, which is the
  site user's and reachable: every open is `O_NOFOLLOW` through `sitefs`.

## 5. Checksum

SHA-256 over the release's regular files and symlinks, sorted by path: for
each, `<len>:<path>\0` then `F` and the SHA-256 of the file's content, or
`L<len>:<target>` for a link. The `.gz` files written by the deploy are
included. Per-file digests make it independent of how many workers ran.
(It used to hash the concatenated contents of regular files only; the value
is recorded with the deployment and never compared, so the change of
definition only affects new releases.)

## 6. Promote and rollback

- **Promote**: the ID must be a release name (§1) and `releases/<id>` a real
  directory (`Lstat`; a symlink is `not_found`). `current` is replaced by a
  new link renamed over it (`sitefs.Symlink`: temporary name, `renameat`),
  so every request sees one release or the other. Then the site's PHP-FPM
  master is reloaded (sites with PHP), so OPcache serves the new code, and
  old releases are pruned (§7; a pruning failure is logged, not returned).
- **Rollback** without an ID goes to the newest release **older** than the
  live one. It used to take the newest release that was not live: a release
  built but not promoted went live untested, and a second rollback undid
  the first instead of going further back. Nothing older is a `conflict`.
  With an ID it is a promote.

**CurrentRelease** reads `current` through sitefs (`Live`: the link must
name `releases/<id>`, a real directory) and returns the id, or `""` when
there is no link or it leads nowhere a release can be. It changes nothing.
The panel calls it before every promotion (the release a rollback goes back
to) and to align its records with what the site serves. (The API always
names the target of a rollback; the rollback without one stays for
compatibility.)

**RemoveReleases** removes each named release (a release ID, never
`initial`) that exists as a real directory and is not the live one; others
are skipped. The panel names the releases of builds it recorded as failed,
which a restart may have left published.

## 7. Pruning

After each promote: build directories are removed, and of the releases
older than the live one the newest `Keep` (default 5) stay. The live
release and every newer one (built, or rolled back from) are kept.
Deployment records of pruned releases remain in the panel; promoting one is
`not_found`.

## 8. Tests

`release_test.go` and `precompress_test.go` (run as root in the container
suite: ownership is asserted against a distinct site uid). `BenchmarkDeploy`
builds a release from a 15,000-file WordPress + WooCommerce shaped tree
(`writeFixture`). e2e: `e2e/features_test.go` (`releases`).
