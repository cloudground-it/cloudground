# `internal/functions` — Lambda-style Node.js and Python handlers (specification)

Issue #143, built on #28's runtimes and sandbox (internal/runtimes SPEC). A **function** belongs to a
site: one handler file in Node.js or Python, its runtime version, memory and timeout, environment
variables and secrets, and optionally a schedule. It answers at
`https://<domain>/.functions/<name>` (and every path beneath) and runs only while it is invoked.
This package is pure: names and limits, the schedule, the units, the runtime shims and the event.
ops/functions installs and invokes; internal/engine SPEC §4.13 routes; internal/api SPEC §3.22 is
the panel's side.

## 1. A function (`state.SiteFunction`, `config.functions`)

| Field | Rule (`Check`) |
| --- | --- |
| `name` | `^[a-z][a-z0-9-]{0,30}$`, unique in the site |
| `runtime` | `node` or `python` |
| `version` | the runtime's version form (internal/runtimes SPEC §1) |
| `memory_mb` | 64–1024 (default 128) |
| `timeout_sec` | 1–300 (default 10) |
| `schedule` | `""` or five cron fields (`cronjob.ValidateSchedule`: numbers, `*`, ranges, steps, lists) |
| `env`, `secrets` | as a managed app's (internal/runtimes SPEC §2; the values of secrets sealed as `site.<id>.fn.<name>.<NAME>`) |
| `release` | the live code release (set by the agent's answer) |

At most 20 functions per site. Functions are not copied to a staging site.

## 2. The code

One file: Node.js `index.js` (CommonJS or ES module, loaded with `import()`), Python `main.py`;
at most 256 KiB of UTF-8. The handler is the module's `handler` (or its default export's), called
with the event (§5), plain or `async` (a coroutine function in Python), and its value is the
answer. Only the runtime's standard library is available: no `package.json` or
`requirements.txt` is installed for a function.

Each change of code is a **release** of its own: `<root>/functions/<name>/releases/<id>/<file>`
(the id a release ID, internal/site), with `<root>/functions/<name>/current` pointing at the live
one. The directories down to `releases/` are root's with the site's group (0750), so the site user
cannot swap a release for a link; a release's own directory and file are the site user's. The
newest 10 releases are kept. A rollback points `current` at the release before the live one, or
at a named one. Nothing has to be restarted: every invocation starts from `current` (§3).

## 3. The units: socket activation, one process per invocation

Per function, two unit files in the systemd directory, `<unit>` being
`cloudground-fn-<user>-<name>`:

`<unit>.socket`

```
[Socket]
ListenStream=/run/cloudground/fn/<user>/<name>.sock
SocketUser=<user>
SocketGroup=<user>
SocketMode=0660
DirectoryMode=0755
Accept=yes
MaxConnections=32
MaxConnectionsPerSource=32
[Install]
WantedBy=sockets.target
```

`<unit>@.service`

```
[Unit]
Description=Function <name> of <domain>
CollectMode=inactive-or-failed
[Service]
Type=simple
User=<user>
Group=<user>
Slice=<user>.slice
WorkingDirectory=<root>/functions/<name>/current
Environment=PATH=<runtime bin>:/usr/local/bin:/usr/bin:/bin  HOME=/tmp  CG_FUNCTION=<name>
Environment=CG_HANDLER=<root>/functions/<name>/current/<file>  CG_TIMEOUT_MS=<timeout − 250 ms>
EnvironmentFile=-/etc/cloudground-fn/<user>-<name>.env
ExecStart=<node or python3.N> /etc/cloudground-fn/shim/<fn.mjs|fn.py>
StandardOutput=append:/var/log/cloudground/<domain>/fn-<name>.log
StandardError=inherit
RuntimeMaxSec=<timeout_sec>
MemoryMax=<memory_mb>M
MemorySwapMax=0
TasksMax=64
<phpfpm.RunSandbox(site)>
```

- **Scale to zero.** Nothing of a function runs between invocations: systemd holds the socket,
  and each connection starts one instance (`Accept=yes`) that serves its request and exits — an
  idle timeout of zero. nginx speaks HTTP/1.0 to it, one request per connection.
- **A flood is bounded before it spawns anything.** With one process per connection, the number of
  connections is the number of processes. The socket takes at most 32 at a time
  (`MaxConnections`; `MaxConnectionsPerSource` is the same 32, since the only source is nginx's
  workers). The per-visitor limits are nginx's (internal/engine SPEC §4.13): each client address at
  most 10 requests a second with a burst of 20 (beyond: **429**) and 8 at a time, and the whole
  site's functions at most 32 at a time — the socket's own bound, so the socket never has to refuse
  (beyond either: **503**). So a burst costs at most 32 processes per site, whatever comes in.
- **Limits are systemd's**, per invocation: `RuntimeMaxSec` kills an invocation past its timeout
  (whatever the handler does: a busy loop too), `MemoryMax` with no swap kills one past its memory,
  `TasksMax` caps its threads. The shim answers `504` itself 250 ms before the timeout when the
  handler only awaits, so the visitor gets a clear answer; the kill is the guarantee.
- The socket is the site user's and its group's (0660): nginx's workers are in every site group,
  nobody else is.
- The instance's cgroup is inside the site's slice: the site's own limits hold its functions too.
- `EnvironmentFile` is root's, 0600, read by systemd before the user changes (as a managed app's).

## 4. The shims

`ShimNode` and `ShimPython` are the two small programs, embedded in the agent and written to
`/etc/cloudground-fn/shim/` (root's, 0644, the directory 0755: readable inside every site's
sandbox). Each takes the accepted connection on file descriptor 3 (`LISTEN_FDS=1`), reads one
HTTP/1.x request (headers at most 64 KiB, body at most 6 MiB, both also bounded by nginx), builds
the event, loads `CG_HANDLER`, calls the handler, writes one response with `Connection: close`, and
exits. A thrown error, an unparseable answer or a missing handler is a `500` with a short message
and the stack on standard error (the function's log). One line of JSON per invocation goes to
standard output: `{"time", "status", "ms", "error"?}`.

## 5. The event and the answer

Event (JSON-like, the same in both runtimes):

| Field | |
| --- | --- |
| `method` | `GET`, `POST`, … |
| `path` | the request's path after `/.functions/<name>` (`/` when nothing follows) |
| `query` | the raw query string, without `?` |
| `headers` | lower-cased names → values (repeated headers joined with `, `) |
| `body` | the body as text when it is UTF-8, else base64 |
| `isBase64Encoded` | whether `body` is base64 |
| `source` | `http`, or `schedule` for a scheduled run (§6) |

The answer is either an object with `statusCode` (100–599; default 200), `headers` (string values)
and `body` (string; `isBase64Encoded: true` for base64), or anything else: a string is sent as
`text/plain`, any other value as JSON. 

## 6. The schedule (`Due`)

`Due(schedule, t)`: whether a five-field cron schedule fires in `t`'s minute, in the panel's local
time, with cron's rules (a step `*/n` or `a-b/n`; when both day of month and day of week are
restricted, either matching is enough). The panel's `functions.sweep` (every minute) invokes each
function that is due, once per minute (internal/api SPEC §3.22).

## 7. Tests

`functions_test.go`: every rule of §1 with its message; the two units (golden-like assertions);
`Due` against a table (steps, ranges, lists, both day fields); the shims run for real when `node`
or `python3` is on the machine, on a socketpair standing for the accepted connection: a JSON
answer, a string, a thrown error (500), a slow handler (504 before the timeout), a binary body
(base64), the path and query.
