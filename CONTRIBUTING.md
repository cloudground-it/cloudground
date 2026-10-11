# Contributing

CloudGround's agent runs as root on the servers it manages, so a bug here lands on someone's
machine. A change therefore has to show that it works on a real server, not only that it builds.
This page says what that means in practice.

## Use a server you can throw away

**Do not install CloudGround on a machine you care about while developing.** The installer takes
over nginx, PHP-FPM, MariaDB and systemd and creates system users. A small cloud VM, or a
container for what does not need systemd, is the place for it.

## Setting up

With Go 1.26 and Node 22:

```sh
git clone https://github.com/cloudground-it/cloudground.git
cd cloudground
make build    # the UI, then panel-api, panel-agent and cgctl into bin/
make test     # go vet, then go test ./...
```

With Docker alone (on a Mac, say), the same work runs in containers: `make docker-test`,
`make docker-dist`, `make docker-e2e-bin`. [docs/build-and-test.md](./docs/build-and-test.md)
lists every target and every generated file.

| Program | Runs as | Does |
| --- | --- | --- |
| `panel-api` | the unprivileged `cloudground` user | the HTTP API and the web interface |
| `panel-agent` | root | every privileged action, asked for over a Unix socket |
| `cgctl` | you | installs the panel and talks to its API |

How they fit: [docs/architecture.md](./docs/architecture.md).

### The web interface

The interface is a Nuxt 4 single-page app built with Nuxt UI, in `ui/app/`, with its text in
`ui/i18n/`. The build in `ui/dist/` is committed, because `go:embed` puts it in the binary and the
Go build must not need Node. **After changing `ui/app/` or `ui/i18n/`, run `make ui` or
`make docker-dist` and commit `ui/dist/`**; CI rejects a stale copy.

The panel sends a strict Content Security Policy: scripts from its own origin and the hashes of the
inline scripts in the built `index.html`, nothing else, no `unsafe-inline`, no `unsafe-eval`. Do not
add inline scripts or outside origins.

Each string a user can see goes through i18n in English, Italian, French, Spanish and German, with
the terms of [ui/i18n/GLOSSARY.md](./ui/i18n/GLOSSARY.md). `npm test` in `ui/` fails on a missing
key or a mismatched placeholder. The English text is British English.

## Check it on a server

Most bugs in this project's history looked right in review and failed on a real machine: security
headers that nginx dropped without a word, an HTTP/3 probe that read standard output where nginx
writes to standard error, a WooCommerce site type without WooCommerce, a kernel TLS setting that
cost 28 % of cached throughput. Look at the actual response headers, protocol, listening sockets
and system calls. A configuration test that passes is not proof.

### The end-to-end suite

`e2e/` runs on a real server. It creates every kind of site, **requests what each one serves**,
uses each feature, deletes the site and checks nothing is left. A fix or a new behaviour comes with
a check that fails on the build before it and passes on yours. The suite has sections: add a new
one to the `sections` table in `e2e/e2e_test.go`, to `e2e/SECTIONS.md` and to `e2e/sections.map`,
then run `make docker-e2e-index`. [docs/e2e.md](./docs/e2e.md) shows how to run only the sections
a change reaches.

A check must test the product, not what the test arranged. One once installed WooCommerce itself,
then found it installed, and hid a real bug for weeks.

### Performance

Speed is what CloudGround claims, and `bench/` keeps the claim honest. A change to caching, request
handling or tuning defaults comes with numbers from before and after in the pull request.

Defaults change one setting at a time, A/B on the same server, and a gain that does not repeat is
not kept. `internal/install/SPEC.md` §4 lists what was tried and dropped, with the numbers: extend it
instead of bringing a setting back. Two servers of the same plan have measured 2.5 times apart, so
check the CPUs match before blaming the code.

## Code

- `gofmt`, `go vet` and `go test -race` (all three in `make docker-test`). A new dependency needs a
  reason: state is SQLite, with no broker and no ORM, and it stays small.
- **The specification comes first.** A package with a contract has a `SPEC.md`; the spec changes in
  the same commit as the behaviour ([docs/spec-convention.md](./docs/spec-convention.md)).
- **Comments give reasons.** Say why ("a reload keeps APCu, so a restore restarts the master"), not
  what the next line does.
- **The agent starts programs from argument lists, never shell strings,** so a domain name cannot
  become a command ([docs/agent-api-boundary.md](./docs/agent-api-boundary.md)).
- **Site trees go through `internal/agent/sitefs`.** No `chown`, `chmod` or `cp` on a path inside a
  site: Ubuntu 26.04's coreutils follow symlinks GNU's would not, and the site owns every entry
  there.

## Pull requests

Fork, branch from `main`, and name the branch after what it does (`fix/cache-purge-race`,
`docs/backup-restore`). [.github/REVIEW_GUIDELINES.md](./.github/REVIEW_GUIDELINES.md) is what a
review checks; reading it first avoids surprises.

1. One concern per pull request.
2. `make docker-test` (or `make test` and `make vet`) passes.
3. Say **why**: the failure the change fixes and how you saw it.
4. Say how you checked it, for example "e2e on Ubuntu 24.04, the affected sections green, the new
   check red on the previous build".
5. Update what describes the behaviour: the `SPEC.md`, `docs/` here, and the user documentation in
   `cloudground-docs`.

A small fix needs no warning. Something structural (a new site type, a new agent method, a change
to the security model) starts with an issue, so the approach is agreed before the work.

## Bugs

An issue with `cgctl --version`, the Ubuntu release, what you expected and what happened. The
panel's logs (`journalctl -u cloudground-api -u cloudground-agent`) and the site's own under
`/var/log/cloudground/<domain>/` usually explain it.

**Security problems go to the [security policy](./SECURITY.md), never to the issue tracker.**

## What to expect

One person maintains CloudGround alongside other work:

- issues are usually read within a few days, and one with steps to reproduce comes first;
- a pull request gets a first answer within about a week, a large one takes longer;
- security reports come before everything else.

If a thread has gone quiet, a short comment is welcome. There is no CLA and no sign-off line.

## Licence

CloudGround is source-available under the Elastic License 2.0 ([LICENSE](./LICENSE)): you may
use, modify and self-host it, including to host sites you build for your clients; you may not
offer CloudGround to others as a hosted or managed service. The PHP under `connector/` is
GPL-2.0-or-later ([connector/LICENSE](./connector/LICENSE)). A contribution is accepted under the
licence of the files it changes. Opening a pull request confirms you have the right to contribute
the code under those terms.
