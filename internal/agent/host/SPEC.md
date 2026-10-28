# `internal/agent/host` — OS adapters (specification)

Every place the agent touches the operating system goes through one adapter
here. Each adapter is an **interface**, a **real implementation** and a
**fake** for tests. The agent's handlers depend on the interface only.

This file is the contract. The code is written from it; the tests check it.

## Common rules

1. **Every call takes a `context.Context`.** Cancelling it stops the work: a
   child process is killed with its whole process group, a D-Bus wait returns.
   An adapter adds its own ceiling only where the spec below says so.
2. **Errors are typed and classified from structured signals only**: exit
   codes, D-Bus error names, driver error numbers, state read back from the
   system's own databases. **Never from the text of stderr.** Stderr is kept
   for the operator (in `ExitError.Stderr`) and never inspected for meaning.
   Callers test errors with `errors.Is` / `errors.As`.
3. **Secrets never go in argv** (world-readable in `/proc/<pid>/cmdline`).
   They travel on stdin, in a 0600 root-owned file, or in the environment of
   a root process (`/proc/<pid>/environ` is readable by its owner only).
4. **Names are validated before they reach a tool.** An identifier that could
   be read as an option (a leading `-`) or that the tool would refuse is
   rejected with `ErrInvalidName` and no process is started.
5. **No sleeps as synchronisation.** Waiting is on a job result (systemd), a
   process generation (nginx) or a process exit.
6. **Fakes are faithful about errors.** A fake returns the same typed errors
   in the same situations as the real implementation, so a handler's error
   path is testable without root.

## exec — the foundation

```go
type Cmd struct {
    Name    string
    Args    []string
    Env     []string      // nil: inherit the agent's environment; non-nil: exactly this
    Dir     string
    Stdin   io.Reader
    Stdout  io.Writer     // nil: captured into Result.Stdout
    Timeout time.Duration // 0: the runner's default (10 min)
    Interrupt time.Duration // 0: killed at once when cancelled
}
type Exec interface { Run(ctx, Cmd) (Result, error) }
```

- `OSExec` starts the process in its own process group; on cancellation or
  timeout the **whole group** is killed (SIGKILL), and pipes are abandoned
  after a short grace (`WaitDelay`, 5 s) so a grandchild holding stdout cannot
  hang the agent.
