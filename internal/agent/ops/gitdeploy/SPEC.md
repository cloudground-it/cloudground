# `internal/agent/ops/gitdeploy` — fetching and building a site's repository (specification)

The agent's half of git deploy (internal/api SPEC §15, #24): it fetches one branch of a repository
into a site, exports its tree and runs the site's build command, **all as the site user**, and
leaves the tree where `DeployRelease` (ops/release SPEC §3) builds a release from it. It never
makes a release, never promotes, and never runs `git` or the build as root.

## 1. Method

| Method | Params → Result | Table row |
|---|---|---|
| `GitSync` | `GitSyncParams{Site, Repo, Branch, DeployKey?, BuildCommand?, ReleaseID}` → `GitSyncResult{Commit, SourceDir, Output, Failed}` | Sites, `LockLiveSite`, 50 min |

`Validate` (internal/rpc SPEC §3) refuses a bad `ReleaseID`, `Repo` (`site.CheckGitRepo`, with the
site's own root as the only place a `file://` repository may be), `Branch` (`site.CheckGitBranch`) or
`BuildCommand`, and a `DeployKey` that is not a single OpenSSH private key block, before the lock.

## 2. Where it works

`<root>/tmp/cg-git/` (the site's `tmp/` is the site user's; the root is root's, so the site user
could not make a folder beside it), made through `sitefs` (`MkdirAll`, owned by the site user,
0700):

| Path | What |
|---|---|
| `repo/` | the clone, kept between runs: only the changes are fetched |
| `known_hosts` | the SSH host keys seen, pinned after the first connection (§3) |
| `export/<release id>/` | the branch's tree at the fetched commit, no `.git`; `DeployRelease`'s source |

`export/` is emptied at the start of every run, so at most one export is on disk. The folder is
inside `tmp/`, which backups leave out (`backup.TransientDirs`). Nothing here is read by root except
`DeployRelease`, which copies through `sitefs` without following a link.

## 3. The fetch and the export

One transient service, `cg-git-<user>-<nanoseconds>.service`: the site user, the site's slice, the
working directory `tmp/cg-git`, the site's run sandbox (`phpfpm.RunSandbox`: the site's own tree
and nothing of any other site, a private `/tmp`, no setuid, no capabilities), 20 minutes
(`RuntimeMaxSec`), an environment of its own (`PATH`, `LANG`, `HOME` the private `/tmp`,
`GIT_TERMINAL_PROMPT=0`, `GIT_CONFIG_NOSYSTEM=1`, `GIT_CONFIG_GLOBAL=/dev/null`,
`GIT_ALLOW_PROTOCOL=https:ssh:file`; nothing of the agent's). The script, with the repository, the
branch and the release ID as arguments (never spliced into the text), does:

1. with a deploy key, it is read from standard input into a private-`/tmp` file of mode 0600, which
   is removed when the script ends, and `GIT_SSH_COMMAND` is `ssh -i <key> -o IdentitiesOnly=yes
   -o BatchMode=yes -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=<known_hosts>`:
   the first connection to a forge pins its host key, a later change of it is refused. Without a
   key the same command has no `-i`, and with `HOME` empty of keys and `IdentitiesOnly=yes` no
   identity is offered;
2. `git init` the clone when there is none, set `origin` to the repository, and
   `git fetch --depth 1 --no-tags origin +refs/heads/<branch>:refs/remotes/origin/<branch>`;
3. `git rev-parse` the fetched ref (the commit), empty `export/`, and `git archive <commit> | tar -x`
   into `export/<release id>/` (so no `.git`, and `export-ignore` is honoured); the commit is the
   first line of the script's output, `CG-COMMIT <sha>`.

A non-zero exit is `Failed: "fetch"`, `Output` the last 4 KiB of the script's output (a deploy key's
bytes are never printed: the script does not echo it, and the `git`/`ssh` messages do not carry it),
`SourceDir` empty, and an `ok` answer (the failure is the result, as `RunCron`'s is, so the panel
can show the output). The service is stopped when the request ends first.

## 4. The build

When `BuildCommand` is not empty, a second service of its own (`cg-git-build-…`, same user, slice
and sandbox, working directory the export, 30 minutes): `/bin/sh -c <command>`, output on one
stream, the last 4 KiB kept; no deploy key is in it (the first service is gone with its `/tmp`).
A non-zero exit or the time limit is `Failed: "build"`, and the export is removed. A build that
leaves the export is what the release is made from: it may add (`vendor/`, `dist/`) and remove.

## 5. What it does not do

- **Submodules** and **Git LFS** are not fetched: the export is the repository's own tree.
- No credential helper, no token in the address, no HTTP(S) authentication: a private repository
  is read by SSH with the deploy key.
- It does not clean the export after a good run: `DeployRelease` copies it, and the next run (or
  the site's deletion) removes it.

## 6. Tests

`gitdeploy_test.go`, over `host.FakeExec`: the service's argv (user, slice, sandbox, limits, the
environment, the script's arguments and no secret in argv), the key on standard input only,
`Failed` and `Output` on a non-zero exit, the build as a second service without the key, a
`file://` repository outside the site refused, the directories made owned by the site user.
An integration test runs the script against a local bare repository
(`TestScriptFetchesAndExports`, skipped without `git`). e2e: `git deploy webhook` (a site's real
fetch and build as the site user, on the box).
