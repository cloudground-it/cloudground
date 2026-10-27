# ops/functions — standalone functions on the server (specification)

The agent's half of #143 as of v0.9.0 (internal/functions SPEC, internal/engine SPEC §8,
internal/api SPEC §17). A function is no site's: none of these methods carries a site but
`MigrateFunction`, and none takes a site's lock. They serialise among themselves: one lock per
function name for its files and units, the agent's nginx lock for the nginx files, its certbot lock
for certbot.

## 1. Methods

| Method | Params → Result | Row |
|---|---|---|
| `SetFunction` | `SetFunctionParams{Function, Files?, Env}` → `FunctionResult{Release, Releases, Output}` | 25 min |
| `DeleteFunction` | `FunctionRef{ID, Name}` → none | 2 min |
| `FunctionCode` | `FunctionRef` → `FunctionCodeResult{Release, Files, Releases}` | read-only, 30 s |
| `RollbackFunction` | `FunctionRollbackParams{ID, Name, Release?}` → `FunctionResult` | 1 min |
| `InvokeFunction` | `InvokeFunctionParams{Name, Source, Method?, Path?, Body?, TimeoutSec}` → `InvokeResult{Status, MS, Body, Error}` | read-only, 6 min |
| `FunctionLogs` | `FunctionRef` → `FunctionLogsResult{Invocations, Output}` | read-only, 30 s |
| `FunctionsSummary` | `FunctionsSummaryParams{Names}` → `FunctionsSummaryResult{Last}` | read-only, 30 s |
| `RenderFunctions` | `RenderFunctionsParams{Domain, Functions}` → `RenderFunctionsResult{Files}` | 2 min |
| `IssueFunctionsCertificate` | `FunctionsCertParams{Name, Domains, Email}` → none | 20 min |
| `DeleteFunctionsCertificate` | `FunctionsCertParams{Name}` → none | 2 min |
| `MigrateFunction` | `MigrateFunctionParams{Site, OldName, Function, Env}` → `FunctionResult` | Sites, `LockLiveSite`, 25 min |

`Validate` (internal/rpc SPEC §3): the function (`functions.Check`), the files
(`functions.CheckFiles`, when given), the environment (`runtimes.CheckEnv`), an id above 0, a name,
a release ID, a lineage name (`cloudground-functions` or `cloudground-fn-<id>`), each domain
(`site.ValidateDomain`), a source `schedule` or `test`, a method among `GET POST PUT PATCH DELETE`,
a path starting with `/` without spaces or control characters (at most 2 KiB), a body of at most
64 KiB.

## 2. SetFunction

1. The runtime version must be installed (ops/runtimes `List`), else `invalid`.
2. **The user** `cg-fn-<id>`: created when missing (a system user, its own group, shell
   `/usr/sbin/nologin`, home `/nonexistent`, no home made).
3. **The tree** `/srv/functions` (root's 0711) and `/srv/functions/<name>` and its `releases/`
   (root's with the function's group, 0750; their modes and owners set again each time).
4. **Code** (when `Files` is given, and always for a function without a live release: then the
   starter of internal/functions SPEC §7): a new release, its directory and files the function's
   user's, `.cg-files` listing them; the dependency step (internal/functions SPEC §2) when the
   release has a `package.json` (Node.js) or a `requirements.txt` (Python), through
   ops/runtimes's transient service runner (`systemd-run`, `User=cg-fn-<id>`,
   `Slice=cg-fn-<id>.slice`, the install sandbox, 20 minutes); its output, at most 4 KiB, is the
   answer's `Output`. A failed step removes the release and answers `invalid` with the output:
   `current` has not moved. Then `current` is moved (a link renamed over the old one) and releases
   beyond the newest 10 removed (never the live one).
