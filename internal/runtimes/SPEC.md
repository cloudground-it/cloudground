# `internal/runtimes` — Node.js and Python for managed sites (specification)

Issue #28. A **managed app site** (type `node` or `python`) runs the operator's application as a
systemd unit inside the site's slice and run sandbox, as the site user, and nginx proxies to it. This
package is pure: it names the runtimes and their versions, renders the app's unit and environment
file, writes the dependency step's script, and reads Node's release index. The agent's
`ops/runtimes` installs runtimes and runs the dependency step; `ops/sites` writes and loads the unit
(its SPEC §11); the panel's side is internal/api SPEC §3.19.

## 1. Runtimes and versions

| Runtime | Site type | Versions | Where it lives |
| --- | --- | --- | --- |
| Node.js | `node` | the majors `20`, `22`, `24` | `/var/lib/cloudground-runtimes/node/<major>/` (the official tarball, ops/runtimes SPEC §2) |
| Python | `python` | the distro's `3.N` that has `/usr/bin/python3.N` (Ubuntu 24.04: `3.12`; 26.04: its own) | the distro's interpreter; each release gets its own virtual environment, `.venv` (§4) |

`ValidVersion(runtime, v)` checks the form (`^(20|22|24)$`, `^3\.[0-9]{1,2}$`). Whether a version
is installed is the agent's answer (`Runtimes`).

`Bin(site)` is the directory put first on the app's `PATH`: `<runtimes>/node/<major>/bin` for
Node, `<root>/current/.venv/bin` then `/usr/bin` for Python.

## 2. The app's settings (`state.SiteApp`, `config.app`)

| Field | Meaning | Rule (`CheckApp`) |
| --- | --- | --- |
| `version` | the runtime version | §1 |
| `start` | the start command, run by `/bin/sh -c` in `<root>/current` | one line, 1–1024 bytes, no NUL |
| `env` | plain environment variables, shown in the panel | at most 100 with the secrets; name `^[A-Z_][A-Z0-9_]{0,63}$`; value at most 4096 bytes, no line break or NUL |
| `secrets` | the names of the secret variables, whose values are sealed (`site.<id>.app_env.<NAME>`, state SPEC) | names as `env`, not also in `env` |
| `port` | `0`: the app listens on a unix socket (the default); else a TCP port on 127.0.0.1 | 0 or 1024–65535, not a reserved port (internal/engine SPEC §7.3) |
| `health_path` | what the health probe asks for | `""` (`/`) or a path as the uptime check's |

Reserved names, refused in `env` and `secrets`: `PORT`, `HOST`, `PATH`, `HOME`, `USER`,
`VIRTUAL_ENV`, every `CG_*` and `LD_*`, `NODE_OPTIONS` (it can load a file as root's child before
the app's own code; an app sets its options in its start command).

Default start commands: Node `node server.js`; Python `python -m gunicorn --bind unix:$PORT
--workers 2 app:app` (an ASGI app: `python -m uvicorn --uds $PORT app:app`). `python -m`, not the
`gunicorn` script: a script's first line names the venv's absolute path, a module run does not.

## 3. The unit (`RenderUnit`)

`cloudground-app-<user>.service` in the systemd directory:

```
[Unit]
Description=<type> app for <domain>
After=network.target mariadb.service

[Service]
Type=simple
User=<user>
Group=<user>
Slice=<the site's slice>
WorkingDirectory=<root>/current
Environment=PATH=<Bin>:/usr/local/bin:/usr/bin:/bin
Environment=HOME=/tmp
Environment=PORT=<socket or port>
Environment=CG_SOCKET=<socket>             (unix socket only)
Environment=NODE_ENV=production            (Node)
Environment=VIRTUAL_ENV=<root>/current/.venv  PYTHONUNBUFFERED=1   (Python)
EnvironmentFile=-/etc/cloudground-app/<user>.env
ExecStart=/bin/sh -c 'exec <start>'
RuntimeDirectory=cloudground/app/<user>
RuntimeDirectoryMode=0750
UMask=0007
StandardOutput=append:/var/log/cloudground/<domain>/app.log
StandardError=inherit
Restart=always
RestartSec=2
StartLimitIntervalSec=0
TimeoutStopSec=20
KillMode=mixed
<phpfpm.RunSandbox(site)>

[Install]
WantedBy=multi-user.target
```

- The socket is `/run/cloudground/app/<user>/app.sock`. The runtime directory is the site user's
  and its group's (nginx's workers are in every site group); `UMask=0007` makes the socket the
  app creates writable by that group and by nobody else. `PORT` is the socket's path: Node's
  `listen(process.env.PORT)` and gunicorn's `unix:$PORT` both take a path.
