# version — what build this is, and which releases may replace it

## 1. The build — `Version`, `HandleFlag`

- `Version` is the release tag the binary was built from, set at link time
  (`-X github.com/cloudground-it/cloudground/internal/version.Version=<tag>`, see the Makefile);
  `dev` when it was not.
- `HandleFlag(args, name, summary) bool` is called by every binary before it does any work. When
  any argument is exactly `-v`, `--version` or `version`, it prints `<name> <Version>` and a
  newline; when it is exactly `-h`, `--help` or `help`, it prints `<name> <Version>`, a blank line,
  `summary` and a newline. The first such argument wins, and the answer is true: the caller exits.
  Any other arguments: nothing printed, false. A daemon must answer `--version` and exit rather than
  start serving (panel-agent once ignored its arguments, and `panel-agent --version` started the
  daemon and hung).

## 2. Versions — `Parse`, `Compare`, `Newer`, `ReleaseTag`, `CheckUpdate`

- `Parse("vX.Y[.Z][-suffix]")`: the three numbers; false for `dev`, empty, a bare hash or anything
  else. The suffix is ignored by `Parse`.
- `Compare(a, b)`: -1/0/1, false unless both parse. Numbers first; between equal numbers a
  `-beta.N` comes before the release and betas order by N (`v1.2.0-beta.1 < v1.2.0-beta.2 <
  v1.2.0`); any other suffix (`-N-gHASH` of a build between tags) orders as the tag it follows.
  `Newer(a, b)` is `Compare(a, b) > 0`.
- `ReleaseTag(tag)`: the channel of a tag the release workflow may publish, exactly
  `vX.Y.Z` (stable) or `vX.Y.Z-beta.N` (beta, N ≥ 1), no leading zeros, nothing else.
- `CheckUpdate(manifest, requested, installed)` refuses a manifest naming no release, a release
  other than the one requested (`""` or `latest` take any; compared with `Compare`, so a beta is not
  the release it leads to), and one not strictly newer than an installed release that parses (a
  development build is not checked here). `cgctl install --release` (internal/install, SPEC §5)
  makes the same checks in `install.CheckRelease`, allowing a reinstall of the same release.

`testdata/version.golden` pins the answers of §1 and §2 for a matrix of inputs.

## 3. Signatures — the one check

### 3.1 Format

- The manifest is `SHA256SUMS`: a `version <tag>` line, then one `<64 hex>  <name>` line per file of
  the release (`sha256sum` layout; a leading `*` on the name is dropped). A name is a plain file
  name: a path separator, `.` or `..` is refused. `ParseManifest` reads it.
- `SHA256SUMS.sig` is the base64 ed25519 signature (64 bytes) of the exact bytes of `SHA256SUMS`.
- `scripts/release-sign sign` writes both, listing every regular file of the release directory
  sorted by name. It is the only place that holds a private key.

### 3.2 Keys and rotation

- `releaseKeys` (keys.go) is the list of base64 ed25519 public keys a release may be signed with.
  It is source, so a key enters or leaves only in a reviewed commit, and it is compiled into every
  binary. It holds **at most two keys** (`TestReleaseKeys`): the current one, and the next one
  during a rotation window.
- `UpdatePublicKey` (link time, comma separated) adds keys for a test build signed with a
  throwaway key. A release build leaves it empty.
- `TrustedKeys()` returns both lists; a malformed entry is an error, never skipped.
- **Rotation procedure** (a new key, the old one retired or suspected leaked):
  1. `go run ./scripts/release-sign keygen next.key` on an offline machine; it prints the public key.
  2. Commit that public key as the second entry of `releaseKeys` and release (vN), still signed with
     the current key. Every server that installs vN trusts both keys.
  3. Put `next.key` in the `release` environment's `CLOUDGROUND_SIGNING_KEY` secret (replacing the
     old one) and destroy every other copy of it.
  4. Release vN+1, signed with the new key, with the old key removed from `releaseKeys`. A server
     updates to it because vN trusts the new key; from vN+1 on, the old key verifies nothing.
  5. The window is vN to vN+1. A server older than vN trusts only the old key, so the newest
     release (vN+1) does not verify for it and its update is refused: it must take vN first, by
     asking for it explicitly (`POST /api/panel/update {"version": "vN"}`). Keep vN+1 back until vN
     has been out long enough, and say so in vN+1's notes.
  - A **leaked** key: skip the window. Release vN with only the new key in `releaseKeys`, signed
    with the old key (servers trust only the old key, which is the leaked one: this is the one
    release an attacker could also have forged, so publish its SHA256SUMS hash out of band), then
    revoke the old key everywhere.

