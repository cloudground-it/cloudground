# ops/sftp — SFTP access and SSH keys

Turning a site's SFTP login on and off, its password, and the public keys
that may sign in as the site user.

Sources: the e2e suite (`e2e/features_test.go` SFTP section: keys refused
while SFTP is off, the password set, the deny group, key login on and off,
the site user unable to replace its `authorized_keys`; the concurrent SFTP /
object-cache change R-0.5; `e2e/security_test.go` the sshd `ForceCommand`),
the API callers (`internal/api/files.go` `/sftp` and `/ssh-keys`), the UI
(`ui/app/pages/sites/[id]/sftp.vue`), the installer's sshd drop-in
(`internal/install/templates/sshd-sftp.conf`, `10-cloudground-sftp.conf`), `host/SPEC.md`.

RPC names, parameters and results are those of `internal/agentrpc/methods.go`
(`SetSFTP`, `SSHKeys`), types in `internal/rpc`. Accounts are changed only
through `host.Accounts` (exit codes and the account databases, never a tool's
wording); no `passwd` is run.

## 1. How sshd decides (installer)

- `DenyGroups cg-sftp-off`: a user in that group cannot sign in at all, by
  password or key. **Membership is the switch.** sshd reads it at every
  login; nothing is reloaded.
- `Match User cg-site-*`: `ChrootDirectory %h` (the site root, root-owned),
  `ForceCommand internal-sftp -d / -P symlink,hardlink@openssh.com`, no
  forwarding, `PasswordAuthentication yes`, `PermitEmptyPasswords no`.