- With `port`, `PORT` is the number and nginx proxies to `127.0.0.1:<port>`. Any local process can
  reach a loopback port; the panel says so beside the setting.
- `EnvironmentFile` is read by systemd, as root, before the user is changed: the file is root's,
  0600, and the app sees its values in its environment and nowhere else. `-`: a missing file is no
  error.
- `ExecStart` is the start command as one argument of `sh -c`, quoted for systemd (§5); `exec` so
  the app is the main process and gets the stop signal.
- `Restart=always` with no start limit: an app that exits, or crashes, is started again two
  seconds later, for ever; how often is `NRestarts` (health, ops/sites SPEC §11.3).
- `phpfpm.RunSandbox`: the confinement every run of the site has (its own tree and logs only,
  private `/tmp`, no setuid, no capabilities, the system-service syscall set).
  `/var/lib/cloudground-runtimes` is not hidden by it: the runtimes are readable by every site.
- The slice's limits (memory, CPU, tasks) hold the app, as they hold a PHP master.

## 4. The dependency step (`PrepareScript`)

Run by ops/runtimes in a release, as the site user (ops/runtimes SPEC §3). `/bin/sh -eu`:

- **Node**: `package-lock.json` (or `npm-shrinkwrap.json`) → `npm ci --omit=dev --no-audit
  --no-fund`; else `package.json` → `npm install --omit=dev --no-audit --no-fund`; else nothing.
- **Python**: `python3.N -m venv .venv` (a release that brings its own `.venv` has it made again:
  one built elsewhere names another tree), then, with `requirements.txt`,
  `.venv/bin/python -m pip install --no-cache-dir --disable-pip-version-check -r requirements.txt`.

One environment per release (not per site), inside the release: a rollback goes back to the
dependencies the release was built with, and pruning a release takes its dependencies with it.

## 5. The environment file (`RenderEnv`) and quoting

`KEY="value"` per line, sorted, after a header comment, with `\` and `"` escaped by a backslash
(systemd's rules for a double-quoted value; `$` is not expanded in an environment file). The values
were checked (§2), so no line break reaches the file. `ExecStart` quotes the start command for
systemd: single-quoted for the shell after `\` and `'` are escaped, with `%` doubled (systemd's
specifiers) and `$` doubled (systemd's own variable expansion), so the shell gets the text the
operator typed.

## 6. Node releases (`PickNode`, `ParseShasums`)

`PickNode(index, major)` reads `https://nodejs.org/dist/index.json` (an array of `{version}`) and
answers the newest `vMAJOR.x.y`. `ParseShasums(text, file)` answers the hex SHA-256 the
`SHASUMS256.txt` of that release gives for `node-<version>-linux-<arch>.tar.gz` (`x64`, `arm64`);
a missing line or a malformed digest is an error.

## 7. Tests

`runtimes_test.go`: version forms; every rule of §2 with its message; the unit for each runtime,
socket and port (golden); the environment file's escaping; the start command's quoting through
`sh` (a value with `'`, `$`, `%` comes out as typed); the script for each case; `PickNode` and
`ParseShasums` on real-shaped inputs.
