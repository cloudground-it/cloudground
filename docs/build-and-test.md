# Build and test

*How-to.* Build CloudGround, run the checks every commit must pass, and regenerate the files that
are generated from code.

## You need

- **Go 1.26** and **Node 20 or later**, or
- **Docker** only: every check has a `docker-*` target that runs in `golang:1.26` (and `node:22`
  for the UI). This is the way on macOS, and on any machine without Go.

## Build

| Command | What it does |
| --- | --- |
| `make build` | builds the UI (`make ui`), then `panel-api`, `panel-agent` and `cgctl` into `bin/` |
| `make ui` | `npm ci` and `npm run build` in `ui/`; the output is `ui/dist/` |
| `make dist` | the UI, then the binaries (`panel-api`, `panel-agent`, `cgctl`, `cg-site-shell`) for linux/amd64 into `dist/` |
| `make docker-dist` | the same, in containers |
| `make clean` | removes `bin/`, `dist/` and `ui/dist/` |

`ui/dist/` is committed, because `panel-api` embeds it with `go:embed` and must build without
npm. After any change in `ui/app/` or `ui/i18n/`, run `make ui` (or `make docker-dist`) and commit
the new `ui/dist/` with it. CI fails when the committed build differs from a fresh one.

The build stamps the version (`git describe`) into the binaries. A release build also sets
`UPDATE_URL` and `UPDATE_PUBKEY`, the only origin and signing key a self-update accepts
(internal/version/SPEC.md); a build without both cannot self-update.

## The gate: `make docker-test`

Every commit passes this, with no exceptions:

```sh
make docker-test
```

It runs, in `golang:1.26`:

1. `gofmt -l internal cmd e2e`: any file listed fails the run;
2. `go vet ./...` and `go vet -tags e2e ./e2e/...`, so the e2e suite keeps compiling;
3. `go test -race -timeout 30m ./...`.

With Go installed, `make vet` and `make test` run the vet steps and the tests without `-race`. The
Docker run uses the host's own Linux architecture on purpose: under amd64 emulation on an arm64
machine `openat2` is missing, and the `sitefs` confinement tests fail for reasons that have nothing
to do with the code.

The UI has its own test, run by CI: `npm test` in `ui/` checks that every locale has exactly the
keys and placeholders of `en.json` and compiles.

## Generated files

Some committed files are generated, and a test fails while they are stale. Never edit them by hand.

| File | Generated from | Regenerate with | Stale check |
| --- | --- | --- | --- |
| `docs/api.md` | `internal/api/routes.go` and `internal/api/apidoc.md.tmpl` | `make api-doc` or `make docker-api-doc` | `TestAPIDocIsCurrent` |
| `e2e/sections.index` | the e2e sections and the functions they reach | `make e2e-index` or `make docker-e2e-index` | `TestSectionsIndex` |
| `internal/rpc/testdata/wire/*.json` | the agent's parameter and result types | `CG_WIRE_UPDATE=1 go test ./internal/rpc` | `TestWireGolden` |
| `testdata/*.golden` of `internal/phpfpm`, `internal/velocity`, `internal/tune`, `internal/sysprobe`, `internal/version` | the rendering or sizing code | `CG_GOLDEN_UPDATE=1 go test ./<package>` | each package's golden test |
| `internal/engine/nginx/testdata/matrix` | the nginx renderer | `go test ./internal/engine/nginx -update-matrix` | `TestRenderMatrix` |
| `internal/agent/ops/sites/testdata/golden` | the agent's site renders | `go test ./internal/agent/ops/sites -update-golden` | the package's golden test |
| `ui/dist/` | `ui/` | `make ui` | CI |

A golden file changes only when the output is meant to change. List the change in the commit.

## Other checks

| Command | When |
| --- | --- |
| `make docker-nginx-test` | after a change to the nginx renderer: runs `nginx -t` on the whole render matrix with the nginx of Ubuntu 24.04 and 26.04 (internal/engine/nginx/SPEC.md §6) |
| `make docker-install-test` | after a change to the installer, the bootstrap or the uninstall: in Ubuntu 24.04 and 26.04 containers (systemd not running, apt skipped), the bootstrap's refusals, `cgctl install --plan`, `cgctl install`, `doctor`, nginx and sshd answering, then `cgctl uninstall` leaving the machine's file list as it was before the install, and a second install (internal/install/SPEC.md §8) |
| `make docker-upgrade-bundle` | the signed throwaway releases the e2e section `R-6.5 upgrade` updates through ([Run the end-to-end suite](./e2e.md)) |
| `go test -tags hostintegration -run Integration ./internal/agent/host/` | the host adapters against real tools, in a throwaway container only (`CG_TEST_MYSQL_SOCKET` for MariaDB) |
| `go test ./connector` | the connector's PHP tests, with a local `php` or a `php:8.4-cli` container (`CG_PHP_IMAGE`) |
| `CG_UIDEV_ADDR=0.0.0.0:5252 CG_UIDEV_UI=ui/dist go test -tags uidev -run TestUIDevServer -timeout 0 ./internal/api/` | a local panel with a fake agent, to look at the UI; sign in as `admin@example.com` / `dev-password-123` |

## What a test cannot tell you

A passing unit test is not evidence that the product works on a server. Response headers, the
negotiated protocol, sockets and system calls are checked on a real box by the end-to-end suite.

## Next step

[Run the end-to-end suite](./e2e.md) for the sections your change affects.