- `UsePAM yes` (Ubuntu's default): a locked password (`!` prefix) does not stop
  a key (measured), which is why the deny group exists.

## 2. The deny group

- `SetAccess(user, allow)`: `allow=false` adds the user to `cg-sftp-off`
  (idempotent), `allow=true` removes it (a user already out is
  `host.ErrNotMember`, which is success). Any other accounts error is
  returned: SFTP must never be reported on while the user is still denied.
- The group is ensured (`groupadd -f --system`) the first time this process
  needs it, not on every call; a failure is returned and retried next time.
  A membership change that finds the group gone (`host.ErrNoSuchGroup`)
  ensures it again and retries once.
- Site creation calls `SetAccess(user, false)`: a new site starts with SFTP
  off, keys included.

## 3. SetSFTP

Params `{site, password, enable}`; result `{"sftp": "enabled", "user": <system
user>}` or `{"sftp": "disabled"}`.

- **Disable**: deny group first, then `Accounts.LockUser` (the password stops
  working too, should the group ever be dropped from sshd's configuration).
- **Enable**: `password` shorter than 12 bytes → `invalid` "sftp password too
  short"; a password with a newline, CR or NUL → `invalid` (chpasswd reads
  `user:password` lines). `Accounts.SetPassword` (chpasswd, the value on stdin,
  never in argv; a new hash also clears the lock), then out of the deny group.
  The password is set before access is opened.
- The API stores `sftp_enabled` after the agent answers.

## 4. SSHKeys

Params `{site, action, public_key, fingerprint}`; result `{keys, port}` — the
keys now in the file and the port sshd listens on.

### 4.1 Files

- `<root>/.ssh` is a real folder, **root:<site group> 0750**;
  `<root>/.ssh/authorized_keys` a regular file, **root:<site group> 0640**.
  sshd reads them as the user (group read) and its `StrictModes` accepts a
  root-owned file. The site user can read its keys but cannot change, replace
  or redirect them: a cron job running as the site user once pointed
  `authorized_keys` at `/root/.ssh/authorized_keys` so that the next "add key"
  wrote there as root.
- On every call, before reading: a missing `.ssh` is created as above; an
  existing one is re-owned and re-moded through `sitefs` (no shell `chown`,
  see AGENTS.md on uutils); anything at `.ssh` that is not a folder →
  `conflict`; an `authorized_keys` that is not a regular file (a planted
  symlink) is removed.
- The file is read with `O_NOFOLLOW`, at most 1 MiB, and written by
  `sitefs.Replace` (temporary file, `fchown`, `fchmod`, rename): never written
  through a link.

### 4.2 Keys

- One key per request: `<type> <base64> [comment]`. Several lines → `invalid`.
  Options before the type (`command="…" ssh-ed25519 …`) are not accepted.
- Types: `ssh-ed25519`, `ssh-rsa`, `ecdsa-sha2-nistp256/384/521`,
  `sk-ssh-ed25519@openssh.com`, `sk-ecdsa-sha2-nistp256@openssh.com`. The blob
  must parse as a public key of exactly that type
  (`golang.org/x/crypto/ssh.ParsePublicKey`), else `invalid`.
- The comment is kept as a label: whitespace runs collapsed to one space,
  non-printable characters dropped, at most 120 bytes (cut on a character
  boundary).
- **Fingerprint**: `SHA256:` + unpadded **standard** base64 of the blob's
  SHA-256 — what `ssh-keygen -lf` and sshd's log print. The old agent used the
  URL-safe alphabet, so for most keys the panel showed a fingerprint that
  matched nothing the user could compare it with. A delete still accepts that
  old form.
- Lines of the file that are not a key this code parses (comments, an option
  an operator added by hand) are kept as they are and not listed.

### 4.3 Actions

- `list`: the keys, in file order, and the port.
- `add`: a key whose fingerprint is already there → `conflict` "SSH key
  already exists". Appended as `<type> <base64>[ <label>]`.
- `delete`: by fingerprint (either form); none matching → `not_found` "SSH key
  not found". Every line with that key is removed.
- Anything else → `invalid` `unsupported SSH key action "<a>"`, before any
  file is touched.
- Changes to one site's keys are serialised in the agent (a per-site mutex):
  two adds at once used to read the same file and the second write dropped
  the first key.
- A key change never touches the password. The old agent "unlocked" a locked
  account for keys with `passwd -d`, leaving the site user with an **empty
  password** (pam_unix's `nullok` accepts that for any PAM service). When SFTP
  is off (`site.config.sftp_enabled` false) a key change locks the password
  instead, which also repairs accounts the old code emptied.
- `port`: sshd's first listening port from `sshd -T` (`ops/system`'s parser,
  falling back to the configuration files), 22 when none is found.
- `access` (R-4.5): every key listed carries `"sftp"` or `"shell"` (§5.2).
  `add` takes `access` (empty is `"sftp"`; anything else → `invalid`).

## 5. Site shell (R-4.5)

An administrator can give a site a shell over SSH: `bash` in the site's
folder, as the site's uid, with `git`, `composer`, `wp`, `php` (and `node`
when the server has it). SFTP keeps working exactly as in §1-4.

### 5.1 Why a second login name

sshd decides `ChrootDirectory` and `ForceCommand` per **user** (a `Match`
block), never per key, and a chroot cannot be entered unprivileged. So the
SFTP login stays what it is — `cg-site-<id>`, chrooted, `internal-sftp` —
for every key, and the shell has a login of its own:

- **`cg-shell-<id>`** (`site.ShellUser`): a second entry in the user
  database with the **same uid and primary group** as `cg-site-<id>`
  (`useradd --system --non-unique --uid <uid> --gid cg-site-<id>`), home the
  site root, password locked, supplementary group `cg-shell` only, login
  shell `<BinDir>/cg-site-shell` (§5.4). Same uid: files the shell writes
  belong to the site user at once, and the uid's lookup (`id`, `ls -l`,
  bash's prompt) prints `cg-site-<id>`, the entry before it.
- It exists only while the site's shell is on. It is in the deny group
  (§2) whenever SFTP access is off, like the site user: the access switch
  covers SFTP and the shell, keys included.

### 5.2 Key types

