# Parity: `scripts/e2e-verify.sh` → `e2e/` (Go)

Every check of the bash suite, by line number, mapped to the Go subtest that
ports it. Line numbers are those of `scripts/e2e-verify.sh` at the commit that
added this file (978 lines, after the R-1.2 checks were added).

How to read it:

- A Go subtest's full name is `TestE2E/<section>/<check>`. `go test` prints
  spaces as underscores, so `site types/static/static provisions` appears as
  `TestE2E/site_types/static/static_provisions`.
- The bash `check "<name>" "$got" "$want"` prints `<name> (<got>)`; the Go
  subtest is named `<name>` and logs `(<got>)`, or `got … want …` on failure.
- A bash `cond && ok "<a>" || bad "<b>"` pair is **one** check. The Go subtest
  is named after the passing text `<a>`; on failure it prints `<b>`. Where the
  passing text carried a value (`woocommerce installed by provisioner ($ver)`),
  the value is dropped from the name and logged instead.
- A bare `bad` on a fixture or setup branch ("alt" below) is a check only when
  that branch is taken, in both suites. The Go subtest has the bad text's name.
- Rows marked *per user* expand to one check per active PHP site at run time,
  in both suites.

Since 2026-10-10 the sections are smaller: `wordpress features` became
`wordpress cache` … `wordpress teardown` (with `firewall`), and the site
types' checks sit directly under `site types` (`site types/static
provisions`). The check names below are unchanged; `SECTIONS.md` has the
current sections and the fixtures they share. As of the parity run:

Sections (Go): `site types`, `woocommerce`, `wordpress features`,
`migration import`, `security`, `woocommerce security`, `R-0.7 reconcile`,
`woocommerce teardown`, in the bash order. Go only, added later: `R-2.4
services` and `R-3.9 certificates and panel`, run before the WooCommerce
teardown.

## Site types (`run_type`, lines 132–152, called at 154–172)

Each of the five calls (static 154, php 157, proxy 165, laravel 169,
wordpress 171) runs these, as `site types/<label>/…`:

| Line | Bash check | Go subtest |
|---|---|---|
| 136 | `$label: create rejected` (alt) | `site types/<label>/<label>: create rejected` |
| 138 | `$label provisions` | `site types/<label>/<label> provisions` |
| 144 | `$label serves ($code, app not deployed OK)` (laravel, `expect=ok`) | `site types/laravel/laravel serves` |
| 146 | `$label serves HTTP` (other types) | `site types/<label>/<label> serves HTTP` |
| 148 | `$label no PHP fatal` | `site types/<label>/<label> no PHP fatal` |
| 150 | `$label delete accepted` | `site types/<label>/<label> delete accepted` |
| 151 | `$label teardown completes` | `site types/<label>/<label> teardown completes` |

5 types × 5 = **25 checks** (plus the alt).

Between the php and proxy types (line 162), outside `run_type`:

| Line | Bash check | Go subtest |
|---|---|---|
| 162 | `R-1.2: the distribution's php$PHPV-fpm is not running` | `site types/R-1.2: the distribution's php<ver>-fpm is not running` (with `CG_E2E_PARALLEL=1`: after all five types) |

## WooCommerce (lines 177–192) → `woocommerce/…`

| Line | Bash check | Go subtest |
|---|---|---|
| 181 | `woocommerce provisions` | `woocommerce provisions` |
| 185 | `woocommerce installed by provisioner ($ver)` / `provisioner did not install WooCommerce` | `woocommerce installed by provisioner` |
| 187 | `woocommerce home serves (was the 500 bug)` | same |
| 188 | `woocommerce no PHP fatal` | same |
| 190 | `woocommerce /shop resolves` | same |
| 191 | `woocommerce /cart resolves` | same |

## WordPress feature suite (lines 194–660) → `wordpress features/…`

| Line | Bash check | Go subtest |
|---|---|---|
| 198 | `feature-site provisions` | same |
| 203 | `page cache serves HIT` | same |
| 206 | `cache pre-compressed` | same |
| 215 | `a page warmed with gzip is a HIT for Chrome` | same |
| 216 | `a page warmed with gzip is a HIT for Safari` | same |
| 219 | `client without gzip gets decompressed HTML` | same |
| 223 | `deflate client did not poison the cache` | same |
| 238 | `an edit is visible to a browser right after purge` | same |
| 241 | `a utm-tagged URL is a HIT` | same |
| 242 | `any other query string still bypasses` | same |
| 248 | `a response setting a cookie is not cached` | same |
| 253 | `object-cache enable accepted` | same |
| 256 | `magic login issues URL` | same |
| 259 | `magic login token is in the URL fragment` | same |
| 262 | `a wrong magic token is refused` | same |
| 264 | `the magic link signs in (after a wrong attempt)` | same |
| 266 | `a used magic link is refused` | same |
| 267 | `the magic token is not in the access log` | same |
| 270 | `cron create accepted` | same |
| 274 | `cache purge accepted` | same |
| 301–302 | `template part edit purges the whole site` | same |
| 307 | `staging clone accepted` | same |
| 308 | `staging clone completes` | same |
| 313 | `staging asks outsiders for a password` | same |
| 316 | `staging opens with its password` | same |
| 318 | `loopback without the internal key gets no staging` | same |
| 320 | `the staging vhost (it holds the key) is root-only` | same |
| 322 | `the panel's own requests pass with the internal key` | same |
| 323 | `the API does not expose the staging password hash` | same |
| 325 | `certificate status answers` | same |
| 326 | `alerts answer` | same |
| 327 | `the drift check answers` | same |
| 335 | `every WordPress site runs the current connector` | same |
| 346 | `a custom certificate is accepted` | same |
| 347 | `nginx serves the custom certificate` | same |
| 348 | `the custom certificate can be removed` | same |
| 349 | `removing it stops serving the custom certificate` | same |
| 366 | `race: the delete is accepted` | same |
| 374 | `race: teardown completes` | same |
| 375–379 | `a certificate racing the delete leaves nothing of $RS in nginx` | same (`$RS` expanded, `certrace.<dash>.sslip.io`) |
| 380–385 | `the deleted site's vhost stays gone` | same |
| 388 | `race: the site for the certificate/delete race did not provision` (alt) | same |
| 403 | `R-1.2: a custom certificate is installed on $CS` | same (`$CS` expanded, `custcert.<dash>.sslip.io`) |
| 404 | `R-1.2: its key is on disk before the delete` | same |
| 405 | `R-1.2: the delete is accepted` | same |
| 406 | `R-1.2: teardown completes` | same |
| 407 | `R-1.2: the deleted site's custom certificate directory is gone` | same |
| 410 | `R-1.2: the site for the custom-certificate delete did not provision` (alt) | same |
| 416 | `an SSH key is added` | same |
| 421 | `SFTP off refuses the site's keys` | same |
| 422 | `SFTP can be turned on` | same |
| 423 | `SFTP signs in with the key` | same |
| 424 | `SFTP can be turned off` | same |
| 425 | `turning SFTP off refuses the key` | same |
| 426–430 | `the site user cannot replace its authorized_keys` | same |
| 445 | `R-0.5: object-cache change sent alongside an SFTP change completes` | same |
| 446 | `R-0.5: SFTP change sent alongside an object-cache change is accepted` | same |
| 448 | `R-0.5: object cache stays off after concurrent changes (stored)` | same |
| 449 | `R-0.5: SFTP stays on after concurrent changes (stored)` | same |
| 450 | `R-0.5: the object-cache drop-in is gone` | same |
| 452 | `R-0.5: SFTP can be turned off again` | same |
| 454 | `R-0.5: object cache can be turned back on` | same |
| 456 | `R-0.5: SFTP off is stored` | same |
| 457 | `R-0.5: object cache on is stored` | same |
| 463 | `PHP errors reach the site's php-error.log` | same |
| 473 | `max_execution_seconds 300 is applied` | same |
| 476–480 | `a 130s PHP request completes under a 300s limit ($slow)` | `a 130s PHP request completes under a 300s limit` |
| 485 | `nginx reopens the site logs` | same |
| 488 | `site crontab runs WordPress's due events` | same |
| 489 | `wp-cron is off in requests` | same |
| 497 | `safe push of identical code passes the guard` | same |
| 503 | `safe push of a 400 ms slower staging is blocked` | same |
| 504 | `a blocked push leaves production untouched` | same |
| 506 | `staging diff shows the file added on staging` | same |
| 514 | `selective push passes the guard` | same |
| 515 | `the chosen folder went live` | same |
| 516 | `nothing outside the chosen folder went live` | same |
| 519 | `staging is recreated from production` | same |
| 520 | `recreated staging starts from production` | same |
| 522 | `staging site not found for safe push` (alt) | same |
| 531 | `backup repository accepted` | same |
| 547 | `backup runs` | same |
| 550–551 | `R-0.2: the backup stored the database as its own restic snapshot ($ndb)` | `R-0.2: the backup stored the database as its own restic snapshot` |
| 554 | `verified restore passes` | same |
| 558 | `in-place restore succeeds` | same |
| 559 | `restore brought the database back` | same |
| 560 | `restore brought the files back` | same |
| 561 | `R-0.2: the restore did not import the dump the site user planted in logs/ (cg_e2e_planted tables)` | same |
| 565 | `restored site serves` | same |
| 566 | `no restore staging left next to the sites` | same |
| 570–571 | `R-0.3: page cache root survives a restore` | same |
| 573 | `R-0.3: the restored site caches again (3rd request after the restore)` | same |
| 579 | `R-0.10: the pre-restore safety snapshot passed its restore test (verify_status)` | same |
| 582 | `staging database push of wp_terms succeeds` | same |
| 583 | `R-0.10: the pre-database-push safety snapshot passed its restore test (verify_status)` | same |
| 585 | `R-0.10: no staging site, the database-push safety snapshot was not checked` (alt) | same |
| 599 | `R-0.9: a backup whose dump does not import fails its restore test (task)` | same |
| 601 | `R-0.9: a backup whose dump does not import is marked verify failed, not passed` | same |
| 603 | `R-0.9 setup: the agent did not run /usr/local/sbin/mariadb-dump (PATH?); the corrupt-dump check did not run` (alt) | same |
| 606 | `R-0.9: restore tests leave no cgv_ scratch database behind` | same |
| 623 | `control: an aged snapshot is in the repository` | same |
| 625 | `retention runs` | same |
| 626 | `retention removed the snapshot beyond the window` | same |
| 627 | `retention kept the newest verified snapshot` | same |
| 629 | `retention fixture: no verified snapshot (…) or the aged snapshot was not created (…)` (alt) | `retention fixture: no verified snapshot or the aged snapshot was not created` |
| 641 | `firewall status answers` | same |
| 643 | `the panel answers on :8443` | same |
| 644–647 | `an unknown hostname on 443 does not get the panel` | same |
| 649 | `the firewall refuses to close 443` | same |
| 651 | `the firewall refuses to drop SSH ($SSHP)` | `the firewall refuses to drop SSH` |
| 653 | `443 is still allowed` (only when ufw is active) | same |
| 658 | `feature-site delete accepted` | same |
| 659 | `feature-site teardown completes` | same |

