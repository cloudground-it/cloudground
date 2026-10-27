# `internal/backuprepo` — where a backup repository may be (specification)

The restic repository URL is a panel setting an administrator types, sent to
the root agent with every backup call, and handed to restic, which acts on
it as root: it creates folders at a local path, opens `ssh` for `sftp:`,
makes HTTP requests for `rest:` and `s3:`. Before R-10.5 it was never
checked. `Validate(ctx, repo, Options)` is the one rule; the API applies it
when the setting is saved (`checkBackupPairing`, before the agent's test of
the pairing) and the agent before every restic call (`ops/backup` `repoOf`),
so a URL the API let through, or one sent to the agent directly, is refused
there too. A refusal is an `*Error` whose message the panel shows.

## 1. Rules

Anywhere in the URL: at most 1024 bytes, no whitespace, no control
character. Then, by prefix (anything else is refused):

| Form | Accepted when |
| --- | --- |
| `/<path>` (local) | clean and absolute, strictly below `/var/backups/cloudground/` or `/mnt/`, each folder name of letters, digits and `. _ @ + = , -` (no `.`/`..`) |
| `s3:<host>[:port]/<bucket>[/…]`, `s3:http(s)://<host>[:port]/<bucket>` | the host passes the endpoint rule (§2); no user info, query or fragment in the URL form |
| `rest:https://[user:pass@]<host>[:port]/…` | `https` only (the repository password and the data would otherwise cross in the clear); the host passes §2 |
| `b2:<bucket>[:<path>]` | a bucket name of 5–63 letters, digits and `-`; the path of letters, digits and `. _ / -` |
| `sftp:[user@]host:<path>`, `sftp://[user@]host[:port]//<path>` | neither user nor host starts with `-` (ssh would read it as an option); the user of letters, digits and `. _ -`; the host a host name or an address; no password in the URL (use the panel's key, internal/api SPEC §5.8) |
| `rclone:<remote>:<path>` | `Options.Rclone()` says rclone is installed (the agent: `rclone` on `PATH`; the API leaves this to the agent); no `..` |

## 2. The endpoint rule (s3:, rest:)

The host, an address or a name, must be **public**: an address literal, and
every address a name resolves to (`Options.Resolve`, the system resolver
with a 5 s bound), must not be loopback, private (RFC 1918, ULA),
link-local (the cloud metadata address among them), unspecified,
multicast, the shared space 100.64.0.0/10 or otherwise not global unicast;
`localhost` and `*.localhost` are refused without a lookup, and a name that
does not resolve is refused. `Options.AllowPrivate`, set from
`CLOUDGROUND_BACKUP_PRIVATE_ENDPOINT=1` in `/etc/cloudground/panel.env`
(internal/config; read by the API and the agent each from its own
configuration, never sent over the socket), lets a private endpoint through
(a MinIO on the LAN). It is the administrator's on the box: no panel user can
set it.

Resolution happens when the rule runs (the save, and each agent call); a
name that resolves elsewhere between that and restic's own lookup is not
caught (accepted: the repository is an administrator's setting).

## 3. Upgrade

A repository saved before R-10.5 that the rules refuse (a local folder
elsewhere under `/var/backups`, a private endpoint) makes every backup fail
with the rule's message until the administrator moves it, or sets the flag
for a private endpoint. Nothing is migrated silently.
