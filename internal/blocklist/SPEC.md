# internal/blocklist — the server's block list (specification)

The one list of who may not reach the server, and the rules that fill it:
addresses blocked by hand or for repeated failed sign-ins (issue #21), and
the addresses that are never blocked (the allowlist). Pure: no file system,
no processes. The panel (`internal/api/blocklist.go`) keeps the list and
decides; the agent (`internal/agent/ops/blocklist`) reports failures and
enforces it.

Sources: the issue's "Done when"; fail2ban's defaults and filters (sshd,
wordpress), which this replaces; the e2e section `blocklist`; the measurements
listed in `bench/blocklist.md`.

## 1. Addresses

`ParsePrefix(s)` reads one entry the way an operator types it: an IPv4 or
IPv6 address (a single host: `/32` or `/128`) or a CIDR. The result is
masked (`203.0.113.9/24` is `203.0.113.0/24`) and unmapped (`::ffff:1.2.3.4`
is `1.2.3.4`). Refused, with the message shown to the operator:

| Input | Message |
| --- | --- |
| not an address or a CIDR | `"<s>" is not an IP address or a network in CIDR form` |
| an IPv4 network wider than `/8`, an IPv6 one wider than `/16` | `<p> is wider than /8 (IPv4) or /16 (IPv6): too broad to block` |
| a zone (`fe80::1%eth0`) | the first message |

The width bound applies to blocks only: `ParsePrefix` returns the prefix and
`CheckBlockWidth` the refusal, so an allowlist entry may be as wide as the
operator likes.

**Unit.** A failure counts against, and a ban blocks, the address's unit:
the address itself for IPv4 (IPv4-mapped included), its `/64` for IPv6 — a
single host or customer is usually given a whole `/64`, and one address each
would hand an attacker 2^64 fresh ones (internal/api SPEC §2.7 counts the
panel's sign-in limits the same way).

**Never blocked** (`Exempt`): loopback (`127.0.0.0/8`, `::1`), the server's
own addresses, and every allowlist entry. A failure from one of them is not
counted; a ban is never made for one.

## 2. Settings

| Field (JSON) | Default | Bounds | Meaning |
| --- | --- | --- | --- |
| `enabled` | true | | automatic blocking; blocks made by hand are enforced either way |
| `max_failures` | 6 | 3–100 | failures from one unit within the window that block it |
| `window_minutes` | 10 | 1–1440 | the window |
| `ban_minutes` | 60 | 1–43200 (30 days) | how long an automatic block lasts |

The defaults are fail2ban's (5 failures in 10 minutes) with one failure of
room: a person who mistypes a password five times is not blocked, while a
guesser reaches the threshold in seconds. A ban lasts an hour rather than
fail2ban's ten minutes: bots retry on a schedule longer than ten minutes.
`Settings.Validate` refuses a value outside its bounds, naming the field.

## 3. Failures and the tracker

A failure is `{address, source, count}`. The sources:

| Source | What counts | Where it is seen |
| --- | --- | --- |
| `ssh` | sshd: `Failed password for <user> from <a>`, `Failed keyboard-interactive/pam for <user> from <a>`, `Invalid user <u> from <a>`, `Connection closed by authenticating user <u> <a> port <p> [preauth]` | the journal of `ssh.service`/`sshd.service`, from root only (§4.1) |
| `wordpress` | a POST that WordPress answered as a failed sign-in (wp-login.php, xmlrpc.php, application passwords): the connector's `X-CloudGround-Auth: failed=<n>` | nginx's log line to the agent (§4.2) |
| `panel` | a wrong password or a wrong two-factor code at the panel's sign-in | the panel itself |

`Failed password for invalid user` is not counted: sshd logged `Invalid
user` for the same attempt already. `count` is at most 100 per line.

`Tracker.Fail(addr, source, count, now, settings)` adds `count` failures to
the address's unit, forgets those older than the window, and reports a ban
when the unit holds at least `max_failures`; the unit then starts again from
zero. It holds at most 50,000 units: when full, units with nothing in the
window are dropped, and a new unit beyond that is not counted (memory stays
bounded under a flood of fresh addresses; a unit already counting keeps
counting). An exempt address, or settings with `enabled` off, count nothing.

## 4. Parsers

### 4.1 sshd (`SSHFailure`)

Input: one journal entry's `MESSAGE`. Output: the address and 1, or nothing.
The address must parse as an IP (`netip.ParseAddr`, unmapped); a line where
it does not is ignored. The agent reads only entries whose trusted fields
say they come from root (`_UID=0`) in the SSH unit (`_SYSTEMD_UNIT`
`ssh.service` or `sshd.service`): any local user can write a line that looks
like sshd's to the journal, and would otherwise block any address.

### 4.2 nginx (`NginxFailure`)

Input: one syslog datagram from nginx, `<PRI>TIMESTAMP cloudground: <addr>
failed=<n>` (`nohostname`, tag `cloudground`, log format `cloudground_auth`:
internal/engine SPEC §3.1). Output: the address and `n` (1–100; `failed`
without a number is 1). Anything else is ignored.

## 5. Enforcement (`NftScript`)

The block list is enforced by netfilter, before any program sees a packet:
one table `inet cloudground`, independent of ufw (which may be off: #14),
replaced as a whole in one `nft -f` transaction:

```
table inet cloudground
delete table inet cloudground
table inet cloudground {
	set allow4 { type ipv4_addr; flags interval; elements = { … } }
	set allow6 { type ipv6_addr; flags interval; elements = { … } }
	set block4 { type ipv4_addr; flags interval; elements = { … } }
	set block6 { type ipv6_addr; flags interval; elements = { … } }
	chain input {
		type filter hook input priority -10; policy accept;
		iif "lo" accept
		ip saddr @allow4 accept
		ip6 saddr @allow6 accept
		ip saddr @block4 drop
		ip6 saddr @block6 drop
	}
}
```

- The first two lines make the replacement atomic: the table is created if
  missing, deleted, and defined again, all or nothing.
- Each set holds its family's prefixes, sorted, with every prefix that
  another one of the same set already covers left out (an interval set
  refuses overlapping elements). An empty set has no `elements` line.
- `accept` here ends only this table's chain: ufw's own rules still run
  after it. `drop` is final.
- Priority -10 runs before ufw's (iptables-nft, priority 0).
- Every packet of a blocked address is dropped, an established connection's
  too: a guesser holding one HTTP/2 connection is cut off like a new one.
  The lookup is an interval-set match per packet; its cost on cached traffic
  is in `bench/blocklist.md`.

Why netfilter and not nginx or fail2ban: one place blocks SSH, the sites
and the panel alike; a dropped packet costs nginx and sshd nothing (no TLS
handshake, no log line); and the kernel already has everything (nftables is
what ufw drives on Ubuntu 24.04 and later). fail2ban would add a Python
daemon reading the same logs, with its own state and configuration, beside
the panel's.

## 6. Countries (#25)

A site may refuse, or serve only, some countries (internal/engine SPEC
§4.7). The data is DB-IP's **IP to Country Lite**
(<https://db-ip.com/db/download/ip-to-country-lite>), licensed **CC BY
4.0**: free to download without an account or a key, and to redistribute
with attribution, which every rendered rules file, the panel's country
picker and the README carry ("IP Geolocation by DB-IP"). It is a CSV of
`start,end,country` rows, IPv4 and IPv6, about 710,000 rows (31 MB, 4.5 MB
gzipped, October 2026), published monthly.

- `ValidCountry(c)`: two capital letters (ISO 3166-1 alpha-2, as DB-IP
  writes them; `ZZ` is its "unknown").
- `RangePrefixes(lo, hi)`: the shortest list of prefixes covering exactly
  the range (the largest aligned block that fits, again from the next
  address), in either family; nothing for a backwards or mixed range.
- `CountryPrefixes(r, want)`: the prefixes of each wanted code, sorted. A
  row that is not three fields, or a range that does not parse or runs
  backwards, is an error (the data becomes configuration); so is a file of
  fewer than 1,000 rows (not the database).

Measured on the October 2026 file: the United States are 92,286 ranges,
China 10,041. The prefixes a rule renders, and what a large country costs
nginx's memory and reload, are in `bench/blocklist.md` (to measure).

## 7. Tests

`blocklist_test.go`: §1 (parse, mask, unmap, the width bound, the unit),
§2 (bounds), §3 (the window, a ban resets, exempt addresses, the bound on
units, `enabled` off), §4 (each sshd line, the ones that must not count,
nginx datagrams), §5 (the script byte for byte, covered prefixes dropped,
empty sets). `geo_test.go`: §6 (ranges of both families, the whole space,
one address, the CSV and its refusals). The e2e sections `blocklist` and
`site blocking` check it on a server.
