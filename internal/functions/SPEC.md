# `internal/functions` — standalone Node.js and Python functions (specification)

Issue #143, made standalone in v0.9.0 and built on #28's runtimes (internal/runtimes SPEC). A
**function** is a server-wide resource, not part of any site: its code in Node.js or Python (one or
more files, with their dependencies), its runtime version, memory and timeout, environment
variables and secrets, an optional schedule, who may call it (§1.1) and the addresses it answers
at (§1.2). It runs as a system user of its own and only while it is invoked.

This package is pure: names, limits, code rules, the schedule, the units, the sandbox and the
runtime shims. ops/functions writes and runs them; internal/engine SPEC §8 routes; internal/api
SPEC §17 is the panel's side; internal/state SPEC (Functions) stores them.

## 1. A function (`state.Function`, table `functions`)

| Field | Rule (`Check`) |
| --- | --- |
| `id` | the row's id: names the system user, the units and the files (§3) |
| `name` | `^[a-z][a-z0-9-]{0,30}$`, unique on the server |
| `runtime` | `node` or `python` |
| `version` | the runtime's version form (internal/runtimes SPEC §1) |
| `memory_mb` | 64–1024 (default 128) |
| `timeout_sec` | 1–300 (default 10) |
| `schedule` | `""` or five cron fields (`cronjob.ValidateSchedule`) |
| `env`, `secrets` | as a managed app's (internal/runtimes SPEC §2); at most 100 together; a secret's value is sealed as `fn.<id>.<NAME>` |
| `auth` | `public` (anyone may call it) or `token` (§1.1); default `public` |
| `cors_origins` | at most 20: `*` alone, or origins `http[s]://host[:port]` (lower-case host, no path) |
| `domains` | the function's own host names (§1.2), at most 10 (`function_domains`) |
| `release` | the live code release (set from the agent's answer) |

At most 100 functions on a server (`MaxFunctions`).

### 1.1 Authentication

With `auth: token` a request must carry `Authorization: Bearer <token>`; nginx checks it before the
function is started, and anything else is a **401** (internal/engine SPEC §8.3). The token is
`cgf_` and 43 characters of URL-safe base64 (32 random bytes, `NewToken`), sealed as
`fn.<id>.token`; the panel shows it once, when it is made. A test run and a scheduled run (§6) come
in on the agent's own socket and are not asked for it.

### 1.2 Addresses

- **The functions domain** (a panel setting, `functions_domain`): every function answers at
  `https://<functions domain>/<name>` and every path beneath, with one certificate for the domain
  (lineage `cloudground-functions`).
- **A function's own domains**: each answers entirely for that one function, at `/` and every path
  beneath, with one certificate for all of them (lineage `cloudground-fn-<id>`).
- **Neither**: the function is not reachable from the Internet. It still runs from a test run and
  from its schedule.

`URLs(f, functionsDomain)` lists them: the functions domain's first, then each own domain's.

## 2. The code

A release is a set of files (`Files`, path → content):

- at most 64 files, 1 MiB together, each UTF-8 text without NUL;
- a path is relative, of `/`-separated parts `[A-Za-z0-9._-]{1,100}`, none `.` or `..`, at most
  255 characters, never under `node_modules/` or `.venv/`, never `.cg-files`;
- the **entry file** must be there: Node.js `index.js` (CommonJS or ES module, loaded with
  `import()`), Python `main.py`. The handler is the entry module's `handler` (or its default
  export's), called with the event (§5), plain or `async`.

**Dependencies.** A Node.js release with a `package.json` has them installed with
`npm ci --omit=dev` when it has a `package-lock.json` (or `npm-shrinkwrap.json`), else `npm install
--omit=dev`; a Python release with a `requirements.txt` gets a `.venv` in the release and
`pip install -r requirements.txt` into it (`PrepareScript`). The step runs in the release, as the
function's user, in the install sandbox (§3.3), at most 20 minutes; a failure removes the release
and leaves the live one serving. Each release has its own dependencies, so a rollback brings back
exactly the ones its code was released with.

**Releases.** Each change of code is a release: `/srv/functions/<name>/releases/<id>/` (the id a
release ID, internal/site: a ULID made by the agent), with `/srv/functions/<name>/current` a
relative link to `releases/<id>`. The file `.cg-files` in a release lists its code files (JSON),
which is what the panel reads back (a release from before v0.9.0 has none: its entry file is its
code). The newest 10 releases are kept, and always the live one. A rollback points `current` at the
release before the live one, or at a named one; nothing restarts: every invocation starts from
`current` (§3).

**Ownership.** `/srv/functions` is root's, 0711 (no site lists it); `/srv/functions/<name>` and its
`releases/` are root's with the function's group, 0750, so neither a site user nor the function's
own user can swap a release or `current`; a release directory and its files are the function's
user's (0750, 0640): it writes there only while its dependencies are installed, and runs the code
from a read-only view of it. Every write by root goes through `sitefs` (`openat2` with
`RESOLVE_BENEATH`), so nothing follows a link the function's user made.

## 3. Running: user, slice, units

| What | Where |
| --- | --- |
| system user and group | `cg-fn-<id>` (`User(id)`; no login, no home) |
| slice | `cg-fn-<id>.slice`, a child of `cg-fn.slice` (`ParentSlice`) |
| socket | `/run/cloudground/fn/<id>.sock` |
| units | `cloudground-fn-<id>.socket`, `cloudground-fn-<id>@.service` |
| environment file | `/etc/cloudground-fn/<id>.env`, root's 0600 |
| logs | `/var/log/cloudground/functions/<name>.access.log` (nginx) and `<name>.output.log` (the handler's output); the server's log rotation already covers `/var/log/cloudground/*/*.log` |

### 3.1 The parent slice

`cg-fn.slice` (written by the agent when it sets a function up) holds every function's slice, so
all the functions together are capped, as each site is by its own slice:

```
[Slice]
CPUAccounting=yes
MemoryAccounting=yes
TasksAccounting=yes
MemoryHigh=40%
MemoryMax=50%
CPUWeight=50
TasksMax=4096
```

The name `cloudground-functions.slice` the plan proposed cannot be the parent: systemd places a
slice by the dashes of its name, so `cg-fn-<id>.slice` is always beneath `cg-fn.slice`.

### 3.2 The units: socket activation, one process per invocation

`cloudground-fn-<id>.socket`

```
[Socket]
ListenStream=/run/cloudground/fn/<id>.sock
SocketUser=root
SocketGroup=www-data
SocketMode=0660
DirectoryMode=0755
Accept=yes
MaxConnections=32
MaxConnectionsPerSource=32
[Install]
WantedBy=sockets.target
```

`cloudground-fn-<id>@.service`

```
[Unit]
Description=Function <name>
CollectMode=inactive-or-failed
[Service]
Type=simple
User=cg-fn-<id>
Group=cg-fn-<id>
Slice=cg-fn-<id>.slice
WorkingDirectory=/srv/functions/<name>/current
Environment=PATH=<runtime bin>:/usr/local/bin:/usr/bin:/bin  HOME=/tmp  CG_FUNCTION=<name>
Environment=CG_HANDLER=/srv/functions/<name>/current/<entry>  CG_TIMEOUT_MS=<timeout − 250 ms>
EnvironmentFile=-/etc/cloudground-fn/<id>.env
ExecStart=<node or python3.N> /etc/cloudground-fn/shim/<fn.mjs|fn.py>
StandardOutput=append:/var/log/cloudground/functions/<name>.output.log
StandardError=inherit
RuntimeMaxSec=<timeout_sec>
MemoryMax=<memory_mb>M
MemorySwapMax=0
TasksMax=64
<Sandbox(name)>
```

- **Scale to zero.** systemd holds the socket and each connection starts one instance
  (`Accept=yes`) that serves its request and exits. nginx speaks HTTP/1.0 to it.
- **A flood is bounded before it spawns anything**: at most 32 connections at a time per function
  (`MaxConnections`, the only source being nginx); per visitor nginx allows 10 requests a second
  with a burst of 20 (beyond: **429**) and 8 at a time, and per function 32 at a time (beyond:
  **503**), internal/engine SPEC §8.4.
- **Limits are systemd's**, per invocation: `RuntimeMaxSec`, `MemoryMax` with no swap, `TasksMax`.
  The shim answers `504` itself 250 ms before the timeout when the handler only awaits.
- The socket is root's and nginx's group's (0660): nginx's workers reach it, no site user and not
  even the function's own user.
- `EnvironmentFile` is read by systemd before the user changes.

### 3.3 The sandbox

`Sandbox(name)` is the run's confinement, the sites' (internal/phpfpm `Sandbox` and
`RunHardening`) with the function's code in place of a site's tree:

```
ProtectSystem=strict
TemporaryFileSystem=/srv/sites:ro /srv/functions:ro /var/log/cloudground:ro
TemporaryFileSystem=/dev/shm:size=64M,mode=1777,nosuid,nodev,noexec
BindReadOnlyPaths=/srv/functions/<name>
InaccessiblePaths=-/etc/nginx -/etc/cloudground -/etc/letsencrypt -/etc/mysql -/var/lib/cloudground -/var/lib/cloudground-agent -/var/cache/cloudground -/var/log/nginx -/var/backups
ProtectHome=yes  PrivateTmp=yes  PrivateDevices=yes  ProtectProc=invisible  ProcSubset=pid
NoNewPrivileges=yes  ProtectKernelTunables=yes  ProtectKernelModules=yes  ProtectKernelLogs=yes
ProtectControlGroups=yes  ProtectClock=yes  RestrictNamespaces=yes  RestrictRealtime=yes
LockPersonality=yes  RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6  CapabilityBoundingSet=
RestrictSUIDSGID=yes  PrivateIPC=yes  SystemCallFilter=@system-service  SystemCallErrorNumber=EPERM
```

No site's tree, no other function's code, no log: the output is opened by systemd. The
**install sandbox** (`InstallSandbox(name, release)`) is the same with the release itself bound
read-write (`BindPaths=/srv/functions/<name>/releases/<id>`) so the dependencies can be written
there; it has the network, like the run.

## 4. The shims

`ShimNode` and `ShimPython`, embedded in the agent and written to `/etc/cloudground-fn/shim/`
(root's, 0644, the directory 0755). Each takes the accepted connection on file descriptor 3, reads
one HTTP/1.x request (headers at most 64 KiB, body at most 6 MiB), builds the event, loads
`CG_HANDLER`, calls the handler, writes one response with `Connection: close`, and exits. A thrown
error, an unparseable answer or a missing handler is a `500` with a short message and the stack on
standard error (the output log). One line of JSON per invocation goes to standard output:
`{"time", "status", "ms", "error"?}`.

- The Python shim puts the entry file's directory first on `sys.path` (so a release's other modules
  import) and, when the release has a `.venv`, its `site-packages` (so the dependencies import).
  Node.js resolves a release's `node_modules` by itself.
- `X-CloudGround-Prefix` (set by nginx: `/<name>` on the functions domain and the agent's socket,
  absent on a function's own domain) is cut from the start of the path and left out of the event's
  headers; `X-CloudGround-Source` too, read into `source`.

## 5. The event and the answer

| Field | |
| --- | --- |
| `method` | `GET`, `POST`, … |
| `path` | the request's path: after `/<name>` on the functions domain (`/` when nothing follows), the whole path on an own domain |
| `query` | the raw query string, without `?` |
| `headers` | lower-cased names → values (repeated headers joined with `, `) |
| `body` | the body as text when it is UTF-8, else base64 |
| `isBase64Encoded` | whether `body` is base64 |
| `source` | `http`, `test` for a test run, `schedule` for a scheduled run (§6) |

The answer is either an object with `statusCode` (100–599; default 200), `headers` (string values)
and `body` (string; `isBase64Encoded: true` for base64), or anything else: a string is sent as
`text/plain`, any other value as JSON.

## 6. The schedule (`Due`)

`Due(schedule, t)`: whether a five-field cron schedule fires in `t`'s minute, in the panel's local
time, with cron's rules. The panel's `functions.sweep` (every minute) invokes each function that is
due, once per minute (internal/api SPEC §17.6).

## 7. Templates

`Starter(runtime, template)` is the code a new function gets without any: `http` (the default)
answers with the request it got, `scheduled` logs a line and answers when it ran.

## 8. Tests

`functions_test.go`: every rule of §1 and §2 with its message (paths, sizes, the entry file, CORS
origins); the two units and the slice (golden-like assertions: the user, the socket's owner and
group, the sandbox's binds); `Due` against a table; `URLs`; the shims run for real when `node` or
`python3` is on the machine, on a socketpair standing for the accepted connection: a JSON answer, a
string, a thrown error (500), a slow handler (504), a binary body (base64), the path with and
without the prefix header, the query, a second module imported from the release.