- `<root>/.ssh/authorized_keys` holds **every** key, as before: any key
  signs in to the chrooted SFTP login.
- `<root>/.ssh/shell_keys` (root:<site group> 0640, written like
  `authorized_keys`, §4.1) holds the keys of type `shell`; sshd reads it
  for `cg-shell-*` only (§5.3). A key is `shell` when its blob is in that
  file, `sftp` otherwise. A key of type `shell` on a site without the shell
  is an SFTP key until the shell is turned on.
- `add` with `access: "shell"` appends the key to both files; `delete`
  removes it from both. A line of `shell_keys` whose key is not in
  `authorized_keys` is dropped on the next write (a key is listed, and
  deleted, through `authorized_keys`).

### 5.3 sshd

The agent owns `/etc/ssh/sshd_config.d/11-cloudground-shell.conf`
(`ShellDropIn`):

```
Match User cg-shell-*
    AuthorizedKeysFile .ssh/shell_keys
    ChrootDirectory none
    PasswordAuthentication no
    KbdInteractiveAuthentication no
    PermitEmptyPasswords no
    PermitUserRC no
    DisableForwarding yes
    AllowTcpForwarding no
    X11Forwarding no
    PermitTTY yes
    ForceCommand cloudground-site-shell
```

Keys only; no forwarding of any kind; `~/.ssh/rc` is not run. The
`ForceCommand` (R-10.5) makes every session of a shell login, whatever the
client asks (a command, a subsystem such as `sftp`) and whatever a global
`Subsystem` or `ForceCommand` of `sshd_config` says, run the login shell,
the gate client, as `cg-site-shell -c cloudground-site-shell`; the client
reads its command from `SSH_ORIGINAL_COMMAND` (unset: a session). Without
it a global `Subsystem sftp internal-sftp` gave the shell login an SFTP
session with no chroot, outside the sandbox. Ensured on
each SetShell(enable): when the file differs from that text it is
replaced (temporary file, rename), `sshd -t` checks the whole
configuration, and on failure the previous file is put back (or the new
one removed when there was none) and the error returned: sshd is never
reloaded on a configuration it refuses. Then `ssh.service` is reloaded
(`sshd.service` on systems that name it so); a unit that is not running
(socket activation) reads the file when it starts. The installer's
`10-cloudground-sftp.conf` (`Match User cg-site-*`) is untouched.

### 5.4 The session: a sandboxed service in the site's slice

A shell must not be more than the site's PHP is. So the session does not
run under sshd at all: it runs as a **transient systemd service** with the
PHP master's sandbox, in the site's slice.

