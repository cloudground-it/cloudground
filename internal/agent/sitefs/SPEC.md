# `internal/agent/sitefs` — confined file access to a site tree (specification)

The agent runs as root and works inside directories that a tenant's Unix user
owns. Anything that tenant runs (its PHP, a cron job, wp-cli plugins) can
rename, replace or symlink any entry it owns, at any moment, including while
root is in the middle of an operation. Every root-side file operation on a
site tree goes through this package, and this file is its contract. The code
is written from it; the tests check it.

## The one invariant

> Every path a `Root` resolves stays beneath the directory the `Root` was
> opened on. The kernel enforces it at the moment of each open, not a check
> made earlier.

It holds because every lookup is
`openat2(root.fd, rel, RESOLVE_BENEATH | RESOLVE_NO_MAGICLINKS)`, or a `*at`
syscall on a directory descriptor obtained that way:

- `RESOLVE_BENEATH` refuses (`EXDEV`) any resolution that would leave the
  root: an absolute path, a `..` above the root, an absolute symlink, or a
  relative symlink whose target climbs out. Relative symlinks that stay
  beneath are followed, so the site layout's own links work.
- `RESOLVE_NO_MAGICLINKS` refuses `/proc/<pid>/fd/*`, `/proc/self/root` and
  the like, which would jump anywhere regardless of the textual path.
- A component swapped for a symlink between two operations is resolved again
  by the next one, under the same rules. There is no check-then-use window.

**Fail closed.** No `openat2` (a pre-5.6 kernel, or a seccomp filter returning
`ENOSYS`/`EPERM`) means no confined open: `Open` returns `ErrUnsupported`.
There is no fallback to `open(2)`.

`openat2` may return `EAGAIN` when a concurrent rename races a `..` lookup;
the package retries those (bounded) and `EINTR`.

## The site layout this relies on

Symlinks inside a site are **relative**, so they resolve beneath the root:

```
<root>/current                                  -> releases/<id>
<root>/releases/<id>/wp-config.php              -> ../../shared/wp-config.php
<root>/releases/<id>/wp-content/uploads         -> ../../../shared/uploads
<root>/releases/<id>/wp-content/object-cache.php -> ../../../shared/object-cache.php
```

A copy of a release into another site (a staging clone) therefore points at
the copy's own `shared/` with no rewriting. An absolute link (the layout
before R-2.3, or one a tenant planted) is never followed by a `Root`.

## Types

```go
type Owner struct{ UID, GID int }

func Open(path string, o Owner) (*Root, error)
func LookupOwner(systemUser string) (Owner, error)

func (r *Root) Close() error
func (r *Root) Path() string           // informational only; never used for access
func (r *Root) Owner() Owner
func (r *Root) WithOwner(o Owner) *Root // same descriptor, different owner for what it creates
func (r *Root) Sub(rel string) (*Root, error)
```

- `Open` takes an absolute path that was validated by the caller (for a site,
  `site.Validate` confines it to `SitesRoot/<domain>`). It opens it once,
  `O_PATH|O_DIRECTORY|O_NOFOLLOW` with `RESOLVE_NO_SYMLINKS`: the root itself
  may not be, or be reached through, a symlink. One `Root` per call; the caller
  closes it.
- `Owner` is who everything the `Root` creates belongs to (files, directories,
  symlinks), set through the new entry's descriptor. For a site it is the site
  user. Root-owned entries inside a site tree (the root directory itself,
  `releases/`, `.ssh/`) use `WithOwner(Owner{0, siteGID})`.
- `LookupOwner` resolves a site user. An unknown user is an **error**: the
  agent never silently leaves root-owned files in a site tree. Tests inject
  their own owner instead.
- `Sub` opens a directory beneath as a new `Root` (closed separately), so an
  operation can be confined to, say, one release instead of the whole site.
- `WithOwner` shares the descriptor: close only the original.

## Paths

`rel` is a slash-separated path relative to the root. It is cleaned; an
absolute path or one whose cleaned form starts with `..` is refused with
`ErrEscape` before any syscall (the kernel would refuse it anyway). `""` and
`"."` mean the root itself; operations on a final component (remove, rename,
symlink…) refuse the root.

Unless stated otherwise:

- **intermediate components** follow in-tree symlinks (kernel-confined);
- the **final component** is *not* followed by anything that writes, removes,
  renames, re-owns or changes mode: the operation acts on the link itself, or
  refuses it with `ErrSymlink`.

## Operations

