# `internal/changelog` — the release notes the panel shows (specification)

The panel shows what changed after an update (issue #106) from `CHANGELOG.md`, which ships inside
the binary: the root package `cloudground` embeds it with `//go:embed` at build time, so it works
offline and always describes the release it is in.

## 1. `Parse(md) []Entry`

Splits the text at its `## vX.Y.Z` headings (a `v`, a digit, then anything but white space), in
the order written, newest first. An `Entry` is the tag (`Version`) and the Markdown under the
heading without it (`Body`, trimmed). The text before the first heading belongs to no release. Any
other `## ` heading ends the entry before it and starts none.

## 2. `Unseen(entries, seen, current) []string`

The versions an account is told about: those after `seen` (the release it last saw, `""` for none)
up to and including `current` (the running release), newest first, ordered with `version.Compare`.

- An account that has seen none (`seen` is `""`, as every account is the first time after the
  panel starts to record it) is told about the newest release the build contains that is not
  after `current`, not about every release there ever was.
- A release after `current` is never listed; a downgrade lists none; a `current` that is not a
  version (a development build) lists none.
- A tag `-N-gHASH` after a release orders as that release (version SPEC §2).
- A `seen` that is not a version (an account made by a development build) counts as `""`.

## 3. New accounts

An account created while a release runs (setup's first administrator, a user added in the panel,
the administrator host recovery makes) is given that release as `seen`, so a fresh install does
not open with "What's new": it is not an update. Only accounts that existed before the column
(migration 28) have none and are told about the running release once.

## 4. Tests

`changelog_test.go`: the split (introduction, a non-version heading, empty text), the cases above,
and that the shipped `CHANGELOG.md` parses into releases that each have text. The routes that
serve it are internal/api SPEC §2.9.
