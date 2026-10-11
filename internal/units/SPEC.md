# units — the systemd units of a release

- The unit files live here and nowhere else: `cloudground-agent.service`, `cloudground-api.socket`,
  `cloudground-api.service` (`Names`, in install order). `//go:embed` compiles them into every
  binary that imports the package; `Get(name)` returns one, and nothing for a name outside `Names`.
- The installer (`cgctl install`) writes them from the binary it runs, so a server never mixes the
  units of one release with the binaries of another.
- The release build (`scripts/build-release.sh`) copies the same files beside the binaries, and the
  signed `SHA256SUMS` lists them: the self-update (`internal/agent/ops/panel`, SPEC §2) installs a
  unit only when its signed checksum matches.
- Every path a unit names in `ReadWritePaths=`, `ReadOnlyPaths=`, `BindPaths=`,
  `BindReadOnlyPaths=`, `InaccessiblePaths=`, `ExecPaths=`, `NoExecPaths=` or
  `TemporaryFileSystem=` without the `-` prefix must exist when the unit starts, or systemd fails
  it at the NAMESPACE step (`status=226/NAMESPACE`) and `Restart=` loops it for ever.
  `RequiredPaths(lines)` lists those paths (a `+` prefix and a `:` suffix stripped); the
  installer's tests hold each one to the installer's directories or to the paths every Ubuntu
  image has (`internal/install` SPEC §3 step 10), and the e2e and the container install test check
  them on a real system.
