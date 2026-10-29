# Releasing CloudGround

A release is a pushed tag, built and published by `.github/workflows/release.yml` to the GitHub
Releases of `cloudground-it/cloudground`. The rules a release must pass, and how servers check
it, are in `internal/version/SPEC.md`.


## Tags and channels

| Tag | Channel | GitHub Release |
| --- | --- | --- |
| `vX.Y.Z` | stable | a release, marked latest |
| `vX.Y.Z-beta.N` (N ≥ 1) | beta | a pre-release, never latest |

Any other `v*` tag fails the build job. A server follows the channel chosen in Settings → Panel
updates (`update_channel`); stable servers are never offered a beta, beta servers get both.

## One-time setup (repository settings)

1. **Protected tags.** Settings → Rules → Rulesets → New ruleset → *New tag ruleset*: name
   `release tags`, enforcement *Active*, target *Include by pattern* `v*`, rules *Restrict
   creations*, *Restrict updates*, *Restrict deletions* and *Block force pushes*; bypass list: the
   owner only. Nobody else can push, move or delete a release tag.
2. **The `release` environment.** Settings → Environments → New environment `release`:
   - *Required reviewers*: the owner; *Prevent self-review* off (the owner tags and approves);
   - *Deployment branches and tags*: *Selected branches and tags*, add the tag rule `v*`;
   - *Environment secrets*: `CLOUDGROUND_SIGNING_KEY`, the private key file's single line
     (below). It is a secret of this environment only, never of the repository.
3. **Actions.** Settings → Actions → General: *Workflow permissions* read-only (the workflows ask
   for what they need per job); *Allow GitHub Actions to create and approve pull requests* off.

## The signing key

On an offline machine:

```sh
go run ./scripts/release-sign keygen release.key   # prints the public key
```

- The printed public key goes into `releaseKeys` in `internal/version/keys.go`, in a reviewed
  commit. Binaries built from then on accept releases signed by it.
- The content of `release.key` goes into the `release` environment's `CLOUDGROUND_SIGNING_KEY`;
  keep one offline copy (encrypted, off any server) and delete the rest.
- `go run ./scripts/release-sign pubkey release.key` prints the public half again.
- Rotation, and what to do if the key leaks: `internal/version/SPEC.md` §3.2.

## Cutting a release

1. On `main`, with CI green and `ui/dist` current: `git tag -s vX.Y.Z -m vX.Y.Z && git push origin
   vX.Y.Z`.
2. The **build** job builds the release twice (`make release-build`) and fails if the two builds
   differ. Its artifact is the unsigned release.
3. The **sign** job waits for the owner's approval (Actions → the run → *Review deployments*).
   Approve only a run for a tag you pushed. It runs `make release-sign`, which fails unless the
   signature verifies with the keys the binaries carry.
4. The **publish** job verifies again (`make release-verify`), attests the provenance of every file
   (`actions/attest-build-provenance`) and creates the GitHub Release, marked latest only when its
   tag is the newest stable one.
5. The **pages** job (`.github/workflows/pages.yml`), for that newest stable release only,
   publishes its `install.sh` on GitHub Pages (`get.cloudground.it`, below) and checks that the
   domain serves it. To publish the site again without a release, for the release marked latest:
   `gh workflow run pages.yml -f tag=vX.Y.Z`.

Locally, the same release from the same commit: `make docker-release SIGNING_KEY=<file>` (pinned
containers), and `make docker-release-repro` checks two builds are identical.