## Migration import (lines 662–727) → `migration import/…`

| Line | Bash check | Go subtest |
|---|---|---|
| 674 | `migration-site provisions` | same |
| 693 | `migration fixture: could not export the database dump` (alt) | same |
| 699 | `migration import accepted` | same |
| 700 | `migration import succeeds` | same |
| 703 | `migrated files are live` | same |
| 707–709 | `cache connector survives migration` | same |
| 713 | `migrated site serves` | same |
| 714 | `migrated site no PHP fatal` | same |
| 717 | `migrated site caches` | same |
| 723 | `migration inputs cleaned up` | same |
| 725 | `migration-site delete accepted` | same |
| 726 | `migration-site teardown completes` | same |

## Security (lines 729–831) → `security/…`

| Line | Bash check | Go subtest |
|---|---|---|
| 743 | `deploy from outside the site is refused` | same |
| 749 | `deploy through a planted symlink fails` (only when a task was returned) | same |
| 750 | `no host file copied into a release` | same |
| 758 | `control: the tenant's own file is served` | same |
| 759 | `control: the cross-tenant link exists` | same |
| 761 | `cross-tenant symlink is not served` | same |
| 762–766 | `SFTP refuses symlink and hardlink requests` | same |
| 772 | `cross-site write refused ($code)` | `cross-site write refused` |
| 778 | `a user address with a display name is refused` | same |
| 779–785 | `no account was stored for the display-name address` | same |
| 795 | `control: the operator reads its own site's log` | same |
| 796 | `control: an admin reads the server status` | same |
| 797 | `an operator is refused the server status` | same |
| 798 | `an operator is refused the server metrics` | same |
| 799 | `an operator is refused a server-wide log` | same |
| 800 | `an operator is refused /api/logs even for its own site` | same |
| 801 | `an operator cannot read another site's log` | same |
| 806 | `site user cannot open the API socket` | same |
| 807 | `API is not on loopback TCP` | same |
| 815 | `a site cannot see another site's APCu entries` | same |
| 816 | `a site's PHP sees no other site's files` | same (all three branches) |
| 820 | `agent work dir is root-only` | same |
| 823 | `releases/ is root-owned` | same |
| 824 | `site user cannot plant a release` | same |
| 828 | `security fixtures did not provision` (alt) | same |

