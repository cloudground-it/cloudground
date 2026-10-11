# cgctl — behaviour spec (R-4.6)

`cgctl` is a thin client of the panel API for scripts. Its configuration
comes from `internal/config` (component `CLI`, internal/config/SPEC.md).

## 1. Command line

    cgctl [--wait|-w] [--timeout <duration>] <command words> [arguments]

The global flags may appear anywhere; `--` ends them. `-v`, `--version` or
`version` anywhere prints `cgctl <version>` and exits 0 without reading the
configuration. `-h`, `--help`, `help` or no command prints the usage (exit 2).

The command is the longest entry of the table in `commands.go` whose words
lead the line; each maps its arguments to one API request. Arguments that
are IDs must be positive integers (they become URL path segments); nothing
else is ever put in a path. A wrong number of arguments prints that
command's usage (exit 2).

## 2. Authentication

1. With a token (`CLOUDGROUND_TOKEN`, or `CLOUDGROUND_TOKEN_FILE`), every
   request carries `Authorization: Bearer <token>`. No sign-in happens.
2. Without one, `CLOUDGROUND_EMAIL` and `CLOUDGROUND_PASSWORD` (and
   `CLOUDGROUND_TOTP`) sign in once (`POST /api/login`) and the session
   cookie is used for the call.
3. `tokens list|create|revoke` always sign in with the password (the API
   refuses tokens on its session-only routes: accounts, users, tokens), and
   fail with a clear message without one.
4. With neither, cgctl exits 1 and says how to get a token.

Transport: `unix://` is plain HTTP over the socket, where the session cookie
(`__Host-…; Secure`) is carried by a jar of its own that sends the panel's
cookies whatever their Secure flag (the socket has one peer, the panel, and
no network to leak on; the standard jar would drop the cookie and every
password-session command would answer 401); `https://` verifies the
certificate unless the host is loopback or `CLOUDGROUND_INSECURE` is set;
`http://` is accepted only to loopback (validated by the loader). No proxy.

## 3. Output

The API's JSON answer, indented, on stdout (with `--wait`, the final task's); `sites create`
prints its summary instead (§6). Errors on stderr, as `cgctl: HTTP <status>: <the API's error>`.

## 4. --wait and exit codes

With `--wait`, when the answer names a task (`{"task": {"id": …}}`, or
`{"job": {"id": …}}` for the job queue, or a task itself as `cgctl task <id>`
returns), cgctl polls `GET /api/tasks/<id>` every second, printing a
progress line to stderr each time it changes, until the status is final:
`succeeded` is success; `failed`, `cancelled` or any status other than
`pending`, `queued`, `running` is failure. The final task is printed on
stdout. An answer that fails while waiting is retried (the panel may be
restarting), except 401, 403 and 404, which end the wait as a failure.
Without a task in the answer, `--wait` changes nothing.

| Exit | Meaning |
| --- | --- |
| 0 | the request succeeded (and, with `--wait`, its task succeeded) |
| 1 | the API refused the request, it could not be reached, or the task failed |
| 2 | usage or configuration error |
| 3 | `--wait` ran out of `--timeout` (default 1h) with the task still running |

## 5. release verify

`cgctl release verify <dir> [file...]` runs before anything else on the line is read: no
configuration, token or panel is needed. It checks `<dir>/SHA256SUMS` against `<dir>/SHA256SUMS.sig`
with the release keys compiled into this cgctl, then each named file (every listed file when none is
named) against its signed checksum (`version.VerifyRelease`, internal/version/SPEC.md §3.3). Exit 0
with `<version>: signature and <n> file(s) verified` on stdout; 1 with `cgctl: release not verified:
<why>` on stderr; 2 for any other use of `release`. The release build runs it, and an operator
can check a downloaded release with it.

## 6. sites create (Fase 9b)

    cgctl sites create <domain> <type> [flags-json] [--with-database] [--db-name <name>]
        [--db-user <user>] [--db-password-stdin | --db-password-file <file> | --db-password <password>]
        [--json]

`<domain>` and `<type>` are the first two arguments that are not flags; `flags-json`, the third,
is an object of more fields of `POST /api/sites` (internal/api SPEC §3.2), never overriding the
domain and type. The flags set `create_database: true`, `database_name`, `database_user` and
`database_password` (also written `--flag=value`), and win over the same fields in flags-json;
`--db-password-stdin` reads the password as one line from stdin (its line ending dropped, at most
4 KiB, empty refused) and `--db-password-file` from a file (its trailing newline dropped), so it
does not show in `ps` nor in the shell's history. `--db-password <password>` still works, but
prints `cgctl: warning: --db-password puts the password where ps and the shell history show
it; use --db-password-stdin or --db-password-file` on stderr before the call. An unknown flag, a
flag without its value (or `--db-password-stdin=…`), two of the three password flags, an
unreadable file, an empty stdin line or more than three positional arguments is a usage error
(exit 2). Unset fields are the panel's (`site_<id>`,
a generated password).

Unlike §3, the answer is printed as the creation's summary, in plain text: the site and its
state (`Being created: task <id>`, or with `--wait` `Created.` or how the task ended), then
the site's URL and panel link, the WordPress admin URL and user, the database's host, port,
socket, name, user and password, and SFTP (host, port and user when on; a new site's is off),
ending with a note that the password is shown only now. `--json` prints the API's answer as §3
does. The exit codes are §4's.

## 7. update and uninstall (R-6.4, R-6.5)

`cgctl update` and `cgctl uninstall`, like `install` and `doctor`, run as root on the server and
need no token or configuration; `main` hands them over before the API client is set up. Both are
specified with the installer: `cgctl update` in internal/install/SPEC.md §10 (it asks the agent,
over its socket with the agent's token, for the same `SelfUpdate` the panel's button asks for, and
waits for the guard's verdict), `cgctl uninstall` in §9. Exit codes: 0 done (or, for `update
--check`, an answer given), 1 refused or failed (with the reason on stderr), 2 a usage error or
not root.
