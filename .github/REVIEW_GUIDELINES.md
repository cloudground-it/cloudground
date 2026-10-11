# How pull requests are reviewed

This page is the checklist a reviewer works through, written down so that every review asks the
same questions and a contributor knows them in advance.

CloudGround runs as root on other people's servers, creates their sites and keeps the keys to
their backups. A careless merge breaks someone's production, so the bar is higher than for most
web projects. It is meant to be predictable, not hostile.

## First

- **An issue or a reason.** A pull request that does not say which problem it solves gets that
  question before any line is read.
- **CI is green, and red means something.** A failing job is investigated before anyone calls it
  flaky; a green one is required but proves little on its own.
- **Changes to what runs on a server are run on a server.** nginx, PHP-FPM, MariaDB, the
  installer and the agent are tried on a throwaway machine (see the table below).

## Questions for every change

### Does it work, and only do that

- It does what the description says. Unrelated edits go in their own pull request: a fix mixed
  with a refactor cannot be reviewed as either.
- The failure paths are handled: a missing file, no network, a full disk, a process killed half
  way. Nobody watches this code while it runs.
- No error disappears. A `|| true` or an ignored `err` carries a comment saying why.

### The line between panel-api and panel-agent

The review never skims this part.

- `panel-api` has no privileges and `panel-agent` is root. Whatever new crosses the agent socket
  is a typed method with checked parameters, never a shell string or a pasted-together path
  ([docs/agent-api-boundary.md](../docs/agent-api-boundary.md)).
- Programs are started with an argument vector. `sh -c` with a value from a request in it is
  refused outright.
- Anything inside a site's tree goes through `internal/agent/sitefs`, beneath the site's root,
  never through joined strings or the coreutils.
- SQL binds its values; a table or database name is checked against the existing rules before it
  is used.

### What it exposes

- If an operator, a site or an API token can now reach more than before, the description says so.
- Secrets stay out of argument lists (other users can read `/proc`), out of logs and out of the
  repository. MariaDB passwords travel on stdin for that reason.
- Sign-in, sessions, two-factor codes, tokens and the setup token get a second reviewer and tests.

### Tests

- A fix arrives with a test that fails without it. A test that passes on both builds is not
  testing the fix.
- New behaviour has at least one test of a failure, not just of the success.
- An e2e check for anything a server would show (e2e/README.md), asserting on the product and not
  on what the test prepared.
- `go test -race` passes; it has found real races here.

### Words

- A change in behaviour updates the package's `SPEC.md` in the same commit, `docs/` for
  developers and the `cloudground-docs` repository for users.
- New settings, CLI commands and API routes are documented in the same pull request.
  `docs/api.md` is generated: regenerate it, do not edit it.
- `CHANGELOG.md` gets a line for anything a user of the panel would notice.
- Every string a user sees is translated in all five locales.

### Form

- `gofmt` and `go vet` clean; shell scripts pass `shellcheck`.
- A comment explains a reason: a measurement, a constraint, an option that was tried and dropped.
  One that repeats the code is removed.
- New code looks like the code around it.

## Verified how

Reading the diff is not enough for a change to a running system.

| Area | Evidence the review expects |
| --- | --- |
| Installer | a full run on throwaway Ubuntu 24.04 and 26.04 servers |
| nginx and the page cache | the headers of real responses, not the rendered file |
| PHP-FPM and site isolation | an attempt from another site's user that fails |
| Backups | a restore that was checked, not an upload that succeeded |
| UI | the page as rendered, in every locale, on desktop and mobile |
| Performance | an A/B on one server, one setting at a time |

A directive in a file is not a directive in effect: security headers that nginx silently dropped,
and a feature probe that read the wrong output stream, both passed review here.

## Merging

- Pull requests are squashed. The title becomes the commit subject, so it is edited until it
  describes the change.
- The commit body says what changed, why, and what was measured.
- Required checks green, discussions resolved, branch current with `main`.

## Saying no

Early, and with the reason. A clear "not this" in a day is kinder than a pull request left open
for weeks. When the idea is good and the code is not, say which part needs to change; when it is
outside the project's scope, point to the scope in ROADMAP.md.

People contribute in their own time. Reviews say what a change needs, never that its author is
wrong.