| Operation | Semantics |
|---|---|
| `OpenFile(rel, flags, perm)` | `openat2` beneath. A write, create or truncate adds `O_NOFOLLOW`. Always opened `O_NONBLOCK` and then checked: only a regular file or a directory is returned (a FIFO or socket a tenant planted cannot hang root). |
| `Open(rel)` | `OpenFile(rel, O_RDONLY, 0)`: follows an in-tree link at the final name. |
| `Create(rel, perm)` | New file only (`O_CREAT|O_EXCL|O_NOFOLLOW`), owned by `Owner`, mode `perm`. |
| `ReadFile(rel, limit)` | Final name **not** followed; must be a regular file; at most `limit` bytes (`ErrTooLarge` past it). |
| `Replace(rel, perm, fill)` | Atomic: a temporary file created `O_EXCL|O_NOFOLLOW` in the target's directory (through that directory's descriptor), filled, `fchown` to `Owner`, `fchmod perm`, `fsync`, `renameat` over the name, `fsync` of the directory. A symlink at the name is replaced, never followed; an existing hard link is not truncated (a new inode takes the name). |
| `ReplaceUnsynced(rel, perm, fill)` | `Replace` without the two `fsync`s, for derived files rebuilt when lost (the precompressed `.gz` of a release, thousands per deploy). |
| `WriteFile(rel, data, perm)` | The file manager's write. Resolves an in-tree symlink at the final name first (see `Resolve`), then `Replace`s the target. An existing regular file keeps its owner and mode; a new one gets `Owner` and `perm`. |
| `Resolve(rel)` | Follows every symlink on `rel`, component by component, the way the kernel would but lexically (at most 40 links); an absolute target or a `..` above the root is `ErrEscape`; a missing tail is kept. The result is still opened beneath, so a link changed after `Resolve` can only lead to another in-tree path. |
| `Mkdir(rel, perm)` / `MkdirAll(rel, perm)` | `mkdirat` in the parent's descriptor, then the new directory is opened `O_NOFOLLOW` and given `Owner` and `perm` through that descriptor. `MkdirAll` creates only missing components; an existing non-directory is `ENOTDIR`. |
| `Remove(rel)` | `unlinkat` (a directory with `AT_REMOVEDIR`). A symlink is removed as a link. |
| `RemoveAll(rel)` | Depth-first through descriptors opened `O_NOFOLLOW|O_DIRECTORY`; a symlink anywhere is unlinked, never descended. Missing is not an error. |
| `Rename(old, new, flags)` | `renameat2` between the two parents' descriptors. `NoReplace` and `Exchange` map to `RENAME_NOREPLACE` and `RENAME_EXCHANGE`. |
| `Lstat(rel)` / `Stat(rel)` | `fstatat(parent, name, AT_SYMLINK_NOFOLLOW)`; `Stat` opens beneath (following in-tree links) and `fstat`s. |
| `ReadDir(rel)` | Lists through the directory's descriptor (`getdents`) and `fstatat`s each name `AT_SYMLINK_NOFOLLOW` relative to it. Never builds a path string to stat, never uses `/proc/self/fd/N`. Sorted by name. |
| `Readlink(rel)` | `readlinkat` on the parent's descriptor. |
| `Symlink(target, rel)` | `target` must be relative and, joined to `rel`'s directory, stay beneath (`ErrEscape` otherwise). Created at a temporary name, `fchownat(AT_SYMLINK_NOFOLLOW)` to `Owner`, then renamed over `rel`, replacing a link or file atomically. |
| `Chown(rel, uid, gid)` | Opens the entry `O_PATH|O_NOFOLLOW` beneath, `fstat`s that descriptor and `fchownat(fd, "", AT_EMPTY_PATH)`s it, so the check and the change are on one inode (a symlink is re-owned as a link). A **foreign link** (below) is `ErrForeignLink`. Never `chown(2)` on a path, never the coreutils tool. |
| `Chmod(rel, mode)` | Opens the entry `O_NOFOLLOW|O_NONBLOCK` and `fchmod`s the descriptor; only regular files and directories; a symlink is `ErrSymlink`, a foreign link `ErrForeignLink`. |
| `SubStrict(rel)` | `Sub` with `RESOLVE_NO_SYMLINKS`: no component of `rel` may be a symlink (`ErrSymlink`), so the directory checked to be real is the one then written in. The new Root's `Symlink` checks targets against the tree it was opened from (a target may climb out of `rel` while it stays beneath that tree). |
| `Walk(rel, fn)` | Pre-order, names sorted, never following a symlink (a link is reported as a link). `fn` may return `fs.SkipDir` / `fs.SkipAll`. The start must be a real directory or file (`ErrSymlink` otherwise); its parents may be in-tree links. |
| `ChownTree(rel)` | Gives `rel` and everything beneath to `Owner`, each entry through an `O_PATH|O_NOFOLLOW` descriptor that is `fstat`ed and then changed, never following a link (links are re-owned as links). A foreign link is **left alone**. |
| `ChmodTree(rel, clear)` | Clears `clear` on every directory and regular file beneath, through descriptors; a foreign link is left alone. |

