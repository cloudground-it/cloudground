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
| `CloudflareRangesParams` | `cfips.Parse` takes `V4` and `V6` (internal/cfips SPEC) | its message |
| `TrafficCollectParams` | at most 4096 sites; each a positive `ID` and a valid domain, no `ID` twice | `too many sites in one traffic read`, `site <id>: …`, `site <id> is listed twice` |
| `TrafficStatsParams` | a positive `ID`, a valid domain, `Days` 1–90 | `invalid site id`, the domain's message, `days must be 1 to 90` |
| `RuntimeInstallParams` | `Runtime` is `node` or `python`, `Version` of its form (internal/runtimes SPEC §1) | `unknown runtime "<r>"`, `invalid <runtime> version "<v>"` |
| `AppEnvParams` | the site is `node` or `python`; `runtimes.CheckEnv` takes `Env` | `not a Node.js or Python site`, its message |
| `AppControlParams` | `Action` is `start`, `stop` or `restart` | `unknown action "<a>"` |
| `SetFunctionParams` | `functions.Check` takes the function, its id above 0; `functions.CheckFiles` takes `Files` when given; `runtimes.CheckEnv` takes `Env` | its message |
| `FunctionRef`, `FunctionRollbackParams` | an id above 0, a function name; a release ID when one is named | its message |
| `InvokeFunctionParams` | a function name; `Source` `schedule` or `test`; `Method` empty or `GET POST PUT PATCH DELETE`; `Path` empty or starting with `/`, at most 2 KiB, no space or control character; a body at most 64 KiB | its message |
| `FunctionsSummaryParams` | at most 100 names, each a function name | its message |
| `RenderFunctionsParams` | at most 100 functions, each with an id above 0, a function name, a timeout 1–300, a token empty or of its form, valid origins and domains; the domain empty or valid (internal/engine SPEC §8) | its message |
| `FunctionsCertParams` | `Name` `cloudground-functions` or `cloudground-fn-<id>`; each domain valid; for an issue at least one | its message |
| `MigrateFunctionParams` | `OldName` and the function as `SetFunctionParams` | its message |
| `ManageFileParams` | `Operation` is `mkdir`, `rename` or `delete` | `unsupported file operation "<o>"` |
| | `rename` has a non-blank `DestPath` | `destination path required` |
| `SSHKeyParams` | `Action` is `list`, `add` or `delete` | `unsupported SSH key action "<a>"` |
| `DatabaseParams` | `Name` is `site_<id>` (`site.ValidDatabaseName`: `^site_[1-9][0-9]{0,18}$`) and `User` the same (SR-R1) | `a site's database and account are site_<id>, the same name (got "<n>"/"<u>")` |
| `BackupParams` | `Kind` is empty (full), `full`, `files` or `database` | `backup kind must be full, files, or database` |
| `RestoreParams` | `Mode`, trimmed and lower-cased, is empty (full), `full`, `files` or `database` | `restore mode must be full, files, or database` |
| `ForgetSnapshotParams` | `SnapshotID` is 8 to 64 hex digits | `invalid snapshot id` |
| `RepositoryInfoParams` | at most 100 000 `Expect` IDs, each 8 to 64 hex digits (#192) | `too many snapshot ids`, `invalid snapshot id` |
| `ForeignSnapshotParams` | `SnapshotID` is 8 to 64 hex digits (#193) | `invalid snapshot id` |
| `RestoreParams` with `Foreign` (#193) | a full restore (no other mode, no `Paths`, no `Databases`); `Hostname` 1 to 253 characters, no space, slash or control character; at most 64 `Databases`, each name on both sides a database name (`site.ValidDBName`); no line break in `DBPassword` or `ConnectorToken` | `a restore from another server is a full restore`, `a restore from another server restores everything`, `invalid hostname of the other server`, `too many databases`, `invalid database name`, `invalid site credentials` |
| `CopyRepositoryParams` | `From` and `To` each have a repository and a password, and are not the same repository (#192) | `repository and password required`, `the two repositories are the same` |
| `RemoveReleasesParams` | at most 100 `ReleaseIDs`, each a release ID (never `initial`) | `at most 100 releases at a time`, `invalid release id "<id>"` |
| `SiteDatabaseParams`, `DBUserParams` | `ServerID`, when not 0, is positive | `invalid database server` |
| `DBServerParams` (`TestDBServer`) | `Kind` empty, `mariadb` or `postgres` (PostgreSQL needs `TLS`); `Host` a host name or an address (at most 253 characters, none of space, `/`, `@`), `Port` 1–65535, `AdminUser` one line of at most 80 characters, `AdminPassword` not empty and without a line break, `CACert` empty or PEM certificates, at most 64 KiB | the rule's sentence |
| `SetDBServersParams` | every server as `DBServerParams`, with a positive `ID`, at most 50 | the rule's sentence |
| `GitSyncParams` | `ReleaseID` a release ID, never `initial`; `Repo`, `Branch` and `BuildCommand` the rules of `site.CheckGitRepo` (the site's root the only place a `file://` repository may be), `CheckGitBranch`, `CheckGitBuildCommand` (internal/api SPEC §15.1); `DeployKey`, when set, exactly one OpenSSH private key block of at most 4 096 bytes | `invalid release id "<id>"`, the check's own sentence, `the deploy key is not one OpenSSH private key` |
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