A release holds: `panel-api`, `panel-agent`, `cgctl`, `cg-site-shell`, the three systemd units (also compiled into
the binaries, `internal/units`), `install.sh` (the installer bootstrap, with this release's tag and
its cgctl's SHA-256 stamped in), `cloudground-<tag>-linux-amd64.tar.gz`, `SHA256SUMS`
and `SHA256SUMS.sig`. Check one by hand:

```sh
cgctl release verify <dir>                                     # the signature, with cgctl's keys
gh attestation verify cgctl --repo cloudground-it/cloudground  # the provenance
```

## Where the installer is served

Two addresses serve the same file, the newest stable release's `install.sh`: `installer/install.sh`
with the release's tag stamped over `@CGCTL_VERSION@` and its cgctl's SHA-256 over `@CGCTL_SHA256@`
by `scripts/build-release.sh` (the build fails if either placeholder survives, and the publish job
checks it again), listed in the signed `SHA256SUMS` like every other file. The unstamped
`installer/install.sh` is never published: it names no release, so it only runs a local build or
hops to `releases/latest/download/install.sh` (internal/install/SPEC.md §6).

| Address | What serves it |
| --- | --- |
| `https://github.com/cloudground-it/cloudground/releases/latest/download/install.sh` | the `install.sh` asset of the release GitHub marks *latest*. The publish job marks a release latest only when it is the newest stable tag by version (a patch to an older line is published, not as latest); a beta never. **The documented command until the domain exists.** |
| `https://github.com/cloudground-it/cloudground/releases/download/vX.Y.Z/install.sh` | that release's copy: the pinned install |
| `https://get.cloudground.it/` and `/install.sh` | GitHub Pages of this repository, deployed by `.github/workflows/pages.yml` for the release marked latest only, from the published release once its signature verifies: `index.html` and `install.sh` are both the stamped script (Pages serves `/` as `index.html`, as `text/html`, which `curl -fsSL … \| bash` does not look at), and `CNAME` holds `get.cloudground.it` |

The same deployment carries the panel's demo under `https://get.cloudground.it/demo/` (`docs/demo.md`): the
`pages` workflow builds it from the tag (`npm run build:demo`), so the demo is the newest stable release's interface
and needs no setting or secret of its own.

The bootstrap itself downloads the binaries from the GitHub release, never from Pages. What the
script host is trusted with, and how an operator avoids trusting it, is internal/install/SPEC.md
§6.1.

### The owner's one-time setup for get.cloudground.it

Once the domain `cloudground.it` is registered:

1. **DNS** at the domain's provider:
   - `get` — `CNAME` — `cloudground-it.github.io.` (the organisation's Pages host; no `A` records
     for a subdomain);
   - recommended: `CAA` on `cloudground.it`: `0 issue "letsencrypt.org"` (Pages' certificates come
     from Let's Encrypt; no other authority may issue for the domain), and DNSSEC if the registrar
     offers it.
2. **Verify the domain for the organisation** (protects it from being claimed by another Pages
   site): GitHub → organisation `cloudground-it` → Settings → Pages → *Add a domain* →
   `cloudground.it`, then the `TXT` record it shows (`_github-pages-challenge-cloudground-it`) at the
   DNS provider, then *Verify*.
3. **Pages for the repository**: Settings → Pages → *Build and deployment* → Source: **GitHub
   Actions**.
4. **The `github-pages` environment** (created by step 3): Settings → Environments →
   `github-pages` → *Deployment branches and tags*: add the tag rule `v*` (the release workflow
   runs on a tag, and the environment allows only the default branch otherwise).
5. **The custom domain**: Settings → Pages → *Custom domain* → `get.cloudground.it` → *Save*. (A
   site deployed by a workflow takes its domain from this setting; the `CNAME` file the `pages` job
   writes is ignored by GitHub and only records it.) Wait for the DNS check to pass, then tick
   **Enforce HTTPS** (available once the certificate is issued, up to an hour).
6. **Run a stable release** (or re-run the `pages` job of the newest one) to deploy the site.
7. **Check it**: `curl -fsSI https://get.cloudground.it/` answers 200;
   `curl -fsSL https://get.cloudground.it/ | sha256sum` equals the `install.sh` line of the newest
   release's `SHA256SUMS`; `curl -sI http://get.cloudground.it/` redirects to https.

The repository is public, so the release assets, the feed and `get.cloudground.it` need no
credentials.

### What the docs say to run

Until the domain answers: `curl -fsSL https://github.com/cloudground-it/cloudground/releases/latest/download/install.sh | sudo bash`.
Afterwards: `curl -fsSL https://get.cloudground.it | sudo bash`. Both, and the pinned and verified
forms, are in `docs/install-update-uninstall.md`.