A **foreign link** is a non-directory with a link count above 1 that belongs to someone other than `Owner`: a hard link to a file that may live outside this tree, where changing its owner or mode would change that file. The check is on the `fstat` of the descriptor the change is made through, never on an earlier listing. A tenant cannot make one today (its tree is a separate mount in the sandbox, SFTP is chrooted, every import copies hard links as content); this keeps it so if that ever changes.
| `CopyTree(src, srcRel, dst, dstRel)` | See below. |

### `CopyTree`

Copies `srcRel` in `src` to `dstRel` in `dst` (two `Root`s, possibly the same
directory opened twice). It replaces `cp -a` (uutils' on Ubuntu 26.04) and the
`/proc/<pid>/fd/<n>` paths it was fed.

- The start is not followed: `srcRel` must be a real directory (or regular
  file). Callers that mean "what `current` points at" resolve it first.
- `dstRel`'s parent must exist and `dstRel` itself must not (`EEXIST`): a
  copy never merges into, or writes through, something already there.
- Entries are read and written relative to descriptors, never followed:
  - **directory**: `mkdirat` 0700, filled, then given `Owner`, the source's
    mode and mtime — post-order, so a read-only source directory can still be
    filled;
  - **regular file**: opened `O_NOFOLLOW|O_NONBLOCK` and re-checked by
    `fstat`; content copied into a new `O_EXCL|O_NOFOLLOW` file (`io.Copy`,
    which uses `copy_file_range`); `Owner`, mode, mtime. A file with link count
    > 1 is copied as content like any other. **A hard link is never
    created**, so a link a tenant made to a file outside its tree becomes an
    independent copy owned by the destination's owner, and the original is
    neither linked nor re-owned;
  - **symlink**: its target text is copied verbatim (`readlinkat` /
    `symlinkat`), owned by `Owner`. Relative layout links therefore point into
    the copy's own tree;
  - anything else (FIFO, socket, device): skipped.
- Modes are kept to the permission bits (`& 0777`): set-uid, set-gid and
  sticky bits are dropped. Ownership is always `dst.Owner()`, never the
  source's.

## Errors

| Error | When |
|---|---|
| `ErrEscape` | the path or a link in it leaves the root (`EXDEV` from the kernel, or the lexical check) |
| `ErrSymlink` | a symlink where the operation refuses one (`ELOOP` from `O_NOFOLLOW` / `RESOLVE_NO_MAGICLINKS`) |
| `ErrUnsupported` | `openat2` is unavailable |
| `ErrTooLarge` | `ReadFile` past its limit |
| `ErrNotRegular` | a read expected a regular file |
| `ErrProtected` | a protected view refused to change an entry (below) |

## Protected views

```go
func (r *Root) Protect(names ...string) (*Root, error)
```

A view of `r` (same descriptor, same owner; `WithOwner` and `Sub` of it stay
protected) for code that writes as root on behalf of a panel user, the file
manager. Every operation that changes a final name (`Create`, `OpenFile`
with a write flag, `Replace`, `WriteFile`, `Mkdir`, `MkdirAll`, `Symlink`,
`Remove`, `RemoveAll`, both ends of `Rename`, `Chown`, the destination of
`CopyTree`) opens the parent directory as usual, `fstat`s **that
descriptor**, and refuses with `ErrProtected` when:

- the parent is the directory `Protect` was called on (same device and
  inode) and the name is one of `names`; or
- the parent is owned by root and is not that directory, and the `Root`
  `Protect` was called on creates for a user other than root (a site's
  `.ssh`, `releases/`, a build directory are root's; everything the tenant
  may change is the tenant's).

The operation then goes on with the same descriptor, so no link swapped in
after the check can move it elsewhere. `Chmod`, `ChownTree`, `ChmodTree` and
a `Chown` of the root itself are refused outright on a protected view.
Reads are not affected.

All are wrapped in `*fs.PathError` with the **site-relative** path, never the
host path. `ENOENT`, `EEXIST`, `ENOTDIR` keep their `errno`, so
`errors.Is(err, fs.ErrNotExist)` and friends work.

## Out of scope

Root-owned paths outside site trees (`/etc/nginx`, the agent's work directory,
`/srv/sites` itself) are not a `Root`'s job; the agent keeps its own safe code
for them. A `Root` opened on a root-only work directory (migration extraction,
restic restore scratch) is fine and gives the same guarantees.

## Linux only

`openat2` and `renameat2` are Linux syscalls. The tests run in the `golang`
container (`make docker-test`), as root, so ownership is exercised for real.