### 3.3 Verification — `VerifyManifest`, `VerifyRelease`

- `VerifyManifest(body, sig)` is the only function in the tree that checks a release signature.
  It refuses: a build with no trusted key or a malformed one, a signature that is not 64 bytes of
  base64, a signature no trusted key verifies, and anything `ParseManifest` refuses (including no
  parseable version). There is no bash or openssl copy of this check.
- `VerifyRelease(dir, names...)` reads `dir/SHA256SUMS` (≤ 1 MiB) and `dir/SHA256SUMS.sig`
  (≤ 4 KiB) — regular files only, a symlink is refused — verifies them with `VerifyManifest`, then
  checks each named file (every listed file when no name is given) is listed, is a regular file
  and matches its signed checksum. `cgctl release verify` is this function.
- `VerifyManifestPinned(body, sig, pin)` is `VerifyManifest` narrowed to one key the operator got
  out of band (the installer's `CLOUDGROUND_RELEASE_PUBKEY`): only `pin` may have signed. A build
  that trusts keys of its own takes `pin` only if it is one of them (`ErrKeyNotTrusted`), since the
  self-updates that follow check with those keys; a build with none takes `pin` alone; an empty
  `pin` is `VerifyManifest`. It shares the signature code of `VerifyManifest`; a build with no key
  at all fails with `ErrNoTrustedKey`.
- Callers: the self-update (`internal/agent/ops/panel`, SPEC §2) and the release check
  (`internal/api`, SPEC §7.2) through `VerifyManifest`; the installer (`cgctl install --release`,
  internal/install SPEC §5, whose bootstrap pins cgctl by the SHA-256 the release build stamps
  into it) through `VerifyManifestPinned`, on the bytes it then stages and installs; the release
  build (`make release`) and anyone by hand through `cgctl release verify` (`VerifyRelease`).

## 4. The release feed — `UpdateURL`, `Feed`

- `UpdateURL` is fixed at link time and never configurable at runtime: in a release build
  `https://github.com/cloudground-it/cloudground`. `CanUpdate()` is nil only with an origin that
  `CheckOrigin` accepts and at least one well-formed key; otherwise it says which is missing.
- `CheckOrigin(url)` (R-6.5): `https://…`, or `http://` to a loopback host (`127.0.0.0/8`, `::1`,
  `localhost`), which only a process on the same machine can answer. That is for the upgrade test,
  whose build serves its feed from the box itself (`scripts/upgrade-bundle.sh`). No user,
  password, query or fragment. The bytes are trusted only once the signed manifest verifies, so
  https is defence in depth here, not the check; `scripts/build-release.sh` refuses a release
  whose `UPDATE_URL` is not https.
- The feed is three paths under `UpdateURL`, the ones GitHub serves for a repository:
  `releases.atom` (the published releases), `releases/download/<tag>/SHA256SUMS` (and `.sig`), and
  `releases/download/<tag>/<file>`. `ReleaseBase(tag)` is the download prefix.
- `Feed.Latest(ctx, channel)` reads `releases.atom` (≤ 1 MiB), takes each entry's tag from its
  `…/releases/tag/<tag>` link (else the last segment of its id), keeps the release tags the
  channel takes, and returns the newest. None → error. The feed is unsigned: it only proposes.
- `Feed.FetchManifest(ctx, tag)` downloads that release's manifest and signature, verifies them,
  and refuses a manifest whose version line is not `tag` (a validly signed old manifest served
  under a newer name). A failed download wraps `ErrNoManifest` or `ErrNoSignature`.

### 4.1 Channels

- `stable` takes `vX.Y.Z` only; `beta` takes those and `vX.Y.Z-beta.N`. `ParseChannel` accepts
  `stable`, `beta` and `""` (stable). `CheckChannel(channel, tag)` refuses anything but a release
  tag, and a beta on stable. It runs on the verified manifest's version line too, so a feed that
  names a beta to a stable server is refused after the signature check as well as before it.
- The panel's `update_channel` setting chooses the channel (internal/api SPEC §12); a release
  workflow run marks a `-beta.N` tag as a GitHub pre-release.
