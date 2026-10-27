# ops/runtimes — installing Node.js and Python, and a release's dependencies (specification)

The agent's half of #28 beside ops/sites: which runtimes the server has, installing one, and the
dependency step of a managed app's release (internal/runtimes SPEC §4).

## 1. Methods

| Method | Params → Result | Table row |
|---|---|---|
| `Runtimes` | none → `RuntimesResult{Node, Python, PythonDistro}` (`Node` and `Python` each `[{version, full}]`) | read-only, 1 min |
| `RuntimeInstall` | `RuntimeInstallParams{Runtime, Version}` → `RuntimesResult` | 20 min |

`Validate` refuses a runtime other than `node` or `python` and a version of the wrong form
(internal/runtimes SPEC §1). `Capabilities` also carries `Runtimes`, so the panel checks a version
when a site is created without a second call.

**Runtimes** reads, for Node, each `<runtimes>/node/<major>/VERSION` (the full version, written
last by an install) whose `bin/node` is a regular file; for Python, every `/usr/bin/python3.N`
whose `venv` module is there (`/usr/lib/python3.N/venv/__init__.py`) and whose `ensurepip` is
there (Ubuntu splits them into `python3.N-venv`). `PythonDistro` is every `3.N` with a
`/usr/bin/python3.N`, installed or not, newest first (`[]` for none): the versions a Python site can
have. `DefaultPython()`, a new Python site's version when none is asked for, is the newest
installed, else the newest of the distro, else `3.12`.

## 2. RuntimeInstall

**Node**: the agent runs itself, `panel-agent runtime-install node <major>`, in a transient unit
`cg-runtime-<ns>.service` (the agent's own unit sees `/var/lib` read-only): root, `ProtectSystem=strict`
with `StateDirectory=cloudground-runtimes` (0755, the one place it may write), private `/tmp`,
`/home` hidden, no capabilities but `CAP_CHOWN CAP_DAC_OVERRIDE CAP_FOWNER`, 512 MB, 15 minutes.
There:

1. `https://nodejs.org/dist/index.json` (at most 8 MiB) → the newest release of the major
   (`runtimes.PickNode`).
2. That release's `SHASUMS256.txt` (at most 64 KiB) → the digest of
   `node-<version>-linux-<x64|arm64>.tar.gz` (`runtimes.ParseShasums`).
3. The tarball (at most 128 MiB), its SHA-256 compared with the digest: another one is refused and
   nothing is unpacked. HTTPS only, the system's roots, no redirect outside `nodejs.org`.
4. It is unpacked in Go into `node/.new-<major>-<random>`, without its top directory: an entry with
   an absolute path, a `..` component, or a link that leads outside the tree is refused, as is
   anything but a file, a directory or a symbolic link; modes are kept without set-id bits,
   everything root's.
5. `VERSION` is written, the old `node/<major>` (if any) is renamed aside, the new one renamed into
   its place, the old one removed. A running app keeps its open binary; the next start takes the
   new one.

The unit's last line is the JSON `{"version": "v22.11.0"}`; a failure is its standard error, as
`internal`.

**Python**: `apt-get install python3.N-venv` (the PHP extension install's transient apt unit,
host.AptUnitArgs) for a `3.N` the distro has (`/usr/bin/python3.N` exists); else `invalid`
(`Python 3.N is not this system's: it has <list>`).

Both answer `Runtimes` afterwards.

## 3. The dependency step (`Prepare`)

`Prepare(ctx, site, releaseDir)`, called by ops/release after a release of a managed app is
published (its SPEC §3 step 8) and by provisioning: one transient service
`cg-app-prep-<user>-<ns>.service`, the site user, the site's slice, `phpfpm.RunSandbox`, the
working directory the release, 20 minutes (`RuntimeMaxSec`), an environment of its own (`PATH` with
the runtime's `bin` first, `HOME=/tmp`, `LANG=C.UTF-8`, `npm_config_update_notifier=false`), the
script of internal/runtimes SPEC §4 on `/bin/sh -eu`. Output on one stream, the last 4 KiB kept. A
non-zero exit is `invalid`: `installing the app's dependencies failed: <output>`. A runtime that is
not installed is `failed_precondition`-like `invalid`: `Node.js 22 is not installed on this server`.
The network is open to it (npm and PyPI); the sandbox is the site's.

## 4. Tests

`runtimes_test.go`: `Runtimes` over a fake tree; the transient unit's argv for both installs (props,
the subcommand, no network path but nodejs.org); the unpacker against a tarball with an absolute
path, `..`, an escaping link and a device (each refused), and a good one (links kept inside, modes
without set-id); the install main against an `httptest` server standing for nodejs.org, a digest
that does not match refused and nothing left; `Prepare`'s service argv (user, slice, sandbox,
limits, environment, script) and its failure message.
