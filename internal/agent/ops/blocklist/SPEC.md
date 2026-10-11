# ops/blocklist — failed sign-ins in, the block list out

The agent's half of the server's block list (internal/blocklist SPEC, issue
#21): it reads the failed sign-ins only root can see, hands them to the
panel, and enforces the list the panel decides on. It decides nothing: which
address is blocked, for how long and which never is, is the panel's
(internal/api SPEC §7.4).

## 1. Readers

`Serve(ctx)` runs both until ctx ends; each restarts on its own after a
failure (5 s, doubling to a minute), so one that cannot start never stops
the other or the agent.

### 1.1 sshd

`journalctl -f -n 0 -q -o json -u ssh.service -u sshd.service`, one entry
per line. An entry counts only when its trusted fields, which journald sets
from the sender's credentials and cgroup and no process can forge, say root
in the SSH unit: `_UID` `0` and `_SYSTEMD_UNIT` `ssh.service` or
`sshd.service`. Any local user can write a line that reads like sshd's to
the journal (`logger -t sshd …`); counted, it would let a site's shell block
any address. `MESSAGE` (a string, or an array of bytes) goes through
`blocklist.SSHFailure`; source `ssh`.

### 1.2 nginx

A Unix datagram socket, `/run/cloudground/auth-log.sock`, which the
WordPress vhosts log their failed sign-ins to (internal/engine SPEC §3.1,
§4: `access_log syslog:server=unix:…`). A stale socket at the path is
removed first; the new one is `root:www-data` `0620`, so nginx's workers may
write and nobody else of the sites can. Every datagram must carry its
sender's credentials (`SO_PASSCRED`): one from another uid than root or
nginx's user (`www-data`) is dropped, so a process that reached the socket
by some other way still cannot report failures. The datagram goes through
`blocklist.NginxFailure`; source `wordpress`.

### 1.3 What is kept

Per (address, source), the sum of the counts since the last read. Failures
from loopback or one of the machine's own addresses are dropped here (the
panel would not count them either: internal/blocklist SPEC §1). At most
10,000 pairs: a new pair beyond that is dropped until the next read. The
panel reads every 10 s, so that is 1,000 new addresses a second.

## 2. BlocklistFailures

Returns the pairs kept since the last call, sorted by address then source,
and empties them; `ssh_watch` and `nginx_watch` say whether each reader is
running now. Not read-only: the reply is the only copy.

## 3. BlocklistApply

`{block, allow}`, each entry an address or a CIDR (rpc SPEC §3: a block is
no wider than /8 or /16). The agent parses them again, leaves out what
another entry covers and runs `nft -f -` with `blocklist.NftScript` on
standard input: the table `inet cloudground` is replaced in one transaction
(internal/blocklist SPEC §5). Answer: how many prefixes each list became.

- `nft` not installed → `unavailable`, `nftables is not installed on this
  server: apt install nftables`. The installer installs it (internal/install
  SPEC §3); a server installed before it lacks it until then.
- `nft` refuses the script → `internal` with nft's message as the detail;
  the table is as it was (the transaction is all or nothing).

## 4. Country data (#25)

`CountryPrefixes(ctx, codes)`, for ops/sites when it renders a site that
refuses or serves only some countries (internal/engine SPEC §4.7):

- The file is `/var/lib/cloudground-agent/geo/dbip-country-lite.csv.gz`
  (root's, 0644). Missing, or older than 35 days (DB-IP publishes monthly),
  it is downloaded first: `https://download.db-ip.com/free/dbip-country-lite-<YYYY-MM>.csv.gz`
  for this month, then last month's when this one is not out yet; 2-minute
  timeout, at most 64 MiB; it must unpack and read as the database
  (internal/blocklist SPEC §6) before it replaces the old one (written
  beside it, then renamed). No account and no key.
- A refresh that fails keeps the file there is (logged); with no file and
  no download the answer is `unavailable`, `could not download the country
  data from db-ip.com`, and the render fails with the site as it was.
- A code that is not two capital letters is `invalid`.
- The answer is the prefixes of every code, by code; the last answer is
  kept for the same file and codes (a site's every render reads the same).
- One read or download at a time.

## 5. Removal

`cgctl uninstall` deletes the table (`nft delete table inet cloudground`,
internal/install SPEC §9) with everything else the panel set up.

## 6. Tests

`blocklist_test.go`: the journal reader counts an sshd failure from root in
the SSH unit and ignores the same message from another uid or unit; the
datagram reader counts a line it received with root's credentials over a
real socket and refuses the rest; loopback and own addresses are dropped;
the bound on pairs; a read empties; the apply's script and its refusals,
and nft missing. `countries_test.go`: the download (last month's when this
month's is missing), the cache, a refresh that fails keeping the old file, no
data being unavailable, and a download that is not the database not kept.
The e2e sections `blocklist` and `site blocking` check it on a server.
