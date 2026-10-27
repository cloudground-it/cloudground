# ops/functions — a site's functions on the server (specification)

The agent's half of #143 (internal/functions SPEC, internal/api SPEC §3.20).

## 1. Methods

| Method | Params → Result | Table row |
|---|---|---|
| `SetFunction` | `SetFunctionParams{Site, Function, Code?, Env}` → `FunctionResult{Release, Releases}` | Sites, `LockLiveSite`, 2 min |
| `DeleteFunction` | `FunctionRef{Site, Name}` → none | Sites, `LockLiveSite`, 2 min |
| `FunctionCode` | `FunctionRef{Site, Name}` → `FunctionCodeResult{Release, Code, Releases}` | Sites, read-only, 30 s |
| `FunctionRollback` | `FunctionRollbackParams{Site, Name, Release?}` → `FunctionResult` | Sites, `LockLiveSite`, 1 min |
| `InvokeFunction` | `InvokeFunctionParams{Site, Name, Source, Body?}` → `InvokeResult{Status, MS, Body, Error}` | Sites, read-only, 6 min |
| `FunctionLogs` | `FunctionRef{Site, Name}` → `FunctionLogsResult{Invocations, Output}` | Sites, read-only, 30 s |

`Validate` checks the function (`functions.Check`), the code's size and UTF-8, the environment
(`runtimes.CheckEnv`), a release ID and a name.

## 2. SetFunction

1. The runtime version must be installed (ops/runtimes `List`), else `invalid`.
2. **Code** (when given, and always for a function without a release): a new release
   `functions/<name>/releases/<id>/<file>` (§2 of the package SPEC; the id made by the agent, a
   ULID), `current` moved to it with a link renamed over the old one, and releases beyond the newest
   10 removed (never the live one). The directories down to `releases/` are made root's with the
   site's group, 0750; through `sitefs`, so nothing follows a link the site user made.
3. The shims are (re)written under `/etc/cloudground-fn/shim/` when they differ.
4. The environment file `/etc/cloudground-fn/<user>-<name>.env`, root's, 0600 (the directory is
   0755 for the shims beneath it: a file of 0600 stays root's alone).
5. The two units (package SPEC §3) are written; when either changed, `daemon-reload`; the socket is
   enabled and started (a running socket whose unit changed is restarted). A failure puts the old
   unit files back (and reloads).

Answer: the live release and the release list, newest first.

## 3. DeleteFunction

The socket is stopped and disabled, the running instances (`<unit>@*.service`) stopped, both unit
files and the environment file removed, `daemon-reload`, and `functions/<name>` removed from the
tree. A site deletion (ops/sites SPEC §6) does the same for every `cloudground-fn-<user>-*` unit and
removes `/etc/cloudground-fn/<user>-*.env`.

## 4. FunctionCode and FunctionRollback

**FunctionCode** reads `current` (it must name `releases/<id>`, a real directory) and the file there
(at most 256 KiB), and lists the releases. **FunctionRollback** without a release goes to the newest
release older than the live one (`conflict` when there is none), with one points `current` at it
(`not_found` when it is not a release).

## 5. InvokeFunction

An HTTPS request from the agent to nginx on `127.0.0.1:443` (SNI and `Host` the site's domain; the
certificate is not checked: the connection never leaves the machine), `POST
/.functions/<name>` with `User-Agent: CloudGround-Functions`, `X-CloudGround-Source: <source>`
(`schedule` or `test`; the shim puts it in the event's `source`) and the body given (at most 64
KiB). So a scheduled or test run goes through the same nginx, socket and limits as a visitor's,
and lands in the same logs. Bound: the function's timeout plus 10 seconds. Answer: the status (0
when nothing came back), the milliseconds, the first 4 KiB of the body, the transport's error.

## 6. FunctionLogs

**Invocations**: the site's access log (its last 2 MiB) read for requests to `/.functions/<name>`
(and beneath): time, method, status, nginx's request time in milliseconds, and `schedule`/`test`
when the user agent is the agent's, else `http`; the newest 100, newest first. A request killed for
its timeout or its memory shows as nginx's `502`. **Output**: the last 200 lines of
`fn-<name>.log` (the handlers' own output and the shim's invocation lines).

## 7. Tests

`functions_test.go`: a function set up on a fake tree (units, env file 0600, shims, code release,
`current`), a second code change making a second release and a rollback back, pruning past 10,
a missing runtime refused, deletion removing everything, the invocation over a test TLS server
standing for nginx, the access log parsed into invocations.
