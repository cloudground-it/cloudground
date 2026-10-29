# Notes for coding agents

CloudGround is a control panel for one Ubuntu server, built for speed: one command installs it,
and a site created in it starts cached, tuned, isolated and backed up off the server with restores
that are tested. Go (`go 1.26`, module `github.com/cloudground-it/cloudground`) with a Nuxt 4
interface. `README.md` is the public description, `docs/` the developer documentation; the user
documentation is the separate `cloudground-docs` repository.

## Status

CloudGround is at 1.x: the API, `cgctl` and the MCP tools stay compatible within a major version
(`docs/api.md`, "Stability"), and so do updates and backups (`internal/install/SPEC.md` §11): a
change that removes or renames something they offer waits for 2.0. The code is source-available under the Elastic License 2.0
(`Elastic-2.0`, `LICENSE`): use, modify and self-host, including for client sites, but not as a
hosted or managed service for others. Never call it "open source" or "free software". The PHP under
`connector/` is GPL-2.0-or-later (`connector/LICENSE`, required for WordPress plugins).
A package's `SPEC.md` is its contract: write or change the spec first, then the code, and say in
the commit which section it implements.

- There have been two internal security reviews and no external audit (`SECURITY.md`): treat
  every change to the privilege boundary as security work.
- Benchmark results are published only once measured on a CloudGround release (`bench/`).

## The habit that matters most

**Check it on a server; the diff is not evidence.** Bug after bug here read as correct and was
wrong on a real machine:

- nginx inherits `add_header` only into a block that declares none of its own, so the security
  headers were missing from real responses for months;
- the HTTP/3 probe read the standard output of `nginx -V`, which writes to standard error, so the
  feature never turned on;
- the `woocommerce` site type did not install WooCommerce;
- kernel TLS, taken up because it looked like a sure win, cost **28 %** of cached throughput.

Look at headers, the negotiated protocol, the listening socket, the system calls. Add a check to
`e2e/` (Go, build tag `e2e`) instead of trusting a reading of the code, and make sure the check
asserts on the product and not on what the test set up itself: that slip has happened twice.

## Commands

```sh
make build     # the UI, then the binaries into bin/
make ui        # only the interface
make dist      # linux/amd64 binaries into dist/
make test
make vet
```

Without Go on the machine (macOS), the container targets do the same: `make docker-test`,
`make docker-dist`, `make docker-e2e-bin`, `make docker-e2e-index`, `make docker-api-doc`,
`make docker-nginx-test`, `make docker-install-test` (`docs/build-and-test.md`).

Where things are:

- `cmd/`: `panel-api`, `panel-agent`, `cgctl`, `cgbench`;
- `internal/`: the logic, with a `SPEC.md` in each package that has a contract
  (`docs/spec-convention.md`);
- `ui/`: the Nuxt 4 + Nuxt UI single-page app; its build `ui/dist/` is committed; the design
  tokens in `ui/app/assets/css/main.css` and their mapping onto Nuxt UI in `ui/app/app.config.ts`
  (the design system, DESIGN.md, is kept outside the repository), translations and
  `GLOSSARY.md` in `ui/i18n/`;
- `installer/`: the bootstrap script, which hands over to `cgctl install` (`internal/install`);
- `connector/`: the WordPress mu-plugin and object-cache drop-in (`connector/SPEC.md`);
- `bench/`: the benchmarks; `scripts/`: the tooling; `docs/`: developer documentation.

A real server, end to end:

```sh
make docker-e2e-bin && scp dist/cg-e2e root@<public-ip>:/root/ && ssh root@<public-ip> ./cg-e2e -test.v
# credentials: /root/cloudground-admin.env, or PANEL_EMAIL / PANEL_PW; the address is detected
```

`docs/e2e.md` explains running only the sections a change reaches.

## Hard rules

1. **Never run the installer or the connector on a server you care about.** They take over the
   system's configuration; use a VM or container you can throw away.
2. **A local run never talks to a production server or its database.**
3. **Commit only what you were asked for, and never push without an explicit go**: one commit per
   coherent change, with the reason in its message. When in doubt, stage the change, show the
   diff and wait.
4. **Never weaken backup or restore verification to get a test through.** A backup that was not
   verified does not count; verified restores are a core promise.
5. **Performance is the claim, and `bench/` checks it.** A change to caching, request handling or
   tuning defaults comes with benchmark numbers from before and after.
6. **Defaults are measured A/B, one setting at a time**, and a gain that does not repeat is not
   shipped. `internal/install/SPEC.md` §4 lists what was tried and dropped: extend it rather than
   quietly bringing a setting back.
7. **`make docker-test` is green on every commit**: gofmt, `go vet` (also `-tags e2e`),
   `go test -race`.
8. **A fix or a new behaviour gets an e2e check** that fails on the build before it, and unit
   tests. Register a new section in the `sections` table of `e2e/e2e_test.go`, in
   `e2e/SECTIONS.md` and in `e2e/sections.map`, then run `make docker-e2e-index`;
   `e2e/sections_test.go` enforces all four.

## Pitfalls

- **The connector is PHP only.** The mu-plugin and the APCu drop-in are the files under
  `connector/`, embedded by package `connector` with `//go:embed`; change the PHP and rebuild.
- **wp-cli and PHP-FPM do not share APCu.** A flush from the command line cannot reach FPM's
  segment, and an FPM reload (SIGUSR2) keeps the master and its APCu. A refresh runs
  `wp cache flush --skip-plugins --skip-themes`, and the drop-in's generation file carries the flush
  into PHP-FPM (connector/SPEC.md §3.4).
- **Ubuntu 26.04's coreutils are uutils 0.8, and they treat symlinks differently from GNU.**
  Measured: `chown -h user link` changes the owner of the target, and `chown -R user link` follows a
  symlink given as its argument. A root `chown` on a path a site can swap for a link is a way to
  root there. So the agent never shells out for owners, modes or copies inside site trees: it uses
  `internal/agent/sitefs` (`openat2` with `RESOLVE_BENEATH`; `Root.Chown`, `Chmod`, `ChownTree`,
  `CopyTree`), and a site's layout links are relative. GNU's tools stay installed as `gnuchown`,
  `gnucp` and so on for scripts that need them.
- **An installed PHP version is detected by its FPM binary** (`/usr/sbin/php-fpm<ver>`), never by
  `/etc/php/<ver>/`, which rendering a pool creates: one failed provision would otherwise leave a
  version that looks installed for ever.
- **Every string a user sees goes through i18n** in en, it, fr, es and de, using the terms of
  `ui/i18n/GLOSSARY.md`; the locale key test must stay green.
