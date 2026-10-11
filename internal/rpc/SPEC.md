# rpc — the agent's parameter and result types

What `panel-api` sends to `panel-agent` and what it gets back, one Go type per JSON document.
The transport, the method table and the errors are `internal/agentrpc` (its SPEC.md); this
package holds only data, and the rules a value must meet before the agent acts on it.

Written before the code from the method table (`internal/agentrpc/methods.go`), the agent's
handlers (`internal/agent/ops/*`, each domain's SPEC.md), the API's call sites and the golden
files in `testdata/wire`, which were captured from the types this package replaced.

## 1. The wire format is fixed

- Every type marshals with `encoding/json`. The JSON names, the `omitempty` choices, the Go
  types behind each value (string, number, boolean, array, object, RFC 3339 time, base64 bytes)
  and which fields exist are the format. The order of fields in an object is not.
- `testdata/wire/<Method>.json` holds, for every method of the table, the JSON of its parameters
  and of its result, as the zero value and with every field set (`TestWireGolden`). A site
  (`state.Site`) is written as the placeholder `"@state.Site"`: its format belongs to
  `internal/state`. A test fails when a method has no file, when a file names no method, and
  when the JSON differs.
- A deliberate change of the format regenerates the files (`CG_WIRE_UPDATE=1`) and says so in
  the commit. The API and the agent ship together, but the format is kept stable anyway: an
  agent left running across an API upgrade (or the reverse, during a self-update) still talks.

## 2. Sites

A parameter type that carries sites implements `Sites() []*state.Site` on its pointer
(`agentrpc.SiteCarrier`), listing every site it holds, in order, with `nil` for an optional site
not given. The first one is the site the server locks. The server validates each one and writes
the validated value back through the pointer; handlers never validate a site themselves.

| type | sites |
| --- | --- |
| every type with a `Site` field | that site |
| `DeployParams` | `Site`, then `SourceSite` (optional) |
| `StagingParams`, `SyncStagingDBParams` | `Production`, then `Staging` |

## 3. Validation

A parameter type may implement `Validate() error`. The agent's server calls it after the sites
are validated and before the lock and the handler (agentrpc SPEC §3), and turns an error into
`invalid` with the error's text as the message. That text is therefore written to be shown to a
user: it names the field and the allowed values, never a path or a command.

`Validate` holds only rules about the value itself, and only rules the agent already applied
in its handlers (which keep their own checks, and are called directly by tests). Anything that
needs the machine (is that PHP version installed, does that release exist) stays in the
handler.

| type | rule | message |
| --- | --- | --- |
| `FirewallRuleParams` | `Action` is `allow`, `deny` or `delete` | `invalid action "<a>"` |
| `FirewallEnableParams` | `Expect` is not empty | `expect is required: the rules shown before enabling` |
| | `Port` is 1..65535 | `invalid port <n>` |
| | `From`, when set, is an IP address or a CIDR | `invalid source address "<f>"` |
| `BlocklistApplyParams` | at most 100,000 entries in each list | `at most 100000 entries in each list` |
| | every entry an address or a CIDR (internal/blocklist SPEC §1) | `"<s>" is not an IP address or a network in CIDR form` |
| | no block wider than /8 (IPv4) or /16 (IPv6) | `<p> is wider than /8 (IPv4) or /16 (IPv6): too broad to block` |
| `ManageFileParams` | `Operation` is `mkdir`, `rename` or `delete` | `unsupported file operation "<o>"` |
| | `rename` has a non-blank `DestPath` | `destination path required` |
| `SSHKeyParams` | `Action` is `list`, `add` or `delete` | `unsupported SSH key action "<a>"` |
| `DatabaseParams` | `Name` is `site_<id>` (`site.ValidDatabaseName`: `^site_[1-9][0-9]{0,18}$`) and `User` the same (SR-R1) | `a site's database and account are site_<id>, the same name (got "<n>"/"<u>")` |
| `BackupParams` | `Kind` is empty (full), `full`, `files` or `database` | `backup kind must be full, files, or database` |
| `RestoreParams` | `Mode`, trimmed and lower-cased, is empty (full), `full`, `files` or `database` | `restore mode must be full, files, or database` |
| `ForgetSnapshotParams` | `SnapshotID` is 8 to 64 hex digits | `invalid snapshot id` |
| `RemoveReleasesParams` | at most 100 `ReleaseIDs`, each a release ID (never `initial`) | `at most 100 releases at a time`, `invalid release id "<id>"` |
| `DeployParams` | `ReleaseID` is a release ID (`site.ValidReleaseID`: a canonical upper-case ULID, or one of the two older forms) | `invalid release id "<id>"` |
| `ReleaseParams` | `ReleaseID` is empty (rollback to the previous), `initial` or a release ID | `invalid release id "<id>"` |
| `MigrationParams` | `ReleaseID` is a release ID | `invalid migration release id` |
| `DBStudioExportParams` | `Format` is `sql` or `csv`; `csv` names exactly one table | `the format is sql or csv`, `a CSV export is of one table` |
| `DBStudioDeleteParams` | `Dir` is `exports` or `imports` | `the folder is exports or imports` |
| `DBStudioAlterParams` | `add_column` with a `Column`, or `add_index` with an `Index` | `give add_column with a column, or add_index with an index` |

## 4. Capabilities

`Capabilities` (no parameters, read-only, 10 s) is the machine as the agent finds it, so that the
API holds policies only and never a copy of a fact about the host:

| field | what | from |
| --- | --- | --- |
| `agent_version` | the agent's build | `internal/version` |
| `started_at` | when this agent process started; a new value means the agent restarted | process start |
| `php_versions` | the PHP versions a site can run, newest first; `[]` when none is found | a regular, executable `php-fpm<ver>` in the agent's PHP binary directory, `<ver>` being `8.N` (AGENTS.md: never `/etc/php/<ver>`) |
| `services` | each managed service: `name`, systemd `unit`, `listed` (a row of the services page), `restartable` (`RestartService` accepts it) | the agent's service table (ops/system) |
| `protected_tables` | the rules that keep a staging push off live data: `match` (`suffix` or `contains`), `pattern`, `reason`, `stores_only` (applies only when production is a store) | the staging push policy (ops/staging) |
| `paths.sites_root` | where sites live, `<sites_root>/<domain>` being each site's root | the agent's layout |
| `paths.log_root` | each site's logs, `<log_root>/<domain>/` | the agent's layout |
| `paths.mysql_socket` | the local MariaDB server's Unix socket (a new site's summary shows it, internal/api SPEC §3.2); omitted when unknown | the agent's layout |
| `features.http3` | nginx serves HTTP/3 (the build has `http_v3_module`) | `nginx -V`, read once per agent process |
| `restic_version` | restic's own version line, e.g. `restic 0.19.1 compiled with go1.24.4 on linux/amd64`; empty when restic cannot be run | `restic version` (30 s), read on every call |
| `restic_supported` | restic is 0.17 or later, the first with typed exit codes (a locked repository, a wrong password); `false` with an empty version | the same line |

The PHP list is read on every call (a version installed with apt shows up without an agent
restart). The nginx probe is the renderer's, read once per process, as a build cannot change
without nginx and the agent restarting.

## 5. Layout

One file per domain, each type documented where it is declared:

| file | domain |
| --- | --- |
| `doc.go` | the package |
| `sites.go` | create, apply, delete, managed files |
| `certs.go` | site certificates, the panel's certificate, certificate status |
| `releases.go` | deploys, promote, rollback, migrations |
| `staging.go` | staging copies, diff, tables, database push |
| `backups.go` | backups, restores, restore tests, retention, repository test |
| `cache.go` | page cache purge and warm, object-cache statistics |
| `system.go` | status, services, logs, firewall, drift, self-update |
| `capabilities.go` | what the machine offers (§4) |
| `cron.go` | cron jobs |
| `databases.go` | databases, the SQL console, export, import |
| `files.go` | the file manager, SFTP and SSH keys |
| `wordpress.go` | wp-cli: login links, plugins and themes, updates, object cache |
| `dbstudio.go` | Quarry |