## WooCommerce security (lines 833–878) → `woocommerce security/…`

| Line | Bash check | Go subtest |
|---|---|---|
| 840 | `the console opens for the first browser` | removed in F4b (Adminer is gone) |
| 841 | `the same browser keeps its session` | removed in F4b |
| 842 | `a second browser with the same link is refused` | removed in F4b |
| 843 | `the console link is not listed` | removed in F4b |
| 846 | `the database console did not launch` (alt) | removed in F4b |
| 849 | `SQL console has no client commands` | same |
| 851 | `SQL console refuses \!` | same |
| 853 | `read-only console cannot write files` | same |
| 855 | `console is confined to the site database` | same |
| 857 | `console still answers ordinary queries` | same |
| 858 | `wp-config.php is unreadable to nginx's user` | same |
| 871 | `an imported SQL file cannot run shell commands` | same |
| 873 | `an imported SQL file is confined to the site database` | same |
| 874 | `an ordinary SQL file still imports` | same |
| 876 | `the imported rows are in the site database` | same |

## R-0.7 (lines 880–967) → `R-0.7 reconcile/…`

`r07_assert` (lines 919–942) runs twice, with `<what>` = `create` (959) and
`delete` (965):

| Line | Bash check | Go subtest |
|---|---|---|
| 921–923 | `R-0.7 (<what>): the static site was not re-rendered by the site-count reconcile` | same |
| 925 | `R-0.7 (<what>): nginx reloads during the site-count reconcile` | same |
| 934 | `R-0.7 (<what>): <user>'s pool is unchanged; systemd reload/restart events on its master` (*per user*) | same |
| 935 | `R-0.7 (<what>): <user>'s pool is unchanged; its master's MainPID` (*per user*) | same |
| 938–939 | `R-0.7 (<what>): <user>'s pool budget changed and its master was reloaded to apply it` (*per user*, instead of 909–910) | same |