- **Interrupt.** With `Interrupt` set, a cancellation or timeout first sends
  **SIGINT to the process alone** (not its group: a helper it talks to, an
  SFTP backend's `ssh`, must stay up while it cleans up). The group is killed
  (SIGKILL) as soon as the process has exited, or when `Interrupt` has passed
  if it has not; `WaitDelay` is 5 s plus `Interrupt`. The error is still the
  context's (`Canceled` / `DeadlineExceeded`), whatever the process exited
  with.
- **The group is signalled only while its leader is unreaped.** `Run`
  starts the process, waits for its exit with `waitid(P_PID, WEXITED |
  WNOWAIT)`, which leaves it a zombie, kills what is left of the group then
  if the command was cancelled, and only then reaps it (`Wait`). A zombie
  keeps its pid, and so the group's id, reserved: a kill can never reach a
  later process that was given the same number. The grace timer and the
  cancellation do nothing once the leader has exited.
- Stderr is always captured separately (the last 64 KiB are kept).
- **Stdout limit.** A captured stdout (`Cmd.Stdout` nil) keeps at most
  `StdoutLimit` (16 MiB): the commands read this way print kilobytes, and a
  child that prints without end (site code under wp-cli) must not grow the
  agent's heap. The first byte past it stops the command the way a
  cancellation does (SIGKILL to the group, or SIGINT first with
  `Interrupt`), and the call fails with `ErrStdoutTooLarge`, with
  `Result.Stdout` holding exactly the first `StdoutLimit` bytes. A streamed
  stdout (`Cmd.Stdout` set) is the caller's to bound; `NewTailBuffer(n)` is
  a writer that keeps the last `n` bytes. `FakeExec` fails a reply past the
  limit the same way.
- A non-zero exit is an `*ExitError{Name, Code, Stderr}`. `Code` is the exit
  status, or `-1` when the process was killed by a signal. `host.ExitCode(err)`
  extracts it.
- A missing binary wraps `exec.ErrNotFound`.
- A timeout or cancellation wraps `context.DeadlineExceeded` /
  `context.Canceled` (testable with `errors.Is`), never a bare "signal: killed".
- **Fake:** `FakeExec` records every call (name, args, stdin, env) and answers
  through a `Respond` function; a non-zero code becomes a real `*ExitError`.

## systemd — over D-Bus

Interface `Systemd`:

| Method | Semantics |
|---|---|
| `Start/Stop/Restart/Reload/ReloadOrRestart(ctx, unit)` | Enqueue the job (mode `replace`) and **wait for its completion signal** (`JobRemoved`). `done` and `skipped` are success; any other result (`failed`, `timeout`, `canceled`, `dependency`, …) is a `*JobError{Op, Unit, Result}` that matches `ErrJobFailed`. |
| `Enable/Disable(ctx, units...)` | `EnableUnitFiles`/`DisableUnitFiles` (persistent, `force` on enable), then a daemon reload, as `systemctl enable` does. Does not start or stop. |
| `DaemonReload(ctx)` | `Manager.Reload`. |
| `ResetFailed(ctx, unit)` | `ResetFailedUnit`. |
| `UnitProps(ctx, unit)` | `LoadState, ActiveState, SubState, UnitFileState, ActiveEnter` from the Unit interface; `MainPID, MemoryCurrent` from the Service interface for `.service` units. `MemoryCurrent` is reported only when systemd knows it (`MemoryKnown`; systemd's "unset" is `2^64-1`). `ActiveEnter` is zero when the unit never started. A unit that does not exist is **not** an error: `LoadState == "not-found"` (`Exists()` is false). |
| `ListUnits(ctx, patterns...)` | Every loaded unit matching the glob patterns, in any state (`systemctl list-units --all`). |
| `StartTransient(ctx, unit, description, argv)` | `StartTransientUnit` of a service with `Type=exec` and `CollectMode=inactive-or-failed`, waiting for the start job (with `Type=exec`, that is the program executing). The unit must be a `.service` name and `argv[0]` absolute, else `ErrInvalidArgument` before D-Bus is asked. A name systemd still has loaded is `ErrUnitExists` (D-Bus `UnitExists`). The unit runs outside the caller's cgroup and sandbox, so it outlives the caller's restart. |
| `RunTransient(ctx, TransientRun)` | Runs a command **to completion** as an account: a transient service with `Type=exec`, `User=`, `WorkingDirectory=`, the given `Environment=` (nothing of the agent's), `RuntimeMaxSec=` (required), `TimeoutStopSec=10s`, stdin `/dev/null`, `CollectMode=inactive`. stdout and stderr go to a pipe whose write end is handed to systemd as a file descriptor (`StandardOutputFileDescriptor`, what `systemd-run --pipe` does); the agent copies the read end to `Output` until EOF, then reads the unit's end: a unit already collected ended cleanly (`success`); a failed one gives systemd's `Result` and `ExecMainStatus`, and is reset. When the main process exits or the limit runs out, systemd stops the unit and kills **every process in its cgroup** (measured: a `setsid` child and a double-forked one both go). Cancelling `ctx` stops the unit. A name still running is `ErrUnitExists`; a failed leftover of an earlier run is reset and replaced; a run that cannot start (no such user or directory) is the start job's `JobError`, with the unit reset. ExecStart is sent with systemd's "ignore failure" flag **off**: go-systemd's `PropExecStart(argv, true)` sets it, and an `exit 3` then ended "Deactivated successfully" (measured on systemd 255 and 259). Verified against systemd 255 (Ubuntu 24.04) and 259 (26.04) in containers booted with systemd (`transient_integration_test.go`). |

- A bare name gets `.service` appended (`nginx` → `nginx.service`), as
  `systemctl` does.
- D-Bus error `org.freedesktop.systemd1.NoSuchUnit` (and `LoadFailed`) map to
  `ErrNoSuchUnit`.
- **Connection:** one lazily dialled connection shared by every caller. As
  root it uses systemd's private socket `/run/systemd/private` (what
  `systemctl` itself uses as root; no dbus-daemon or polkit involved),
  falling back to the system bus. go-systemd's connection is two sockets:
  one for calls, one for the signals that carry job results (`JobRemoved`).
  - It is dialled with **no caller's context**, bounded by its own 15 s dial
    timeout. godbus closes a connection when the context it was dialled with
    ends, and go-systemd's `New…ConnectionContext` pass theirs down.
  - It is **counted while in use** and never closed under a caller: a dead
    or replaced connection is retired, and closed by its last user.
  - It is **replaced when it dies** (either socket closed: systemd re-exec,
    dbus restart): one dial at a time, and a connection dead as soon as it
    is made is an error, not a dial loop.
  - A caller whose connection dies under it — in a call, or waiting for a
    job result, which can then never come — fails **at once** with
    `ErrConnLost` (the job may or may not have run). `Start`, `Stop`,
    `Restart`, `Reload`, `ReloadOrRestart`, `Enable`, `Disable`,
    `DaemonReload`, `ResetFailed` and the reads run **once more** on a fresh
    connection; `StartTransient`/`RunTransient` do not (a second start runs
    the program twice).
- **One job per unit at a time.** The adapter's jobs on one unit wait for
  each other (bounded by the same ceiling). systemd merges a job into one
  already queued for the unit and returns the *same job path*; go-systemd
  keeps one listener per path, so the earlier waiter's channel was replaced
  and its result never came. Measured on systemd 255 and 259 (the storm
  below, before this lock): 35 of 192 calls failed, 10 of them waits with no
  result after the full 60 s ceiling while the unit was `active/running` (or
  `failed`) and the connection up; 8 were `canceled` (a `replace` job
  cancelling another caller's) and 17 `failed` (the test units' start rate
  limit, which the test now turns off). After: 192 of 192 in 4.5 s (255) and
  4.6 s (259), on one connection.
- **Ceiling:** every call and job wait is bounded at `DefaultJobTimeout`
  (2 minutes; `DBusSystemd.JobTimeout`, or `WithJobTimeout(ctx, d)` per call
  for a job known to be long, such as a MariaDB restart), whatever the
  context's own deadline: the earlier of the two applies. A job wait that
  runs out is a `*JobTimeoutError` (`ErrJobTimeout`, and
  `context.DeadlineExceeded`) carrying the unit's state read when it gave up
  (`State`, e.g. `activating/start`), so the caller can tell a slow start from
  a dead one; systemd goes on with the job.
- **Why (measured, R-3.7 storm on the test VPS, 2026-10-09).** Concurrent
  creates and deletes deadlocked the agent: a `Provision` waited 22 minutes
  in `job` ← `Restart` ← `startPHP` holding the nginx lock, and a
  `DeleteSite` failed with `systemd daemon-reload: read unix
  @->/run/systemd/private: use of closed network connection`. *Use of closed
  network connection* is Go's error for a socket closed **by this process**,
  not by its peer. The adapter dialled its shared connection with the
  context of the first request that needed it; the RPC server cancels each
  request's context when its handler returns; godbus then closed both
  sockets, failing the calls in flight (the daemon-reload) and losing every
  job result still awaited (the restart, whose wait had only the request's
  45-minute deadline). The next caller saw `Connected() == false` and
  re-dialled — with *its* request's context. Sequential requests hid it
  (each one re-dialled and finished before it was cancelled); concurrency
  exposed it. Measured in containers booted with systemd
  (`systemd_conn_integration_test.go`), on systemd 255 (Ubuntu 24.04) and
  259 (26.04):
  - a connection dialled with a context is closed when it is cancelled, and
    a start job enqueued on it never reports, though systemd ran it;
  - **daemon-reload does not drop private-socket clients**: 20 adapter
    reloads interleaved with 20 `systemctl daemon-reload`, then a restart,
    all on the first connection;
  - **daemon-reexec does** drop them; the adapter re-dials and the next
    restart succeeds;
  - 16 callers × 12 rounds of restart / stop / reload-or-restart /
    daemon-reload (adapter and `systemctl`), each with a request context
    cancelled right after its call: every call succeeds, on one connection.
- **Sandbox requirements** (`cloudground-agent.service`): `AF_UNIX` in
  `RestrictAddressFamilies` and `/run` reachable. Both hold today.
- **Fake:** `FakeSystemd` keeps a map of units with their properties, applies
  start/stop/restart to `ActiveState`, records every operation, and can be told
  to fail a job (`FailJob[unit] = "failed"`) or to report a unit missing.
  `RunTransient` validates like the real one, refuses a unit that is active
  (`ErrUnitExists`), and plays the run through `OnRun`, which writes the
  output and returns the result; `Runs()` lists what was run.
- **FlakyBus:** the real `DBusSystemd` over a simulated bus that plays every
  operation on a `FakeSystemd` and drops the connection where `Drop` says
  (before a call, or after it with the answer or job result lost), for tests
  of the code above the adapter under connection loss.

## accounts — shadow tools, classified by exit code

Interface `Accounts`: `CreateUser`, `DeleteUser`, `LockUser`, `UnlockUser`,
`ExpireUser`, `SetPassword`, `EnsureGroup`, `CreateGroup`, `DeleteGroup`,
`AddToGroup`, `RemoveFromGroup`, `IsMember`, `UserExists` (read from
`/etc/passwd`, where useradd writes; no tool runs).

Tools: `useradd`, `userdel`, `usermod` (`-L`, `-U`, `-e`), `groupadd`,
`groupdel`, `gpasswd` (`-a`, `-d`), `chpasswd` (password on stdin).

Exit codes, from the man pages and **measured** on Ubuntu 24.04 (shadow
4.13), Ubuntu 26.04 (shadow 4.17.4) and Debian 13:

| Tool | Code | Typed error |
|---|---|---|
| useradd | 3 | `ErrInvalidArgument` |
| useradd | 4 | `ErrIDTaken` (UID not unique) |
| useradd | 6 | `ErrNoSuchGroup` (a named group does not exist) |
| useradd | 9 | `ErrUserExists` |
| useradd | 12 | `ErrHome` (cannot create home) |
| useradd | 19 | `ErrInvalidName` (shadow ≥ 4.15: bad user name; 26.04 returns this) |
| userdel | 6 | `ErrNoSuchUser` |
| userdel | 8 | `ErrUserBusy` (user logged in / has processes) |
| userdel | 12 | `ErrHome` |
| usermod | 3 | `ErrInvalidArgument` |
| usermod | 6 | `ErrNoSuchUser` |
| usermod | 8 | `ErrUserBusy` |
| groupadd | 9 | `ErrGroupExists` (without `-f`) |
| groupadd | 4 | `ErrIDTaken` |
| groupadd | 3 | `ErrInvalidArgument` |
| groupdel | 6 | `ErrNoSuchGroup` |
| groupdel | 8 | `ErrPrimaryGroup` (someone's primary group) |
| gpasswd | 3 | **ambiguous**, see below |
| chpasswd | 1 | generic; the user is checked first, see below |

- **gpasswd** exits 3 for "no such user", "no such group" and "not a member"
  alike (measured). On exit 3 the adapter reads the group and user databases
  (`/etc/group`, `/etc/passwd`) and returns `ErrNoSuchGroup`, `ErrNoSuchUser`
  or `ErrNotMember`, in that order. `gpasswd -a` of an existing member exits 0
  (idempotent, measured).
- **chpasswd** exits 1 for every failure. `SetPassword` checks the user exists
  first (`ErrNoSuchUser`) and refuses an empty password or one containing a
  newline, CR or NUL (`ErrInvalidPassword`) — chpasswd reads one
  `name:password` line. A `:` inside the password is kept (chpasswd splits at
  the first colon only; measured on Ubuntu 26.04).
- Lock/unlock/expire use `usermod -L` / `-U` / `-e 1|""` rather than
  `passwd -l`, because `passwd` exits 1 for a missing user and usermod exits 6.
- User and group names must match `^[a-z_][a-z0-9_-]{0,31}$`; anything else
  is `ErrInvalidName` before a tool runs.
- Any other non-zero exit is returned as the `*ExitError`.
- Verified against the real tools in throwaway Ubuntu 24.04 and 26.04
  containers (`integration_test.go`, build tag `hostintegration`).
- **Fake:** `FakeAccounts`, an in-memory user/group/membership database with
  the same typed errors, plus `Locked`, `Expired` and `Password` state.

## postgres — an external PostgreSQL server (#81)

`PostgresAdmin` (`NewPostgres(RemoteServer, workDir, exec)`, `postgres.go`) administers an external
PostgreSQL server; there is no PostgreSQL on the box.

- **Statements:** `pgx` as `AdminUser`, one pool per database (`""` is `postgres`), `sslmode=require`
  with `tls.Config{ServerName: Host}` verified against `CACert` or the system's authorities: TLS or
  no connection, never a fallback to plain text. Statements go as text (simple protocol): identifiers
  are checked (`ValidPGName`: lowercase letters, digits, `_`, at most 63) and quoted
  (`QuotePGIdent`), values quoted (`QuotePGLiteral`).
- **Tools:** `pg_dump` and `psql` run by `systemd-run` as a **dynamic user** with a read-only file
  system, no home, a private `/tmp`, no capabilities, `AF_INET`/`AF_INET6` only, 1 GiB of memory and the
  data timeout. The password and the CA reach them as service credentials (files in a `0700`
  directory of the work directory, removed after the run), never an argument; host, port, user,
  database and `PGSSLMODE=verify-full` are environment. The reason: a dump is the tenant's data, and
  `psql` runs the meta-commands in it (`\!` runs a shell), so nothing it reads may hold anything of
  this machine or of the administrator.
- `Dump` is `pg_dump --format=plain --no-owner --no-acl` as the administrator. `Restore(database,
  role, r)` makes a short-lived login role `cgq_<16 hex>` allowed to act as `role` (the database's
  owner role), runs `psql -v ON_ERROR_STOP=1 --single-transaction` as it with `role=<role>` (objects are
  then the owner's), and drops the login afterwards, whatever happens.

## mariadb — admin over the driver, dump and import over the tools

Interface `MariaDB`: `Exec`, `QueryStrings`, `ScopedAccount`, `Dump`, `DumpAs`, `Import`, `Unborrow`.
The agent's database domain over it is `internal/agent/ops/db`.

- **Admin connection:** `go-sql-driver/mysql` as `root` over the Unix socket
  (`/run/mysqld/mysqld.sock`), authenticated by MariaDB's `unix_socket`
  plugin — no password exists. One pooled `*sql.DB`, opened lazily.
  `InterpolateParams` is on so values (passwords) are bound as escaped
  literals in statements MariaDB cannot prepare (`CREATE USER … IDENTIFIED
  BY ?`); identifiers are never bound, they are validated against
  `^[A-Za-z0-9_]{1,64}$` (and never `mysql`, `sys`, `performance_schema`,
  `information_schema`: SR-R1) and back-quoted. Multi-statements stay off.
  `AllowAllFiles` stays false and nothing is registered with the driver, so
  `LOAD DATA LOCAL INFILE` reads no file (`RootConfig`, SR-T1).
- In a GRANT the database name is a LIKE pattern: `GrantPattern` escapes
  `_` and `%` (`site\_1`), so a grant is on one database (SR-T2).
- Driver errors are returned as `*mysql.MySQLError` (test with `errors.As`,
  read `.Number`), never matched by text.
- **A remote server (#30).** `MariaDBAdmin.Remote` (`RemoteServer{ID, Host, Port, TLS, CACert,
  AdminUser, AdminPassword}`, `NewRemoteMariaDB`) points the same adapter at an external MariaDB or
  MySQL server: the admin connection is the driver over TCP as `AdminUser` (with
  `tls.Config{ServerName: Host}` verified against `CACert` or the system's authorities when `TLS`;
  the driver's plain connection otherwise), and `Exec`/`QueryStrings` read an account's host part
  `'localhost'` (and `` `localhost` ``) as `'%'` and add `REQUIRE SSL` to a `CREATE USER` or `ALTER
  USER … WITH MAX_USER_CONNECTIONS` when `TLS`. The tools reach it with `--protocol=tcp` and a
  defaults file (still `0600`, still the only option file read) that names `host`, `port` and, with
  TLS, `ssl-ca` (the CA written beside it, or the system bundle) and `ssl-verify-server-cert`. A
  dump the agent itself issues (`Dump`) reads as the administrator in that file. An **import never
  does**: a dump is tenant content, and `--one-database` is a client-side filter, not a privilege
  boundary (it skips statements only after a `USE` of another database, so leading and server-level
  statements — `GRANT`, `CREATE USER`, `DROP DATABASE other` — would run as the administrator).
  Borrowing a site's account needs `mysql.global_priv`, which a managed server rarely grants, so an
  `ImportAs.Site` import on a remote server signs in as the account itself with the password the
  panel sent (`ImportAs.SitePassword`, `loginAccount`), DEFINER `CURRENT_USER`, so the site's
  triggers and views keep their owner. Without that password it falls back to a throwaway `cgq_`
  account scoped to exactly that one database (`GRANT … ON ``<db>``.* `` with `TRIGGER`, host part
  `` `%` ``, `REQUIRE SSL` under TLS), dropped afterwards, DEFINER `CURRENT_USER`; `Import` returns
  an `ImportNote` so the caller can warn that objects with a definer will not run. `Unborrow` does
  nothing. Errors never include the password.
- **`ScopedAccount(ctx, db, readOnly)`** — the semantics of the agent's
  `scopedDBUser`, unchanged:
  - account `cgq_<16 hex>@localhost`, a 32-character base64url password,
    `MAX_USER_CONNECTIONS 4`;
  - read-only: `SELECT, SHOW VIEW` on `` `db`.* `` (the name escaped);
  - read-write: `SELECT, INSERT, UPDATE, DELETE, CREATE, DROP, INDEX, ALTER,
    CREATE TEMPORARY TABLES, LOCK TABLES, CREATE VIEW, SHOW VIEW` — never
    `TRIGGER`, `EVENT` or routine privileges (their objects would carry a
    definer that is dropped seconds later);
  - a `[client]` defaults file with user, password, the socket and
    `default-character-set=utf8mb4`, mode 0600, in the work
    directory;
  - `Close()` removes the file and drops the account (with its own short
    deadline, so a cancelled request still cleans up). A failed setup step
    drops what it created.
  - The startup sweep matches exactly `'cgq_<16 hex>'@'localhost'`
    (`ScopedAccountRe`).
- Verified in a MariaDB 11.8 container with root set up as Ubuntu does
  (`mysql_native_password USING 'invalid' OR unix_socket`): the driver
  authenticates as root over the socket, a scoped account is created, seen,
  closed and gone, an import's `system` command is refused and does not run,
  an import cannot read another database, the dump streams
  (`integration_test.go`).
- **`Dump(ctx, db, w, tables…)`**: `mariadb-dump --protocol=socket
  --single-transaction db [tables…]`, stdout streamed into `w`. Root (socket
  auth). Table names follow the database rule, so none can read as an
  option.
- **`DumpAs(ctx, as, db, w, tables…)`** (Quarry's signed-in users,
  Fase 9b): the same argv after `--defaults-file=<file>`, a 0600
  root-only file in the work dir holding `as.User` (a site name,
  `site.ValidDBName`) and `as.Password` (never empty, no NUL, CR or LF),
  removed when the dump ends. The server then dumps what that user may
  read. Nothing is created or granted.
- **Option values** (`OptionValue`): a person's password goes into the
  defaults file as written when it is only `A–Z a–z 0–9 . _ ~ @ % + = -`,
  otherwise in double quotes with `\` and `"` escaped: unquoted, a `#`
  would start a comment and spaces at the ends would be dropped.
- **Only the agent's defaults file** (`DefaultsArg`, `--defaults-file`): the
  clients run with a defaults file read nothing else, neither `/etc/mysql`
  nor root's `~/.my.cnf`. With `--defaults-extra-file` those were read too,
  and later, so a `[client]`/`[mysql]`/`[mysqldump]` `user=root` there
  replaced the file's user and unix_socket signed the tenant's import or
  dump in as root. The file therefore carries what the server's client
  files give: `socket=` (the agent's) and `default-character-set=utf8mb4`.
- Dump and Import run with `DataTimeout` (24 h) as the command's own
  timeout: they move a whole database, and the caller's context is the real
  deadline (the runner's default — a minute in the agent — would kill them).
- **`Import(ctx, db, r, as)`**: exactly `mariadb --defaults-file=<file>
  --sandbox --local-infile=0 --protocol=socket --one-database db` with `r`
  on stdin, through `RewriteDefiners` (R-0.1: `--sandbox` refuses `system`
  and other client commands that would run as root; SR-T1:
  `--local-infile=0` refuses `LOAD DATA LOCAL INFILE`, which the root-run
  client would otherwise answer with any file root can read; the account
  keeps the SQL inside the one database). The account (SR-T7):
  - `as.Site` (`site_<id>` only): the site's own account, **borrowed**: its
    first authentication is read from `mysql.global_priv` (`$.plugin`,
    `$.authentication_string`; only `mysql_native_password` and `ed25519`),
    then `ALTER USER ?@'localhost' IDENTIFIED VIA <plugin> USING ? OR
    mysql_native_password USING PASSWORD(?)` with a random password in the
    defaults file, and `ALTER USER … IDENTIFIED VIA <plugin> USING ?` (its
    own alone) when the client has exited, success or not, on its own
    deadline. One borrow per account at a time. Measured on 10.11 and 11.8:
    the site's own password works during and after, the borrowed one stops
    working, `MAX_USER_CONNECTIONS` is kept;
  - `as.Login`: a signed-in database user with its own password (the
    defaults file of `DumpAs`): nothing is created, granted or borrowed,
    and every `DEFINER=` becomes `` `<user>`@`localhost` ``;
  - `as.Triggers`: a read-write scoped account with `TRIGGER` too;
  - otherwise a read-write scoped account, dropped afterwards.
  Every `DEFINER=` clause becomes `` `site_<id>`@`localhost` `` (or
  `CURRENT_USER` for a scoped account), so no account of the importing side
  needs SET USER and no privileged definer is kept.
- **`RewriteDefiners(dst, src, definer)`**: a streaming lexer — single,
  double and back quotes (with backslash escapes and doubled quotes), `#`
  and `-- ` comments and plain `/* */` comments are copied untouched;
  executable comments (`/*!…`, `/*M!…`) are code. In code, `DEFINER
  = <user>[@<host>]` (each part back-quoted, quoted or bare) and `DEFINER =
  CURRENT_USER[()]` / `CURRENT_ROLE[()]`, not preceded by an identifier
  character, are replaced. It is not the security boundary: a clause it
  misses is refused by the server.
- **`Unborrow(ctx)`**: every `site_<id>` account whose `mysql.global_priv`
  entry has more than one `auth_or` method (a borrow cut short) gets its
  first, its own, alone. The startup sweep runs it.
- **Fake:** `FakeMariaDB` records statements, hands out fake scoped accounts
  (and tracks which are still open), keeps dumps/imports in memory (with
  `ImportedAs`, each import's database and definer, and `DumpedAs`, each
  `DumpAs`'s user and database; `Unborrow` is the
  statement `UNBORROW`);
  `OnExec`, `OnQuery`, `OnDump` and `OnImport` script failures and answers.

## nginx — test, reload that waits, build info

Interface `Nginx`: `Test`, `Reload`, `BuildInfo`.

- `Test(ctx)`: `nginx -t`. Failure is a `*ConfigError` carrying nginx's stderr
  (where `-t` writes its verdict) and matching `ErrConfigInvalid`.
- `Reload(ctx)`: reads the master PID from the pid file and the current
  worker PIDs from `/proc` (processes whose parent is the master), asks
  systemd to reload the unit (waiting for the job), then **waits until a
  worker that was not there before is running and none of the old workers
  is still serving** (each is gone or titled "… is shutting down") — the
  proof that the master re-read its configuration, started a new generation
  and stopped the old one from accepting. The master forks the new workers
  before it signals the old ones, and in between an old worker still accepts
  new connections with the old configuration: measured on the test box, the
  request sent right after a reload that waited only for a new worker was
  served the configuration it replaced. It polls `/proc` (no event exists
  for this) with a 10 s ceiling; on timeout it returns `ErrReloadTimeout`
  (the master kept the old generation, typically because the new
  configuration failed at runtime). Old workers that are draining
  connections they already hold are not waited for.
- `BuildInfo(ctx)`: the output of `nginx -V`, **read from stderr**, where
  nginx writes it (the HTTP/3 probe once read stdout and never activated).
- **Fake:** `FakeNginx` with a configurable test verdict and counters.

## restic — `--json`, secrets in the environment

`Restic.Run(ctx, repo, args...)` / `RunIO(ctx, repo, stdin, stdout, args...)`.

- `Repo{URL, Password, Env}`. `RESTIC_REPOSITORY` and `RESTIC_PASSWORD` are
  set in the child's environment — never argv (a REST/S3 URL can embed
  credentials). Only `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`,
  `AWS_DEFAULT_REGION`, `B2_ACCOUNT_ID`, `B2_ACCOUNT_KEY` pass from `Env`;
  values with NUL or newline are dropped. Any `RESTIC_*`, `AWS_*`, `B2_*` the
  agent itself inherited is stripped first.
- **SFTP key** (#23). `Repo.SSHKey`, the panel's private key for an `sftp:`
  repository, is written to `<SSHDir>/id_ed25519` (root's, 0600; the folder
  0700, `/var/lib/cloudground-agent/backup-ssh` by default) before the
  call, and restic gets `-o sftp.command=ssh <user@host> -p <port> -i
  <key> -o IdentitiesOnly=yes -o BatchMode=yes -o
  StrictHostKeyChecking=accept-new -o UserKnownHostsFile=<SSHDir>/known_hosts
  -s sftp` ahead of its other arguments, the user, host and port read from
  the URL (`SFTPTarget`; a user or host starting with `-` is refused). So the
  key is never an argument or a variable, the destination's host key is
  learned at the first connection and checked at every later one, and
  `ssh` reads nothing of root's own `~/.ssh`. A sandboxed run binds
  `SSHDir` read-only. Without a key, an `sftp:` repository runs as before
  (root's own ssh configuration).
- Exit codes (restic ≥ 0.17, documented): 1 fatal, 3 backup read some files
  incompletely (`ErrIncomplete`), 10 repository does not exist
  (`ErrRepoNotFound`), 11 lock failed (`ErrRepoLocked`), 12 wrong password
  (`ErrWrongPassword`), 130 interrupted.
- Decoders: `DecodeBackupSummary` (the `summary` message of `backup --json`),
  `DecodeSnapshots` (`snapshots --json`), `DecodeForgetPlan` (`forget
  --dry-run --json`, which may be preceded by non-JSON lines).
- **Output.** `Run` and `RunSandboxed` keep restic's stdout in a
  `ResticOutput` (`NewResticOutput(args)`, also what a caller of `RunIO`
  that decodes a backup uses): at most `ResticOutputLimit` (16 MiB) is
  kept; past it the rest is dropped (restic is never blocked or killed for
  it) and the call fails with `ErrOutputTooLarge` joined to restic's own
  error, so a truncated plan or snapshot list is never decoded as whole. For
  `backup … --json` the `status` progress messages are dropped as they
  arrive: without `--quiet` restic prints them dozens of times a second when
  stdout is not a terminal (measured: 0.16.4 and 0.18.1 about 40/s, 0.19.0
  about 6/s), so an hours-long backup held tens of MB. The backups pass
  `--quiet` as well, which on 0.16.4, 0.18.1 and 0.19.0 leaves only the
  `summary` line on stdout (errors go to stderr, kept as its last 64 KiB);
  the filter is the bound if a restic ever prints them anyway.
- Timeouts come from the caller's context (data-moving commands take hours).
- **Stopping.** restic runs with `Interrupt: ResticGrace` (20 s): a
  cancelled or timed-out restic gets SIGINT, on which it removes its lock
  from the repository and exits (130); only one that has not exited 20 s
  later is killed with its group. Killed outright (SIGKILL, the previous
  behaviour) it left its lock file in the repository: another host sees it
  as held until it is 30 minutes old, and `restic unlock` is needed to clear
  it. restic 0.16 handles SIGINT the same way. In the agent restic runs as
  a helper in a unit of its own (see helpers), where the same stop is the
  unit's `KillSignal=SIGINT` with `KillMode=mixed`.
- **Version.** The installer puts a pinned restic (0.19.1, the official
  release binary, its SHA-256 fixed in `internal/install`, SPEC §3) in
  `/usr/local/bin`, ahead of the distribution's on `PATH`: Ubuntu 24.04 ships
  0.16, which exits 1 for every failure (no typed codes). The adapter still
  works with 0.16: an untyped failure is an `*ExitError` with no `Kind`, and
  callers treat it as "anything else" (ops/backup SPEC §3).
  `ResticVersion(ctx, exec)` runs `restic version` and parses it
  (`ResticVer{Major, Minor, Line}`, `TypedExitCodes()` for ≥ 0.17). The
  agent logs a warning at start for a restic without typed exit codes
  (`Agent.CheckRestic`): a box installed before the pin, and kept up to date
  by self-updates (which install no packages), still has 0.16.
  `Agent.ResticVersion` is there for Capabilities to report. Phase 6
  (installer and self-update): the self-update installs the pinned restic
  too.
- **The sandboxed files backup** (R-10.5, ops/backup SPEC §4):
  `RunSandboxed(ctx, repo, TreeSandbox{Tree, Writable}, args...)`. With
  `Transient` (the agent's wiring) it is `systemd-run` (`ServiceArgs`:
  `--wait --pipe --collect`, `--uid=root`, unit `cg-restic-<n>.service`,
  description "restic backup of <tree>", `RuntimeMaxSec` 24 h) with
  `ResticSandboxProps`: `TemporaryFileSystem=/` (an empty root), the
  read-only binds of `/usr`, `/lib*`, `/bin`, `/sbin` and the few `/etc`
  files restic reads (`resolv.conf`, `hosts`, `nsswitch.conf`, `ssl`,
  `ca-certificates`, `passwd`, `group`, `localtime`, `gai.conf`; a missing
  one tolerated), in `HelperSlice` (see helpers), `BindReadOnlyPaths=<Tree>`, `BindPaths=` for each of
  `Writable` (restic's cache, a local repository), `PrivateDevices`,
  `PrivateTmp`, `PrivateIPC`, `ProtectProc=invisible`, the kernel
  protections, `NoNewPrivileges`, `RestrictSUIDSGID`,
  `CapabilityBoundingSet=CAP_DAC_READ_SEARCH` (a tenant's files may be
  0600; nothing else), `KillSignal=SIGINT` and `TimeoutStopSec=20` (so a
  stop is restic's SIGINT, which removes its lock). The command is
  `/usr/local/bin/panel-agent restic-exec -- <args>`: the repository, the
  password, the cache directory and the allowed credentials go to it on
  **stdin**, NUL-terminated (never a unit property, which every local user
  can read over D-Bus, nor argv); it accepts only those variables, sets a
  fixed `PATH`, puts `/dev/null` on stdin and execs restic. A path with
  whitespace, a quote or a backslash, a relative one or `/` is refused
  before systemd is asked. A cancelled call stops the unit (`systemctl
  stop`, retried each second while systemd has not loaded it yet) **while
  systemd-run is still up**, and waits for systemd-run to return on its own:
  with `--pipe` restic's stdout and stderr are systemd-run's, so killing
  systemd-run first closed them and restic died of SIGPIPE on its next write
  (progress, or the line SIGINT makes it print) without removing its lock
  (R-4.6, measured on the e2e box). Only a run still there `ResticGrace` +
  30 s after the first stop has systemd-run killed, and its unit stopped once
  more. The call returns ctx's error. Without `Transient` (the tests, which have no systemd) restic runs
  directly. Only the box shows that systemd accepts `TemporaryFileSystem=/`
  with these binds on 255 and 259.
- **Fake:** `FakeExec` underneath — restic is a CLI and the decoders are pure.

## wpcli — one runner

`WPCLI.Run(ctx, Invocation{User, Path, Args, Stdin, Slice, Sandbox})`:

- wp-cli runs the site's code (plugins, theme, `wp-config.php`), so it runs
  as the site's PHP does (R-4.5 review): `systemd-run --wait --pipe` of a
  transient service `cg-wp-<user>-<n>.service` in `Slice`, as `User`, in
  `Path`, with `Sandbox` (the PHP master's unit lines, `phpfpm.Sandbox`) as
  `-p` properties and `RuntimeMaxSec=3000`, running `wp --path=<path>
  <args…>`. Its stdout, stderr and exit status come back through
  systemd-run (`--pipe`, `--wait`), as `runuser`'s did. A slice and a
  sandbox are required. A cancelled call, or one whose output passed
  `StdoutLimit` (`ErrStdoutTooLarge`), stops the service (`systemctl
  stop`): it is not systemd-run's child;
- an **explicit minimal environment**: `PATH`, `LANG=C.UTF-8`,
  `WP_CLI_CACHE_DIR` in the run's private `/tmp`, nothing else of the
  agent's (no tokens, no restic cache dir); systemd sets `HOME`, `SHELL`,
  `USER`, `LOGNAME` for the user;
- `ServiceArgs(Service)` builds the systemd-run argv of every such run (the
  shell, cron jobs, wp-cli): `--quiet --collect --wait
  --service-type=exec`, unit, description, slice, `--uid`/`--gid`,
  working directory, `IgnoreSIGPIPE=no`, `TimeoutStopSec=5`, the
  properties, `RuntimeMaxSec`, `--setenv`, and the terminal (`TTYPath`,
  `StandardInput=tty-force`, …) or `--pipe`;
- **no expansion of the command** (R-10.5): systemd expands `$VAR` and
  `${VAR}` in ExecStart's arguments, so a wp-cli search-replace value or a
  cron job's line holding `$HOME` reached the command expanded. Where
  `systemd-run --version` says 254 or later (read once per process),
  `ServiceArgs` passes `--expand-environment=no`; otherwise (or when the
  version cannot be read) every `$` of the arguments after the program is
  doubled, which systemd's expansion turns back into one. `SystemdCommand`
  is what ends up after `--`;
- an optional secret on stdin (`--prompt=<field>`), never in argv;
- `User` must be a valid account name, `Path` absolute and clean.

## helpers — the heavy tools in units of their own (#66)

restic, `mariadb-dump`, the `mariadb` client and certbot (`HelperNames`) use
as much memory as the data they handle (a repository's index, a database,
certbot's Python). Run as the agent's children they were in
`cloudground-agent.service`'s cgroup, so a memory limit on the agent would have
cut a large backup or restore. In the agent they run through `HelperExec`, an
`Exec` that starts each of them in a transient service of its own and passes
every other command to the `Exec` it wraps (OSExec) unchanged:

- `systemd-run` (`ServiceArgs`: `--wait --pipe --collect`, `--uid=root`) of
  unit `cg-helper-<tool>-<n>.service` in `HelperSlice` (`cg-helper.slice`,
  under `cg.slice` beside the sites' `cg-site.slice`) with `HelperProps`: the
  sandbox lines of `cloudground-agent.service`, one for one (the helpers
  inherited them; `TestHelperPropsAreTheAgentsSandbox` holds the two
  together, so a line added to the unit reaches the helpers too), and the
  helper's own `MemoryMax=75%` (provisional, `bench/agent-memory.md`: a
  runaway helper is killed in its own unit before the kernel picks a site's
  PHP or MariaDB; swap is not limited). The command is
  `/usr/local/bin/panel-agent helper-exec -- <tool's absolute path> <args>`.
- **The same contract as OSExec.** The tool is looked up on the agent's
  `PATH` before anything starts (a missing one wraps `exec.ErrNotFound`). Its
  environment (`Cmd.Env`, or the agent's own when nil; restic's secrets
  included) goes to helper-exec on **stdin**, `KEY=value` entries each
  NUL-terminated and then an empty one, never as a unit property (which any
  local user can read over D-Bus) nor in argv. helper-exec reads it one byte
  at a time, so nothing past it is consumed, and execs the tool, whose stdin
  is what follows (`Cmd.Stdin`: a dump piped into `restic backup --stdin`).
  With `--pipe` the tool's stdout and stderr are systemd-run's: a captured
  stdout keeps `StdoutLimit`, a streamed one is the caller's, stderr keeps its
  last 64 KiB. systemd-run exits with the tool's status, returned as an
  `*ExitError` under the tool's name (restic's typed codes classify as
  before); a tool killed by a signal or for memory comes back as
  systemd-run's 1 or 255, not -1. helper-exec's own failures exit 126.
- **Deadline and cancellation** are the caller's context and `Cmd.Timeout`
  (the runner's default when unset), and fail the call with the context's
  error under the tool's name. systemd-run runs on a context they do not
  end: the tool is not its child, and killed first it would leave the tool
  writing into closed pipes. The unit is stopped instead (`systemctl stop`,
  retried while systemd has not loaded it yet) while systemd-run is up:
  `KillSignal=SIGKILL` to the whole cgroup at once, or, with `Interrupt`,
  `KillMode=mixed`, `KillSignal=SIGINT` and `TimeoutStopSec=<Interrupt>`
  (SIGINT to the tool alone, the rest killed when it has exited or the grace
  is over: OSExec's Interrupt). systemd-run is killed only if it is still
  there `Interrupt` + 30 s after the first stop. A stdout past `StdoutLimit`
  has systemd-run killed by the runner, then the unit stopped.
  `RuntimeMaxSec` is a backstop past the agent's own deadline.
- **Leftovers.** A helper outlives an agent that stops or dies. At start,
  before serving, the agent stops every `cg-helper-*` and `cg-restic-*` unit
  still loaded (`Agent.SweepHelpers`): none of them is the new agent's, and
  a restic among them holds its repository's lock.
- Verified on systemd 255 (Ubuntu 24.04) and 259 (26.04) in containers booted
  with systemd (`helper_integration_test.go`): the tool runs in
  `/cg.slice/cg-helper.slice/cg-helper-…`, gets its environment and stdin,
  its exit status and stderr come back, a cancel interrupts it and leaves
  nothing behind, and one past its memory limit ends with result `oom-kill`
  while the caller runs on.
- Wired: `ResticCLI.Exec`, `MariaDBAdmin.Tools`, `Certbot.Exec`. The rest
  (`nginx -t`, ufw, the account tools, apt-get, `restic version`) stays the
  agent's child, inside its limit.

## firewall (ufw) and acme (certbot)

- `UFW`: `Status` (`ufw status` under `LC_ALL=C`; the first line decides
  active/inactive), `Allow`, `Deny`, `Delete` with typed `Rule{Port, Proto,
  From}` validated before any process starts. A missing binary is
  `ErrUnavailable`. ufw exits 1 for every failure, so other errors are the
  `*ExitError`. `Delete` runs `delete deny` and `delete allow` and reports
  either failure: deleting a rule that does not exist exits 0 (measured,
  Ubuntu 24.04). `Enable` runs `ufw --force enable` (without `--force` ufw
  asks on its terminal); allowing what must stay reachable first is the
  caller's (system SPEC §4).
- `Certbot`: `CertOnly(ctx, CertRequest)` (webroot or Cloudflare DNS, staging)
  and `Delete(ctx, name)`. A lineage name is a domain or a single label (the
  panel's `cloudground-panel`): lowercase letters, digits and inner hyphens,
  never a leading `-`, a `/` or `..`. **Fake:** `FakeACME` validates as the
  real adapter does, records requests, runs an optional hook, and remembers
  issued lineages so `Delete` of another is `ErrNoSuchCert`. `Delete` of a lineage with no renewal config
  (`<config>/renewal/<name>.conf`) returns `ErrNoSuchCert` **without running
  certbot** — the replacement for matching "no certificate found" in its
  output. certbot exits 1 for every failure; that is the `*ExitError`.

## What is wired (R-2.4) and what is not

Wired as proof: `ServiceStatus` / `RestartService` use `Systemd`; the SFTP
access group and passwords (`ops/sftp`) use `Accounts`.

R-3.7: `ops/sites` uses `Accounts` (the site user, `UserExists`, the busy
case of `DeleteUser`), `Systemd` (the PHP master, its slice drop-in, the
slice stop on deletion), `Nginx` (test and waiting reload; `BuildInfo` for
HTTP/3 and the HTTP/2 form) and `Exec` (`php-fpm -t` as the site user). The
agent's old `Runner` is gone: every command goes through an adapter.

R-3.9: `ops/certs` uses `ACME` for issuance and for the lineage removed with a
site; `ops/panel` uses `ACME`, `Nginx` (test, waiting reload, `nginx -V` for
the HTTP/2 form) and `Systemd` (the update guard's transient unit, and the
guard's restarts).

R-3.11: `ops/cron` uses `Exec` (`crontab -u <user> -`) and `Systemd`
(`RunTransient` for "run now", `ListUnits`/`Stop` for the runs of a site
being deleted).

## Per-site slices (R-3.7)

A site's processes run in `cg-site-<id>.slice` (`site.Slice`), so that one
`Stop` of the slice ends them all:

- `TransientRun.Slice` sets `Slice=` on a transient unit (validated by
  `ValidSlice`: letters, digits, `-`, `_`, `.`, ending in `.slice`, never a
  leading `-`; anything else is `ErrInvalidArgument` before D-Bus is asked);
- `Invocation.Slice` and `Invocation.Sandbox` run wp-cli as a sandboxed
  transient service in the slice (see wpcli above). A scope, which it was,
  only moved the cgroup and kept the agent's mounts: the site's plugins ran
  outside the sandbox;
- the PHP master's persistent unit gets the slice from a drop-in that
  `ops/sites` writes.

**Fake:** with `FakeSystemd.UnitDir` set, `DaemonReload` (and enable and
disable, which reload) reads that directory as systemd reads
`/etc/systemd/system`: a new `*.service` file is loaded inactive, a unit
whose file is gone is not-found (forgotten once it is not running; a running
one can still be stopped), and `Slice=` in the file or its
`<unit>.d/*.conf` drop-ins places it in a slice (`SliceOf`). Stopping a
slice stops every unit in it; a slice nothing is in is `ErrNoSuchUnit`.

## Site shell (R-4.5)

Two additions for `ops/sftp`'s site shell, on the concrete types (not in
the `Systemd` and `Accounts` interfaces; ops/sftp declares what it needs):

- `UserSpec.UID` (`--uid`) and `UserSpec.NonUnique` (`--non-unique`, which
  requires a uid): a site's shell login shares the site user's uid.
- `ForceDeleteUser(ctx, name)` on `ShadowAccounts` and `FakeAccounts`:
  `userdel --force <name>`, for that login: the uid's PHP processes would
  make a plain `userdel` refuse (exit 8). No home is removed; the login has
  no group of its own.
