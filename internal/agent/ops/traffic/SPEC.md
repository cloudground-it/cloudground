# ops/traffic — reading the access logs into statistics (specification)

The agent's half of #33 (internal/api SPEC §11.8): it reads each site's
access log incrementally into internal/traffic's aggregates and answers
them. Root reads the logs (`<LogRoot>/<domain>/access.log`, root and the
site's group); the aggregates live in `Dir` (`/var/lib/cloudground-agent/traffic`,
0700), one file per site, `<id>.json` (0600, written atomically).

## 1. TrafficCollect

`{sites: [{id, domain}]}` → `{sites: [{id, read, lines, skipped}]}`; one
call at a time (a second waits). For each site:

1. Its state: the file, or a new one (offset 0: the current log is read
   from its start, so a site has statistics from the day it is first seen).
2. The log is opened without following a link and must be a regular file
   (else the site is skipped, its state kept).
3. **Rotation.** The installer's logrotate copies and truncates
   (`copytruncate`, `delaycompress`: install SPEC step 27). A log shorter
   than the offset, or with another inode than the one recorded, was
   rotated: the rest of `access.log.1`, from the offset, is read first
   when that file is at least that long (and has the recorded inode, for
   a rename), then the new log from 0.
4. At most `MaxRead` (32 MiB) is read from the offset; only whole lines are
   counted and the offset moves past the last line feed (a line being
   written waits for the next round; a single line longer than the whole
   read is skipped).
5. Each line goes through `traffic.ParseLine` and `Site.Add`; then
   `Settle`, and the state is written.

The files of sites not in the list are removed: the panel sends every site
it has. A failure for one site (a read error) is logged and reported in
its row; the others go on.

**Cost.** The parse is a few string scans per line, run on the panel's
five-minute schedule, never on a request; the read is bounded per site
and round. The state is a few tens of KiB per site.

## 2. TrafficStats

`{id, domain, days}` → `traffic.Stats` for the last `days` days (1–90) of
the site's file, with `collected_at` the last round that read its log.
A site with no file yet answers zero-filled days. Read-only.

## 3. Tests

`traffic_test.go`: a log read across rounds, a partial last line, a
copytruncate rotation that loses nothing, a rename rotation, a link
refused, files of dropped sites removed, no address in the state file.