The rest:

| Line | Bash check | Go subtest |
|---|---|---|
| 947 | `R-0.7: the static probe site did not provision` (alt) | same |
| 952 | `R-0.7: restarting the API runs a reconcile ($r)` | `R-0.7: restarting the API runs a reconcile` |
| 955 | `R-0.7: a PHP site is added` | same |
| 958 | `R-0.7 (create): adding a site runs the site-count reconcile, not a full one` | same |
| 959 | `r07_assert create` | see above |
| 962 | `R-0.7: the added PHP site is deleted` | same |
| 962 | `R-0.7: its teardown completes` | same |
| 964 | `R-0.7 (delete): deleting a site runs the site-count reconcile, not a full one` | same |
| 965 | `r07_assert delete` | see above |

## WooCommerce teardown (lines 969–973) → `woocommerce teardown/…`

| Line | Bash check | Go subtest |
|---|---|---|
| 971 | `woocommerce delete accepted` | same |
| 972 | `woocommerce teardown completes` | same |

## Canary (lines 46–65)

`cg_canary` is not a numbered check: it adds a failure whenever `/etc`,
`/root`, `/srv`, `/usr` or `/var/lib` stops being `root:root`. The Go canary
(`harness_test.go`) is checked before every command, API call, site request and
file write the suite makes, and reports `CANARY: <dir> stopped being root:root
after: <previous step>` as a failure of `TestE2E` itself.

## Count

On the happy path (no alt branch taken, ufw active):

