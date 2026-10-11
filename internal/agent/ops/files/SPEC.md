# ops/files — the file manager

Listing, reading, editing, uploading, downloading, creating folders, renaming
and deleting inside one site's tree, for the panel's file manager.

Sources: the e2e suite (`e2e/files_test.go` editor and upload through nginx,
`e2e/layout_test.go` the hard link the editor must not write through,
`e2e/agentrpc_test.go` the error a missing directory gives the browser,
`e2e/security_test.go` the console link hidden from the listing), the API
callers (`internal/api/files.go`), the UI (`ui/app/pages/sites/[id]/files.vue`,
`ui/app/composables/useUploadQueue.ts`), `docs/api.md` "Files", `sitefs/SPEC.md`.

RPC names, parameters and results are those of `internal/agentrpc/methods.go`
(`ListFiles`, `ReadFile`, `WriteFile`, `TransferFile`, `ManageFile`), types in
`internal/rpc`. The RPC server has already validated the site (its root is a
direct child of the sites root); no method takes a site lock.

## 1. Confinement

- Every operation goes through a `sitefs.Root` opened on the site's root,
  owned by the site user (the agent's `openSite`). Paths are resolved by
  `openat2(RESOLVE_BENEATH)`: a relative symlink that stays in the tree is
  followed (the layout's `current`, `wp-config.php`, `wp-content/uploads`), an
  absolute one or one that climbs out is refused. Nothing here resolves a path
  in user space and then opens it.
- A path from the API is site-relative. It is cleaned as if rooted at `/`:
  `"/a/../b"`, `"a/./b/"` and `"b"` are the same; `""`, `"/"`, `"."`, `"//"`
  name the site root itself.
- Operations that act on a name (rename, delete) act on the entry itself, a
  symlink as a symlink; only the components above it are followed.

### 1.1 The skeleton is not the file manager's

The agent runs the file manager as root, and the site's skeleton is root's
(release SPEC §1.1): root may write `.ssh/shell_keys` or delete `current`
where the site user cannot. So every method opens the site through a
**protected view** (`sitefs.Root.Protect`, sitefs SPEC "Protected views")
that refuses to create, replace, rename (from or onto) or delete:

- these entries of the site root itself (the entry, not what is inside it):
  `releases`, `current`, `.ssh`, `shared`, `logs`, `tmp`, `src` (`Managed`);
- any entry directly in a folder root owns other than the site root: all of
  `.ssh/**` (`authorized_keys`, `shell_keys`), the release folders directly
  in `releases/`, and a release's private build folder.

The check is made on the descriptor of the parent folder the operation then
uses, so a path that reaches `.ssh` through a link the tenant planted
(`shared/k -> ../.ssh`, then `shared/k/shell_keys`) is refused like the
direct one, and a link swapped in after the check changes nothing.
Everything inside `shared/`, `logs/`, `tmp/`, `src/` and a release
(`current/…`) stays editable, and new entries of the root with other names
can be made. Reading and listing are unchanged. A refusal is `invalid`:
"this is part of the site's layout, which the panel manages; it cannot be
changed in the file manager".

## 2. Errors

Each refusal is typed (`agentrpc`), so the browser gets a status and a message
without paths; the underlying error, path included, is the detail for the log.

| Cause | Code | Message |
| --- | --- | --- |
| path leaves the tree (`sitefs.ErrEscape`) | invalid | path escapes the site root |
| symlink where none is followed (`ErrSymlink`) | invalid | the path is a symlink; refusing to follow it |
| not a regular file or folder (`ErrNotRegular`) | invalid | not a regular file |
| a component is not a folder (`ENOTDIR`) | invalid | part of the path is not a folder |
| missing (`ENOENT`) | not_found | no such file or folder |
| name taken (`EEXIST`) | conflict | already exists |
| a folder where a file is wanted | invalid | path is a folder |

The old agent answered `internal` (502, "Could not …") for a missing path, an
existing folder name on mkdir, and a folder given to the editor's save.

## 3. ListFiles

Params `{site, rel_path}`; result `{rel_path, entries}`.

- Lists the folder `rel_path` (an in-tree link to a folder is followed).
- Each entry: `name`, `is_dir`, `size`, `mode` (Go's `FileMode.String()`),
  `mod_time` (RFC 3339, UTC). An entry is described without following it,
  except that a symlink whose in-tree target is a folder is `is_dir: true`
  (so `current` opens); a link leading outside, or nowhere, stays a file.
- Folders first, then by name (byte order). `entries` is `[]`, never `null`,
  for an empty folder.

## 4. ReadFile

Params `{site, rel_path}`; result `{rel_path, content, size, binary,
too_large, encoding}`.

- Opens `rel_path` read-only (in-tree link followed); a folder is `invalid`
  "path is a directory". Reads at most 2 MiB + 1 byte (`io.ReadFull`, so a
  short read never truncates a file).
- **too_large**: more than 2 MiB. **binary**: a NUL byte in the first 8 KiB,
  or the bytes are not valid UTF-8 — a character cut in two at the 2 MiB mark
  is not held against a file that goes on, but a complete file ending in half
  a character is binary. Either way `content` is empty and `encoding` is
  omitted; the UI offers a download.
- **encoding**: `utf-8`, or `utf-8-bom` when the file starts with EF BB BF.
  The mark is not part of `content`.
- `size` is the file's size (at least the bytes read).

## 5. WriteFile

Params `{site, rel_path, content}`; result `{"written": rel_path}`.

- `content` over 2 MiB → `invalid` "file too large to edit here".
- If `rel_path` exists (in-tree link followed): a folder → `invalid` "path is
  a directory"; a binary file (§4's test on its first 2 MiB) → **`conflict`**
  "this file is binary and cannot be edited as text; download it instead", and
  the file is untouched (a stale tab or a script must not destroy an image);
  a file with a byte order mark keeps it (prepended unless `content` already
  starts with one).
- The write is `sitefs.WriteFile`: an in-tree link at the name is resolved,
  and a **new file is renamed over the target** — a hard link to a file
  elsewhere is never written through (the target gets a new inode). An
  existing file keeps its owner and mode; a new one is the site user's, 0640.
- The parent folder must exist (`not_found` otherwise).

## 6. TransferFile

Params `{site, rel_path, data, upload}`; result `{name, data, size}`.

- `rel_path` naming the root → `invalid` "file path required".
- **Upload** (`upload: true`): `data` over 16 MiB → `invalid` "upload exceeds
  16 MB; use SFTP for larger files"; a folder at the name → `invalid`
  "destination is a directory"; otherwise written like §5 (atomic, owner and
  mode kept, new file 0640 for the site user; no binary check, no BOM logic).
  Result `{name: base name, size}`. The parent must exist: the UI's upload
  queue creates folders with `ManageFile mkdir` first.
- **Download**: the file (in-tree link followed) must be regular (`invalid`
  "path is not a regular file") and at most 16 MiB (`invalid` "download exceeds
  16 MB; use SFTP for larger files"). Read through a 16 MiB + 1 bound, not by
  the size `fstat` gave: a file that grows meanwhile is refused, one that
  shrinks returns what is there. Result `{name, data, size: len(data)}`.
- 16 MiB is the API's own limit (`maxPanelTransfer`); base64 in JSON keeps it
  under the agent socket's 32 MiB body limit.

## 7. ManageFile

Params `{site, operation, rel_path, dest_path}`; result one key naming what
happened: `{"created"|"renamed"|"deleted": path}`.

- `rel_path` naming the site root → `invalid` "site root cannot be modified",
  before anything is opened. An operation other than `mkdir`, `rename`,
  `delete` → `invalid` `unsupported file operation "<op>"` (the UI uses only
  these three; there is no chmod, which would let a panel user open a site's
  `wp-config.php` to the world).
- **mkdir**: one folder, the site user's, 0750; the parent must exist; a name
  that exists → `conflict`.
- **rename**: `dest_path` required (`invalid` "destination path required"),
  not the root; `renameat2(RENAME_NOREPLACE)` — an existing destination →
  `conflict` "destination already exists", never replaced. The entry moves as
  itself (a link stays a link). Moving a folder into itself → `invalid`.
- **delete**: the entry must exist (`not_found`); a file or link is unlinked,
  a folder removed recursively without ever following a link inside it.
