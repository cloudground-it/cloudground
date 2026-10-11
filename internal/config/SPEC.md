# internal/config — behaviour spec (R-4.6)

The one configuration loader of `panel-api`, `panel-agent` and `cgctl`.
Each binary calls `config.Load(<component>)` once at start and gets a
validated `Config`; nothing else in a binary reads `CLOUDGROUND_*`
variables.

## 1. Sources

Lowest to highest precedence:

1. the built-in defaults (§3);
2. the component's file in `/etc/cloudground` (§2);
3. the process environment.

The environment wins so that systemd's `Environment=` lines and a one-off
`CLOUDGROUND_X=… cgctl …` override the file.

## 2. Files

| Component | File |
| --- | --- |
| `API`, `Agent` | `/etc/cloudground/panel.env` (also systemd's `EnvironmentFile=` of both units) |
| `CLI` | `/etc/cloudground/cgctl.env` |

`CLOUDGROUND_CONFIG` in the environment names another file instead.

Format, systemd `EnvironmentFile` compatible: one `KEY=VALUE` per line;
blank lines and lines starting with `#` or `;` are ignored; spaces around
the key and the value are trimmed; a value wrapped in matching double or
single quotes loses them. A line without `=` or with an empty key is an
error naming the file and line. Keys that do not start with `CLOUDGROUND_`
are ignored (the file may serve other tools). A `CLOUDGROUND_` key that is in
no component's table (§3) is not an error but is returned in
`Config.Warnings`, which the daemons log; another component's key is not
reported, since `panel.env` is shared by the API and the agent.

A missing file is not an error. Any other read error is, for the daemons;
`cgctl` ignores a file it may not read (an unprivileged user on the box).

## 3. Keys

| Key | Components | Default | Validation |
| --- | --- | --- | --- |
| `CLOUDGROUND_AGENT_SOCKET` | API, Agent | `/run/cloudground/agent.sock` | absolute path |
| `CLOUDGROUND_AGENT_TOKEN_FILE` | API, Agent | `/etc/cloudground/agent.token` | absolute path |
| `CLOUDGROUND_STATE` | API | `/var/lib/cloudground/state.db` | absolute path |
| `CLOUDGROUND_SECRET_KEY` | API | `/var/lib/cloudground/secret.key` | absolute path |
| `CLOUDGROUND_LISTEN` | API | `127.0.0.1:5252` | `host:port`, port 1–65535 |
| `CLOUDGROUND_LOCAL_LISTEN` | API | `127.0.0.1:9080` | `host:port` on a loopback IP (it is plaintext) |
| `CLOUDGROUND_TLS_CERT` | API | `/etc/cloudground/certs/panel.pem` | absolute path |
| `CLOUDGROUND_TLS_KEY` | API | `/etc/cloudground/certs/panel.key` | absolute path |
| `CLOUDGROUND_DEV` | API | off | switch (`1`) |
| `CLOUDGROUND_PHP_VERSION` | API | `8.4` | `<major>.<minor>` |
| `CLOUDGROUND_DRAIN_TIMEOUT` | API | `30s` | Go duration, 1s–10m |
| `CLOUDGROUND_BACKUP_PRIVATE_ENDPOINT` | API, Agent | `` (off) | `1` lets a backup repository's s3:/rest: endpoint be a private or local address (internal/backuprepo SPEC §2) |
| `CLOUDGROUND_SOCKET_GROUP` | Agent | `cloudground` | user/group name |
| `CLOUDGROUND_API_USER` | Agent | `cloudground` | user/group name |
| `CLOUDGROUND_URL` | CLI | `unix:///run/cloudground/api.sock` | `unix://<absolute path>`, `https://host[:port]`, or `http://` to a loopback host |
| `CLOUDGROUND_TOKEN` | CLI | — | API token (`cgt_…`) |
| `CLOUDGROUND_TOKEN_FILE` | CLI | — | file holding the token (precedence below) |
| `CLOUDGROUND_INSECURE` | CLI | off | switch (`1`): skip TLS verification for a non-loopback URL |
| `CLOUDGROUND_EMAIL`, `CLOUDGROUND_PASSWORD`, `CLOUDGROUND_TOTP` | CLI | — | sign-in fallback without a token |

Switches (`CLOUDGROUND_DEV`, `CLOUDGROUND_INSECURE`): `1` is on, empty or
`0` is off, as they always were; anything else (`true`, `yes`) is an error
rather than a new meaning. A user/group name is `[a-z_][a-z0-9_-]*`, at most
32 characters.

`CLOUDGROUND_TOKEN` and `CLOUDGROUND_TOKEN_FILE` are one setting. The source
decides first: anything in the environment beats anything in the file (a
token file named in the environment beats a token written in the file).
Within one source, the token beats the token file. Only the chosen one is
read and validated, so a stale or malformed token in the file does not fail
a command the environment gives a good one. A token read from a file is
trimmed; an unreadable token file is an error. A token must look like
`cgt_` followed by 43 base64url characters.

Every validation error names the key and the source (`default`, the file,
or `environment`). `Load` reports all of them at once.

## 4. Secret files

`ReadSecret(path, min)` reads a shared secret (the agent token): the file's
content without surrounding whitespace, an error when it is shorter than
`min` bytes. The agent requires 32; the API requires the token not to be
empty.