5. The shims under `/etc/cloudground-fn/shim/` when they differ; the environment file
   `/etc/cloudground-fn/<id>.env` (root's, 0600); the log directory
   `/var/log/cloudground/functions` (root's, 0750).
6. `cg-fn.slice` and the two units (internal/functions SPEC §3); when one changed, `daemon-reload`;
   the socket enabled and started (restarted when its unit changed). A failure puts the old unit
   files back.

Root makes directories, writes files and moves links only through `sitefs` on
`/srv/functions/<name>` (`RESOLVE_BENEATH`, owner set at creation), never by a shell; it never
runs the code or the dependency step.

## 3. DeleteFunction

The socket stopped and disabled, the running instances (`cloudground-fn-<id>@*.service`) stopped,
both unit files and the environment file removed, `daemon-reload`; `/srv/functions/<name>`
removed (through `sitefs`), the two logs; the user `cg-fn-<id>` (its processes ended first, as a
site user's, ops/sites SPEC §6.8) and its group; the certbot lineage `cloudground-fn-<id>` (under
the certbot lock). Every step runs even when one before failed; the errors are joined. The nginx
files are the panel's next `RenderFunctions`.

## 4. FunctionCode and RollbackFunction

**FunctionCode** reads `current` (it must name `releases/<id>`, a real directory), the files that
release's `.cg-files` lists (each through `sitefs`, a regular file, within the limits of
internal/functions SPEC §2; without `.cg-files`, the entry file of either runtime) and lists the
releases. **RollbackFunction** without a release goes to the newest release older than the live
one (`conflict` when there is none), with one points `current` at it (`not_found` when it is not a
release).

## 5. InvokeFunction

An HTTP request from the agent to nginx on `functions.InvokeSocket`
(`/var/lib/cloudground-agent/nginx-functions.sock`, the internal server of internal/engine SPEC
§8.2; only root can reach it), `Host: cloudground-functions`, `<method> /<name><path>` (default
`POST /<name>`), `User-Agent: CloudGround-Functions/<source>`, `X-CloudGround-Source: <source>`
and the body given. So a scheduled or test run goes through the same nginx, socket and limits as a
visitor's and lands in the same log, without the token check. Bound: the timeout plus 10 seconds.
Answer: the status (0 when nothing came back), the milliseconds, the first 4 KiB of the body, the
transport's error.

## 6. FunctionLogs and FunctionsSummary

**FunctionLogs.** Invocations: the function's access log (its last 2 MiB) in the `cloudground`
format: time, method, path, status, nginx's request time in milliseconds, and `schedule` or `test`
when the user agent is the agent's, else `http`; the newest 100, newest first. A request killed for
its timeout or its memory shows as nginx's `502`. Output: the last 200 lines of the output log.
**FunctionsSummary**: for each name, its newest invocation (from the last 64 KiB of its access log),
absent when there is none.

## 7. RenderFunctions

The nginx files of internal/engine SPEC §8 for the functions domain and every function, rendered
from the params (each function's `ID`, `Name`, `TimeoutSec`, `Token` when its auth is `token`,
`CORSOrigins`, `Domains`) with the paths only the agent knows: the sockets, the logs, the ACME
webroot, each lineage's certificate when it exists (`<CertLiveDir>/<lineage>/fullchain.pem` and
`privkey.pem`) else the fallback certificate, the internal socket, `http2 on;` when nginx takes it.
Under the nginx lock: each file written (root's, **0600**: a file may hold a token, and only
nginx's master, root, reads its configuration) when it differs; every other file of the three
kinds in the sites directory (`cloudground-functions.conf`, `cloudground-functions-internal.conf`,
`cloudground-fn-<id>.conf`) removed; when anything changed, `nginx -t` and a reload, and on a
failure every file put back as it was (and nginx reloaded). Answer: the files written, with their
SHA-256 (the panel records them for drift).

## 8. Certificates

**IssueFunctionsCertificate** (under the certbot lock): the preflight (ops/certs SPEC: each domain
answers on port 80 from this server), then `certbot certonly --webroot` with `--cert-name <Name>`
and the domains, then (under the nginx lock) `nginx -t` and a reload, so a renewed certificate is
served. The panel's next `RenderFunctions` points a first certificate's files at the lineage.
**DeleteFunctionsCertificate**: `certbot delete` of the lineage (none: nothing to do). `CertStatus`
(ops/certs) also reads `cloudground-functions` and `cloudground-fn-<id>`.

## 9. MigrateFunction (from v0.8.x)

The site's function `OldName` (its releases in `<site root>/functions/<OldName>/releases/`, its
units `cloudground-fn-<site user>-<OldName>.*`, its environment file
`/etc/cloudground-fn/<site user>-<OldName>.env`) becomes the standalone `Function`:

1. steps 1–3 of §2 for the new function;
2. each release copied (`sitefs.CopyTree`, from the site's tree to the function's, owned by the
   function's user, links not followed) and `current` pointed at the release the site's `current`
   named (else the newest);
3. steps 5–6 of §2 with `Env`;
4. the old socket stopped and disabled, its instances stopped, both old unit files and the old
   environment file removed, `daemon-reload`, and `functions/<OldName>` removed from the site's
   tree.

A failure before step 4 removes what steps 1–3 made (as `DeleteFunction`) and leaves the old
function as it was. Answer: as `SetFunction`'s.

A site deletion (ops/sites SPEC §6) still removes every `cloudground-fn-<site user>-*` unit and
`/etc/cloudground-fn/<site user>-*.env`: a v0.8.x function whose migration failed is not left
behind with its site.

## 10. Tests

`functions_test.go`, on a fake tree, systemd and accounts: a function set up (the user, the tree's
owners and modes, units, env file 0600, shims, slice, a release and `current`, `.cg-files`), a
second release and a rollback, pruning past 10, a dependency step that fails leaving `current`
where it was, a missing runtime refused, a file path that leaves the release refused, deletion
removing everything (user, units, tree, logs, lineage), the invocation over a test server standing
for nginx (method, path, headers), the access log parsed into invocations and the summary,
`RenderFunctions` writing, removing stale files and putting everything back when `nginx -t` fails,
`MigrateFunction` copying the releases and removing the old units.