| Section | Checks |
|---|---|
| site types | 26 |
| woocommerce | 6 |
| wordpress features | 107 |
| migration import | 11 |
| security | 24 |
| woocommerce security | 14 (10 since F4b removed Adminer's 4) |
| R-0.7 reconcile | 6 + 2 × (2 + 2 per active PHP site in the snapshot) |
| woocommerce teardown | 2 |
| **total** | **196 + 2 × (2 + 2 per PHP site)** |

Both suites print the same total: bash `RESULT: <pass> passed, <fail> failed`,
Go `RESULT: <pass> passed, <fail> failed` in the `TestE2E` log.

## Deliberate differences

Behaviour that is not one-for-one, and why. None of them changes what a check
asserts on the product.

1. **The canary reports, it does not repair.** The bash trap ran `chown 0:0`
   on a directory it found changed; the Go canary only reports (a repair hides
   the extent of the damage on a throwaway box that should be rebuilt anyway).
   It reports each change once, and again only if the directory went back to
   root's and changed again.
2. **Login fails fast.** Bash printed `[login] FAILED` and carried on, so every
   check failed; Go stops `TestE2E` with that message.
3. **`laravel serves` needs an HTTP answer.** Bash's `[ "$code" -lt 500 ]`
   passed curl's `000` (no connection at all); Go requires a status in
   1..499.
4. **A missing task id does not wait.** Bash polled `/api/tasks/0` for the
   whole timeout and then reported `timeout`; Go reports `timeout` at once.
   Same verdict, minutes sooner.
5. **Safety nets.** Everything the bash suite undid inline is also registered
   with `t.Cleanup` on `TestE2E`, so it is undone even when a check fails or
   the run panics: sites the suite created (deleted if still there), the
   operator account, the backup repository setting (cleared when it was
   empty, as in bash) and the test repository, the planting loop, the fake
   `/usr/local/sbin/mariadb-dump` (a pre-existing one is put back rather than
   deleted), the SQL import fixtures, temporary keys and fixtures. If the
   CSRF check ever lets the cross-site `POST /api/users` through, the account
   it created is deleted.
6. **Native where it is equivalent, the same tool where it matters.** API
   calls, site requests (`curl -sk --resolve`), the certificate (`openssl
   req`), the served-certificate probe (`openssl s_client`), the upstream for
   the proxy type (`python3 -m http.server`, now a Go listener on the same
   127.0.0.1:9099, scoped to the proxy check), JSON parsing (`python3`),
   `stat`, `sha256sum`, `readlink` and file writes are Go. Fixture files are
   written with `O_EXCL|O_NOFOLLOW` and re-owned through the descriptor
   instead of `printf > f; chown u:u f`, so a link the site planted is never
   followed. Everything that runs *as a site user* (`sudo -u`, `runuser`),
   and wp-cli, sftp, ssh-keygen, sshd, nginx, mariadb, restic, systemctl,
   journalctl, crontab, ufw, tar and `cp -a`, is the same command.
   The one site request that stays `curl` is line 234: `--compressed` with
   Chrome's `Accept-Encoding` must decode br and zstd, which `net/http` cannot.
7. **HTTP/2.** curl negotiates h2 over TLS by default; the Go client does too
   (`ForceAttemptHTTP2`). No compression is requested or decoded unless a
   check sets `Accept-Encoding`, as with curl.
8. **IP may be omitted.** Bash requires `IP`. `/root/cloudground-admin.env`
   holds only `PANEL_EMAIL` and `PANEL_PW`, so when `IP` is unset the Go suite
   uses the source address of the default route, and only if it is a public
   IPv4 address (not private, CGNAT or loopback); otherwise it stops and asks
   for `IP`.
9. **Parallel site types (opt-in).** `CG_E2E_PARALLEL=1` runs the five site
   types side by side. The default is the bash order, one at a time.

## Checks added after the port

Go only; the counts above are those of the port.

| Section | Check | Since |
| --- | --- | --- |
| wordpress features | `R-2.1: the backup password is not in state.db in plaintext` | R-2.1 |
| wordpress features | `R-2.1: the site's database password is not in state.db in plaintext` | R-2.1 |
| wordpress features | `R-2.1: settings show the backup password masked` | R-2.1 |
| wordpress features | `R-2.1: the secret key is the API user's alone` | R-2.1 |
| wordpress features | `R-2.4: the SFTP password is the one set` (the shadow hash verifies the password sent), `…SFTP on takes the user out of the deny group`, `…SFTP off puts the user in the deny group` (3 checks) | R-2.4 |
| R-2.4 services | the services page answers; nginx and mariadb each match `systemctl show ActiveState`, read as running, report cgroup memory, start time and version; redis matches systemd; site PHP row present; an nginx restart is accepted, replaces the main PID and nginx is already active when the call returns; an unmanaged service is refused (17 checks) | R-2.4 |
| security | `control: root reaches the agent with the token` | R-2.5 |
| security | `control: the API's user reaches the agent with the token` | R-2.5 |
| security | `a site user in the socket's group is cut off by the agent` (curl 52/55/56, not 7) | R-2.5 |
| security | `the agent logged the refused peer's uid` | R-2.5 |
| security | `a site user cannot open the agent socket` | R-2.5 |
| security | `control: the file manager lists the site` | R-2.2 |
| security | `control: listing a missing directory fails` | R-2.2 |
| security | `an agent error reaches the browser without paths or commands` | R-2.2 |
| wordpress features | `R-2.3: a site's current link is relative`, `…a release's wp-config.php link is relative`, `…a release's uploads link is relative` (3 checks, before the staging clone) | R-2.3 |
| wordpress features | `R-2.3 control: the site user hard-linked a file of its own into its release` (a file it owns in `/var/tmp`; `fs.protected_hardlinks` allows exactly that) | R-2.3 |
| wordpress features | `R-2.3: the staging clone's current link is relative`, `…staging's wp-config.php resolves to staging's own shared/`, `…staging's uploads resolve to staging's own shared/`, `…production's wp-config.php is unchanged by the clone` (sha256 before and after) (4 checks) | R-2.3 |
| wordpress features | `R-2.3: the clone copied a hard-linked file as an independent file owned by staging`, `…the clone neither linked nor re-owned the file outside the tree` (2 checks; `cp -a` passed these too, they guard the Go copier) | R-2.3 |
| wordpress features | `R-2.3: the file manager does not write through a hard link` (fails on a build before R-2.3, whose editor truncated the file in place) | R-2.3 |
| wordpress features | `R-3.10: the agent's journal answers`, `…the agent's journal is labelled`, `…agent journal lines start with an ISO time`, `…the journal tail honours lines`, `…the site PHP masters' journal answers`, `…the PHP journal is labelled`, `…the site access log answers`, `…the site access log is the site's file`, `…the access log tail shows the last request` (9 checks; coverage of the rewritten log reads) | R-3.10 |
| wordpress features | `R-3.10: a symlinked connector does not fail the drift check`, `…the symlinked connector is reported as drift`, `…a FIFO at the connector does not block the drift check`, `…the FIFO is reported as drift` (4 checks; the old agent failed the whole check on the symlink and blocked on the FIFO) | R-3.10 |
| wordpress features | `R-3.10: the firewall refuses to deny the panel port`, `…the firewall refuses to delete the panel port's rule`, `…a port is opened`, `…a port is opened to one source`, `…the source-limited rule is deleted`, `…deleting the source rule keeps the port's own rule`, `…deleting the source rule removes it` (7 checks; the old agent accepted deny/delete 8443 and removed the port's own rule) | R-3.10 |
| wordpress features | `R-3.9: the certificate status marks a brought certificate as custom` (the old status had no `custom` field, and the scheduler renewed brought certificates with certbot) | R-3.9 |
| R-3.9 certificates and panel | `R-3.9: the panel's certificate status is the panel.pem nginx serves` (agent `CertStatus` over the socket vs panel.pem's NotAfter; the old agent read certbot's lineage, absent on a box without a panel domain). Fixture failures: `R-3.9: control: panel.pem is readable`, `…panel.pem parses` | R-3.9 |
| wordpress features | `R-3.1: adding a key while SFTP is off leaves no empty password` (the old agent ran `passwd -d` on a locked account when a key was added) | R-3.1 |
| wordpress features | `R-3.1: the panel shows a key's fingerprint as ssh-keygen prints it`, `…a key whose fingerprint has a / is removed by it`, `…the removed key is gone from the list`; fixture `control: a key whose fingerprint has a / is added` (the old agent printed URL-safe base64) | R-3.1 |
| security | `R-3.1: listing a missing directory answers 404` (the old agent answered 502) | R-3.1 |
| security | `R-3.1: making a folder that exists answers 409`; fixture `control: the file manager makes a folder` (the old agent answered 502) | R-3.1 |
| wordpress features | `R-3.2: deploy 1 of two started together succeeds`, `…deploy 2 …`, `…two deploys started together get distinct releases`, `…each deploy built its own release` (the old API named releases to the second: the second deploy of a second collided and failed) | R-3.2 |
| wordpress features | `R-3.2: a rollback never lands on a release built after the live one` (the old agent rolled back to the newest release that was not live: one built and never promoted) | R-3.2 |
| wordpress features | `R-3.2: no build directory is left behind`, `…a new release belongs to the site user`, `…the connector in a new release belongs to the site user`, `…a deploy does not change the live release`, `…the new release is promoted`, `…current names the promoted release`, `…the promoted release serves`, `…the deploy precompressed a stylesheet`, `…gzip_static serves the precompressed stylesheet` (body byte for byte the `.gz`), `…at most five releases older than the live one are kept` (10 checks; coverage of the rewritten deploy) | R-3.2 |
| woocommerce security | `R-3.6: the database page lists the site's tables` (coverage of the new `DBTables` path) | R-3.6 |
| woocommerce security | `R-3.6: a console statement that ran out of time is not left running on the server` (`SELECT SLEEP(90)`, then no `cgq_` connection in `PROCESSLIST`; the old agent dropped the account and left the statement running for its whole length) | R-3.6 |
| woocommerce security | `R-3.6: a failed import leaves no table behind (exact rollback)` (after `cg-evil.sql`; the old rollback imported the safety dump on top and kept `cg_e2e_import`) | R-3.6 |
| wordpress features | `R-3.6: the restored database is the snapshot's, without tables made since` (`cg_e2e_after`, created after the backup; the old restore imported on top and kept it) | R-3.6 |
| wordpress features | `R-3.4: after a database-only restore the site serves the restored option, not its object cache's` (option read over HTTP through the APCu object cache; the old agent imported the dump and left the FPM pool's APCu alone, so the site kept serving the value from after the backup). Also `R-3.4: object cache on before a database restore`, `…the backup for the database restore runs`, `…database-only restore succeeds`, `R-3.4 control: the site reads the changed option through its object cache`; fixture failure `R-3.4: database restore fixture` | R-3.4 |
| wordpress features | `R-3.3: a store installed by hand cannot push its orders table` (`wp_wc_orders` planted in both databases of the WordPress feature site, push answers 400; the old policy went by site type and accepted it) | R-3.3 |
| wordpress features | `R-3.3: staging's options push succeeds`, `…production's PHP serves the pushed option at once (object cache flushed)` (blogname set on staging, read through production's FPM before and after; the old push left alloptions in APCu), `…production stays indexable after an options push (blog_public)` (the old push carried staging's 0), `…the pushed home address is production's` (coverage), `…a JSON-escaped staging address is rewritten to production's` (the old rewrite had one plain pass); fixtures `R-3.3 control: production's object cache is on`, `…production's PHP serves its own site name`, `…production is indexable before the push`, `…staging is not indexable` | R-3.3 |
| wordpress features | `R-3.3: the staging password file goes with the staging site` (after the feature site's delete; the old teardown left `/etc/nginx/cloudground-auth/staging.<domain>.htpasswd`); fixture `R-3.3 control: the staging site has its password file` | R-3.3 |
| woocommerce | `F7: a price change purges the product page`, `F7: a price change purges the product's category archive` (price set through `WC_Product::save()` in PHP-FPM, as the REST API and bulk edit do; WooCommerce then writes post meta only and fires no `transition_post_status`, so the old connector purged nothing); fixtures `F7 control: a product in a category created through PHP-FPM`, `…the product page is cached`, `…the product category archive is cached`, `…the price changed` | F7 |
| wordpress features | `F7: a slug change purges the old URL` (the old connector purged only the new permalink), `F7: a custom taxonomy term archive is purged` (`cg_genre`, registered by a test mu-plugin; the old connector purged categories and tags only), `F7: publishing a post purges the sitemap`, `…the sitemap lists the new post` (`/wp-sitemap-posts-post-1.xml`; never purged before); fixtures `F7 control: posts and a custom taxonomy term created through PHP-FPM`, `…the post's URL is cached`, `…the custom taxonomy term archive is cached`, `…the posts sitemap is cached`. Each purge check polls `X-CloudGround-Cache` for up to 10 s, because the connector purges after the response is sent | F7 |
| R-3.9 certificates and panel | `R-3.9: the agent answers the request in flight when it is stopped` (headers sent, `systemctl restart --no-block cloudground-agent`, body sent a second later; the old agent died on SIGTERM and the reader saw EOF), `R-3.9: the agent is back after the restart`. Fixture failures: `R-3.9: control: the agent token is readable`, `…the agent socket answers` (2 checks) | R-3.9 |

### Files (editor and upload)

| Check | Go subtest | Why |
| --- | --- | --- |
| control: the admin signs in through nginx on :8443 | security / filesUploadThroughNginx | setup control |
| a 2 MB upload through nginx reaches the panel and is stored | security / filesUploadThroughNginx | nginx's 1 MB default answered 413 to every larger upload (measured 2026-10-09) |

### Quarry (F4b) → `F4b Quarry/…`

Run after `woocommerce security`, on the WooCommerce site's database. They replace the Adminer checks removed above.

| Check | Why |
| --- | --- |
| `F4b control: the read-only account signs in`, `F4b: a read-only account lists the tables`, `…reads rows` | setup and control: the read-only role reaches Quarry's reads |
| `F4b: a read-only account cannot edit rows`, `…cannot alter a table`, `…cannot allow writes in SQL` (403 each), `F4b: the server refuses a read-only account's INSERT` (`ERROR 1142` from the read-only scoped account), `F4b: nothing a read-only account sent was written` | the plan's "un `readonly` non può scrivere": refused by the panel and by the server |
| `F4b: the SQL editor cannot read mysql.user`, `…cannot read cg_e2e_tenant.secret`, `F4b: Quarry cannot browse cg_e2e_tenant`.`secret`, `…mysql`.`user`, `F4b: an injected sort column is refused and the table stays` | nothing outside the site's database; identifiers are looked up, never interpolated |
| `F4b: the SQL editor refuses system`, `…refuses \!` | the mariadb CLI's client commands do not exist on the protocol |
| `F4b: a dry run writes nothing`, `F4b: the preview is the SQL that ran`, `F4b: the insert landed`, `…the update landed`, `F4b: a failing batch is refused and rolled back`, `F4b: the delete landed`, `F4b: a guided ALTER runs` | edits by primary key in one transaction, preview first |
| `F4b: the audit records quarry.insert`, `….update`, `….delete`, `….alter`, `F4b: the audit never holds row contents` | every write in the audit with table and key, never the row's values |
| `F4b control: room for the large transfer`, `F4b control: the large table is there`, `F4b: the large export succeeds`, `F4b: the export is listed with its size`, `F4b: the download is the export, byte for byte`, `F4b: the large upload is stored`, `F4b: the upload is the file, byte for byte`, `F4b: the large import succeeds`, `F4b: the imported table is the exported one`, `F4b: the agent's memory does not grow with a 1 GB export and import` (VmRSS of the agent's MainPID sampled every 100 ms from `/proc`; peak minus start below 192 MiB while 1 GiB is exported, downloaded, uploaded in 8 MiB chunks and imported) | streamed export and import; `CG_E2E_DBSTUDIO_MB` sets the size (default 1024) |

### Events (F8) → `F8 events/…`

Go only, run after `R-3.7 site lifecycle`. Two static sites: A, assigned to an operator created for the section, and B, created by the admin while both streams are open.

| Check | Why |
| --- | --- |
| `F8 control: the operator's site provisions`, `F8 control: the operator signs in`, `F8: the event stream answers the operator`, `…answers the admin`, `F8: the event stream is text/event-stream` | setup and control: both streams open over the API socket |
| `F8: a site's change to active reaches the admin stream within 2 s` (B's `site.changed` with status active, timed against a 200 ms poll of `GET /api/sites/{id}`) | status changes are pushed, not found by the next poll |
| `F8: a task's progress arrives on the stream` (a cache warm of A: `task.progress` with its id), `F8: the operator's stream carries its own site's task` (`task.finished`) | live task progress; the control that the operator's stream delivers at all |
| `F8 control: the admin deletes the other site`, `F8 control: the admin stream carries the other site's events` | B's events exist and are delivered to someone |
| `F8: an operator's stream carries no event of a site it is not assigned` (nothing with B's id, nothing server-wide), `F8: the operator's stream is not ended by the other site's events` | per-user filtering on the server |
| `F8 control: the admin signs in through nginx on :8443`, `F8: the event stream answers through nginx`, `F8: the stream through nginx is open after 65 s, with heartbeats` (at least two `ping`s), `F8: an event reaches the stream through nginx at once` (`task.created` within 10 s), `F8: a cross-site request for the stream is refused` (403) | nginx neither buffers nor cuts the stream. The old stream was cut by the API's 30 s `ReadTimeout` and fails the 65 s check |
| `F8 control: the operator's site is deleted` | cleanup |