The login shell of `cg-shell-<id>` is `<BinDir>/cg-site-shell`, the **gate
client** (`internal/siteshell`), running as the user. Since R-10.5 it is a
binary of its own (`cmd/cg-site-shell`, in the release and installed by
`cgctl install`; a self-update installs it only where the name is already a
regular file), whose only imports of this module are `internal/siteshell`
and `internal/siteshell/early`. On a server whose name is still the link
to `panel-agent` (upgraded by self-updates only), `panel-agent` started
under that name is the client, as before; the agent's `ensureGateLink`
keeps an executable regular file at the name and makes the link only when
the name is missing or holds anything else. sshd starts it (through the
`ForceCommand` of §5.3: `cg-site-shell -c cloudground-site-shell`, the
client's command in `SSH_ORIGINAL_COMMAND`). It runs nothing itself. It
makes itself non-dumpable (`prctl(PR_SET_DUMPABLE, 0)`): it runs as the
site uid outside the sandbox for the whole session, and the shell (same
uid) could otherwise reach the host's mounts through its `/proc/<pid>/root`
(§5.7). In the separate binary that happens in package `early`'s init
(which depends on `syscall` alone), before any code of this project runs;
in `panel-agent` the agent's whole package initialisation ran first. Then:

1. it connects to the agent's gate socket `/run/cloudground/shell.sock`
   (root:`cg-shell` 0660: only shell logins reach it; PHP and the shell's
   own processes run as the site user, who is not in `cg-shell`) and sends
   one JSON line, `{has_command, command, term, env}` (`env`: `LANG`,
   `LANGUAGE`, `LC_*`, `COLORTERM` only), with its stdin, stdout and stderr
   attached (`SCM_RIGHTS`);
2. it waits for `ok <unit>` (or `error <reason>`: it prints `the site
   shell is not available` and exits 1);
3. it stays until `exit <status>` and exits with that status. A hang-up,
   `SIGTERM`, `SIGINT` or `SIGQUIT` sends `stop`, which stops the service.
   The agent going away ends it with 255.

The **gate** (agent side), for each connection:

- at most 64 connections at once (one more is closed unanswered); the
  request line within 2 s, at most 64 KiB, with exactly three descriptors;
- the peer's credentials (`SO_PEERCRED`); the uid must be, in the user
  database, exactly one `cg-site-<id>` and its `cg-shell-<id>`, and the
  site user's home a folder directly under the sites root (that is the
  site's root and domain); at most 8 open sessions per site and 120 started
  per minute (tools open one session per command: 20 a minute refused a
  single deploy run and the e2e section; two a second still bounds the
  transient units a site makes systemd create);
- the request checked (`siteshell.Request.Check`: no NUL in the command, a
  plain terminal name, only the language variables);
- **the terminal**: when stdin is a pseudo-terminal slave owned by the
  peer's uid and `/dev/pts/<n>` is that very device, the session gets it
  through systemd (`TTYPath=/dev/pts/<n>`, `StandardInput=tty-force`,
  `StandardOutput=tty`, `StandardError=tty`): systemd opens it and makes it
  the service's controlling terminal, taking it from sshd's session, so
  bash has job control, ^C reaches the foreground job and a resize reaches
  it as `SIGWINCH`. Otherwise (no terminal: `ssh host cmd`, scp, sftp) the
  descriptors themselves are handed to systemd (`systemd-run --pipe`);
- `systemd-run --wait --collect --service-type=exec
  --unit=cg-shell-<id>-<pid>.service --slice=cg-site-<id>.slice
  --uid=cg-site-<id> --gid=cg-site-<id>`, every line of
  `phpfpm.Sandbox(site)` as a `-p` property (the very list the PHP
  master's unit is rendered from) and of `ShellHardening` (§5.7),
  `InaccessiblePaths=-/etc/profile.d/80-systemd-osc-context.sh`
  (`ShellQuiet`: systemd's terminal integration reads
  `/proc/sys/kernel/random/boot_id` and `uuid`, which `ProcSubset=pid`
  hides, and every login printed two "No such file or directory" lines;
  `/etc/profile` skips a profile.d file it cannot read), `IgnoreSIGPIPE=no`,
  `TimeoutStopSec=5`, the working directory `<root>/current` for a login
  (when it resolves; else the root), and the environment: `HOME=<root>`,
  `USER`/`LOGNAME` the site user, `SHELL=/bin/bash`,
  `PATH=/usr/local/bin:/usr/bin:/bin`, `PWD`, `TERM` (`dumb` when none),
  the language variables, and `TMPDIR`, `XDG_CACHE_HOME`,
  `XDG_CONFIG_HOME`, `COMPOSER_HOME`, `WP_CLI_CACHE_DIR` under the
  session's private `/tmp`. The command: `/bin/bash -l`, or `/bin/bash -c
  <command>`;
- `ok <unit>`, then `exit <status>` (systemd-run's, which is the shell's)
  when the service ends; a `stop` or the client going away stops the unit.

So a shell sees what the site's PHP sees: `/srv/sites` with only its own
tree in it, its logs, no `/home` or `/root`, none of the panel's,
nginx's, MariaDB's or Let's Encrypt's configuration; other users'
processes invisible (`ProtectProc=invisible`; the site's own PHP
processes, same uid, are visible); a private `/tmp`, `/dev` and `/dev/shm` (phpfpm SPEC §4.1);
`NoNewPrivileges` and no capabilities (`sudo`, `su` and every setuid
binary gain nothing); no namespaces (`unshare` fails). Everything the
session starts is in the service's cgroup and ends with it (a `nohup`ed
process included), with `SetShell(disable)`, and with the slice (site
deletion).

Why `systemd-run` rather than `StartTransientUnit` by hand: it parses
unit-file syntax, so the sandbox is passed as the same text lines the PHP
unit is rendered from and cannot drift through a hand-written D-Bus type
mapping; and `--pipe` passes descriptors as systemd expects. Why not a
scope around sshd's process (the first design): a scope only moves a
cgroup; it cannot add a mount namespace, `NoNewPrivileges` or a private
`/tmp` to a process already running.

**The user manager.** sshd's PAM session (`pam_systemd`) would start
`user@<uid>.service` for the login, outside the slice. While the shell is
on, the agent masks it (`/etc/systemd/system/user@<uid>.service ->
/dev/null`, then a daemon reload, and stops one already running): logind
starts sessions without a user manager when it cannot start one. The mask
goes when the shell is turned off and with the site.

### 5.5 SetShell

Params `{site, enable}`; result `{"shell": "enabled", "user":
"cg-shell-<id>"}` or `{"shell": "disabled"}`. The server holds the site's
lock (`LockLiveSite`). The API stores `shell_enabled` after the agent
answers; only an administrator may call it.

- **Enable**: `<root>/src`, the site user's (0750; the root is root's, so
  the shell could not make a folder there), for checkouts that become
  releases (§5.6); the gate link (`<BinDir>/cg-site-shell -> panel-agent`, a
  relative link; something else at that name is replaced), the `cg-shell`
  group, the sshd drop-in (§5.3), the account (an existing
  `cg-shell-<id>` with another uid is a `conflict`), the deny group as
  SFTP access says, the user-manager mask. Each step is idempotent: a
  repeated enable repairs what is missing.
- **Disable**: every `cg-shell-<id>-*.service` is stopped (open shells
  end, with everything they started), the account is removed (`userdel
  --force`: the uid's PHP processes would make a plain `userdel` refuse,
  and the alias owns no group or home of its own), the mask is removed.
  `shell_keys` stays: the keys keep their type.
- **SetSFTP** applies the deny group to `cg-shell-<id>` too when it exists;
  turning access off stops the open shells.
- Enabling also makes `/run/sshd` (0755, root) when it is missing: `sshd
  -t` needs it, and a socket-activated sshd makes it only on its first
  connection.
- **Site deletion** (`RemoveShell`, called by ops/sites before the site
  user is deleted): disable, ignoring what is already gone. The site
  user's group can then go (the alias had it as its primary group).

### 5.6 From a checkout to a release

Nothing new in the agent: `DeployRelease` (ops/release §3) already takes a
`SourceDir` inside the site's root. A clone or pull in `~/src/<app>` is
published with `POST /api/sites/{id}/deployments {"source_dir":
"<root>/src/<app>"}` (or `cgctl deploy <id> <dir>`), then promoted and
rolled back like any release. The copy runs as root through
`sitefs.CopyTree` (no link followed); the repository's `.git` goes into
the release, and nginx refuses dot paths. The panel has no form for it yet
(Fase 8).

The build runs nothing of the checkout: no `git` (a root `git` in a
repository the site user controls would run its hooks and its
`core.fsmonitor`), no `composer`, no hook; links are copied as links
(`release/checkout_test.go`).


### 5.7 Crontabs, and the site's code outside the sandbox

- The shell cannot make a crontab: `crontab` is setgid `crontab`, and under
  `NoNewPrivileges` the setgid gains nothing, so it cannot read or write
  cron's spool. A site's schedule is root's `/etc/cron.d` file (ops/cron),
  whose jobs run in the same sandbox. Nothing else gives the site uid a
  crontab, so the shell needs no `cron.deny` (an earlier build listed the
  site users there, which also made root's `crontab -u <user>` fail; the
  gate removes that block when the agent starts) and nothing to kill
  outside the slice: a shell cannot leave its service's cgroup.
- `/proc/<pid>/root` of a process of the same uid that runs outside the
  sandbox would show the host's mounts. Everything the agent runs of a site
  is in the sandbox: PHP, the shell, cron jobs (scheduled and run now) and
  wp-cli (ops/wp, staging, provisioning). What runs as the uid outside it is
  the gate client, which makes itself non-dumpable
  (`prctl(PR_SET_DUMPABLE, 0)`) before anything else, and sshd's own
  session processes, which OpenSSH makes non-dumpable.
- The shell adds to the PHP master's sandbox `RestrictSUIDSGID=yes`,
  `PrivateIPC=yes`, `SystemCallFilter=@system-service` and
  `SystemCallErrorNumber=EPERM` (`ShellHardening`; PHP keeps its sandbox
  until a benchmark says otherwise). Not `RemoveIPC`: it removes every IPC
  object of the uid when the unit stops, and the uid is also the site's
  PHP-FPM's; with `PrivateIPC` and `PrivateDevices` the shell's IPC is its
  own anyway.
  The same list (`phpfpm.RunHardening`) is taken by cron jobs and wp-cli
  since R-10.5 (phpfpm SPEC §4.1).

### 5.8 Local sockets: nothing trusts loopback alone (R-10.5)

Every site's PHP, its shell, its cron jobs and wp-cli share the host's
network namespace: they can connect to every loopback TCP port and every
abstract Unix socket of the box. So no service of a box trusts a peer for
being local; each one names what it trusts instead.

Loopback TCP (127.0.0.1 / ::1):

| Port | Who | What it trusts |
| --- | --- | --- |
| 9080 | panel-api, the connector's listener (`CLOUDGROUND_LOCAL_LISTEN`) | a site's own connector token (bearer, per site); wrong tokens are limited per address (401, then 429) |
| 5252 | panel-api, **development only** (`CLOUDGROUND_DEV`) or without socket activation | nothing local: in production the panel is the Unix socket `/run/cloudground/api.sock` (cloudground:www-data 0660, nginx only), and on a TCP connection `X-Real-IP` is ignored (`clientIP`) |
| 3306 | MariaDB (`bind-address 127.0.0.1`) | a password per database account (`site_<id>` for its own database only); `root` signs in only over the Unix socket with `unix_socket` authentication (the uid must be root) |
| 6379 | Redis | not running (the installer turns it off); reserved by the routing guard anyway |
| 53 | systemd-resolved (127.0.0.53/54) | resolver only |

A site's routing block cannot proxy to any of them, nor to another site's
listener (internal/engine SPEC §7.3).

Unix sockets with a path (the file's owner and mode decide who connects,
then each checks its peer):

| Socket | Mode | Then |
| --- | --- | --- |
| `/run/cloudground/agent.sock` | root:cloudground 0660 | `SO_PEERCRED` uid root or the API's user, and the shared token (agentrpc SPEC §2) |
| `/run/cloudground/shell.sock` (the gate) | root:cg-shell 0660 | `SO_PEERCRED`: a shell login whose site has a shell (§5.4) |
| `/run/cloudground/api.sock` | cloudground:www-data 0660 | nginx is the one client; it sets `X-Real-IP` |
| `/run/cloudground/php/<user>/php.sock` | the pool's listen owner and mode (phpfpm) | nginx (www-data, in the site's group) |
| `/run/mysqld/mysqld.sock` | MariaDB's | `unix_socket` for root, passwords for the rest |

Abstract Unix sockets: CloudGround creates none (none of its programs
binds a name starting with `@`); what the distribution's services bind
there (systemd's, D-Bus's) authenticate by `SO_PEERCRED` themselves.
