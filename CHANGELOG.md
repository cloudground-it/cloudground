# Changelog

What changes in each CloudGround release, newest first. Every release has a heading of the form
`## vX.Y.Z`: the panel's update notice links straight to it, so the heading carries the tag and
nothing else.

Entries say what a person running the panel will notice. The reasons and the measurements are in
the commits.

## v1.0.1

- **an update from the panel brings the release's kernel settings**: a server updated to v1.0.0
  from the panel, or with `cgctl update`, kept the kernel's old range for its own connections
  (1024–65535), so a managed app could be given no TCP port and `cgctl doctor` reported
  `60-cloudground.conf`. The agent now writes and loads the release's settings as it starts after
  an update. A server installed with v1.0.0, or where `cgctl install` ran since, is not concerned.

**Upgrading from v1.0.0** is one update, from the panel or with `cgctl update`.

## v1.0.0

The first stable release. 1.0 is a promise about compatibility: the API, `cgctl` and the MCP tools
stay compatible through 1.x, an update within 1.x never takes away what a 1.x release offers, and
every release restores the backups of every release before it. What changes for a person running
the panel:

- **move the backups**: Settings → Backups → Move takes every backup to a new repository, either
  by copying the snapshots across or after you copied the files yourself. The panel checks that
  every backup is there before it points anything at the new place, then runs a restore test of
  each site's latest backup from it. An SFTP destination shows the panel's key to add there;
- **restore on a new server**: a fresh CloudGround connected to an existing repository lists the
  sites it holds from other servers, and restores one in full: the site is created with its
  type, PHP version, aliases and cache settings, and its database accounts come back with their
  own passwords, so the application connects as before. Each backup now carries a short
  description of its site for this; a backup from before 1.0 asks for the type and PHP version;
- **restores across updates, proved**: the end-to-end suite installs real older releases, backs a
  site up on each, updates to the new release from the panel and restores it in every mode;
- **a stable API**: docs/api.md says what stays the same through 1.x, and how anything that will
  go is announced first;
- the kernel hands out ports from 32768 to 65535 for the server's own connections, no longer from
  1024: the ports below stay with services, and a managed app's new TCP port must be one of them.
  An app keeps the port it has.

**Security.** The second internal review found these, all fixed in this release:

- GHSA-2x3m-w3c8-7xj8 (high): an import into a database on an external server ran the dump as the
  server's administrator; it now runs as the site's own database account;
- GHSA-hv85-58rm-pvxx (medium): through the server's relay, a site could send mail as another site
  or as the panel; a sender not of the site is replaced by the site's own;
- GHSA-fc8m-vwp2-52h4 (medium): a site's code could get a visitor blocked from SSH and the panel;
  WordPress failures now block the sites' ports only;
- GHSA-w642-56fp-4cx4 (medium): a site's code could take a managed app's port, or the connector's,
  while it restarted; on Ubuntu 26.04 sites and functions bind no port. Ubuntu 24.04 cannot enforce
  this: keep apps on their unix socket there;
- GO-2026-6809 (medium, in Go's `crypto/x509`): built with Go 1.26.10;
- five low-severity fixes: passkey challenges, an open redirect while the MCP server is on, a
  credential moved by an API token, Cloudflare purges, the backup archive download. The hardening
  that came with them is in docs/security-model.md.

**Upgrading from v0.9.x** is one update, from the panel or with `cgctl update`. On a server without
`libbpf1` the update installs it and re-executes systemd, so the port rule applies at once; running
services are not restarted. Backups taken before the update restore as before.

## v0.9.0

Functions on their own. What changes for a person running the panel:

- **functions are no longer part of a site**: they have their own entry in the sidebar, after
  Sites, and their own page with Code, Addresses, Settings, Runs and Releases. Each one runs as a
  system user of its own, isolated from every site and from the other functions;
- **addresses**: set a functions domain in Settings → Functions (say `fn.example.com`) and every
  function answers at `https://fn.example.com/<name>`, with one Let's Encrypt certificate. A
  function can also have domains of its own (say `api.example.com`) that answer it at `/`, with
  their own certificate. A function with no domain cannot be reached from the Internet, but still
  runs from Test and on its schedule;
- **code in several files, with dependencies**: a `package.json` or a `requirements.txt` is
  installed for each release, so a rollback brings back the same dependencies, and a failed
  install leaves the live release as it was;
- **a bearer token**, checked by nginx before the function starts (401 without it), shown once
  and renewable; and allowed CORS origins per function;
- shorter texts across the panel, in all five languages: hints that only repeated their label are
  gone, and the ones left say what is not obvious.

**Upgrading from v0.8.x: the functions' URLs change.** At its first start the panel moves every
function out of its site, with its code, releases, variables and secrets. The old address
`https://<site>/.functions/<name>` stops answering: set a functions domain (or a domain of the
function's own) and point whatever calls the function at the new URL. A function whose name is
already taken by another site's becomes `<name>-<site id>`. If one cannot be moved, it stays in its
site, an alert says why, and the panel tries again at its next start.

## v0.8.2

What changes for a person running the panel:

- the Traffic tab's chart is readable: page views and visitors are two lines with an axis, dates
  and a tooltip on hover (or with the arrow keys), and the last 48 hours are thin bars.

## v0.8.1

What changes for a person running the panel:

- on Node.js, Python, PHP, Laravel, static and proxy sites the Functions tab is in the row of tabs,
  right after Releases, instead of under "More"; on Node.js and Python sites App comes second;
- the demo shows the v0.5.0–v0.8.0 features with sample data (a Node.js and a Python site,
  functions, traffic, redirects, deploy from Git, external database servers, an AI assistant),
  and its bottom bar no longer covers the end of the page.

## v0.8.0

Beyond WordPress. What changes for a person running the panel:

- **Node.js and Python sites**: two new site types run your app as a service of the site's own
  user, inside its sandbox and resource limits, behind nginx on a private socket. Node.js 20, 22
  or 24 is downloaded from nodejs.org and checked against its published checksums; Python is the
  server's, with a virtual environment per site. Each release installs its own dependencies
  (`npm ci`, or pip in the venv), so a rollback brings back exactly what that release ran; a
  failed install leaves the live release live. Variables and encrypted secrets, the app's log,
  automatic restarts and its health on the new App tab; deploy from Git works the same way;
- **functions**: small Node.js or Python handlers at `https://<domain>/.functions/<name>`, or on
  a schedule. Nothing runs between requests: each call starts its own process with its memory
  and time limits, and a flood is held back (per client and per site) instead of piling up
  processes. Invocations, status, duration and output in the panel; code changes are releases
  you can roll back;
- fixed: a managed app restarted several times in a row (a deploy, a rollback, saving its
  variables) could be left stopped by systemd's start limit.

## v0.7.0

Traffic and visibility. What changes for a person running the panel:

- **redirects and the canonical name** per site: choose whether the site answers at `www.` or at
  the bare domain (the other name redirects to it in one hop, the certificate covers both), and
  add up to 200 exact or prefix 301/302 redirects. Redirects are answered before the page cache,
  so a cached page never hides one;
- **traffic statistics** from each site's own access logs, on the server, with no third party and
  no cookie: requests, page views, visitors, bots, status codes, cache hits, top pages and
  not-found paths, kept as daily totals for 90 days. Visitors are counted without storing any
  address. On the new Traffic tab;
- **Cloudflare**: for a site behind Cloudflare, the panel sees the real visitor address (trusted
  only from Cloudflare's published networks, refreshed daily), the block list still applies, and
  a purge from the panel also purges Cloudflare's cache with a purge-only API token;
- **download a site's full logs**, access and error, rotated ones included, from the panel or with
  `cgctl logs`; every download is in the audit log;
- **a clickable demo** of the panel with sample data, published with each stable release at
  get.cloudground.it/demo/: nothing you do there leaves your browser;
- missing static files (a 404 on an image or a stylesheet) are now written to the site's access
  log; served static files still are not.

## v0.6.0

Data. What changes for a person running the panel:

- **external database servers**: register a MariaDB/MySQL or PostgreSQL server in Settings
  (admin only; the password is encrypted and never shown again) and create a site's databases
  and users there. Backups with their restore test, restores, the console, exports and Quarry
  follow the database to its server (Quarry and the console are MariaDB/MySQL only for now).
  PostgreSQL needs TLS; a site's main database stays on this server;
- **restore one file or folder** from a backup, still behind a tested safety snapshot;
- **download a backup** as an archive, streamed, for administrators and for operators on their own
  sites, every download in the audit log; the archive is refused when the disk would not hold it,
  and deleted once sent or after a day;
- **new backup destinations**: Backblaze B2 and SFTP, next to S3-compatible storage and a local
  folder; for SFTP the panel makes the key;
- **move a site from another host**: give the old server's SSH/SFTP access, see a dry run (files,
  size, WordPress or not, the host key, whether it fits), then CloudGround copies the files and the
  database and imports them behind a safety snapshot. The password or key is used once and kept
  nowhere. Administrators only;
- fixed: the backup of a site whose PostgreSQL database had no tables failed its restore test.

## v0.5.0

Remote access and AI. What changes for a person running the panel:

- **passkeys** for panel accounts: a second factor after the password, or the whole sign-in with
  fingerprint, face or PIN; several per account, named and removable. They work when the panel is
  opened at its domain over HTTPS, not at its IP address;
- **recovery codes**: ten single-use codes for when the phone is lost, shown once and made anew on
  demand; a lost factor can now be replaced without root;
- **required two-step verification**, set in Settings → Access: off, for administrators or for
  every account; an account that must set it up is sent to its Security page after the password;
- changing a factor asks for the password and a current factor; an administrator can reset
  another account's factors;
- **deploy from Git**: a per-site webhook for GitHub, GitLab, Gitea and Forgejo builds a release
  from a branch and makes it live, with the usual rollback. The repository is fetched and the
  optional build command runs as the site user; private repositories use a deploy key the panel
  makes. On the Releases tab, or `cgctl git-deploy`;
- **a remote MCP server**, off by default: turned on in Settings → Access, it lets AI assistants
  (Claude, ChatGPT and other MCP clients) read and manage the server with OAuth. Each connection is
  approved with the password and a second factor, can only write where it was allowed to, and
  destructive actions (restore, staging push, deleting a site) wait for a confirmation in the
  panel. Turning it off disconnects every assistant. It needs the panel on its own domain with a
  trusted certificate;
- fixed: after a release was made live or rolled back, a static file could still come from the
  previous release for up to a minute.

## v0.4.0

WordPress care. What changes for a person running the panel:

- WordPress updates, from the buttons or on a per-site schedule, run behind a restore-tested
  snapshot: the site is measured before and after, and restored on its own when the update
  breaks it; without a backup repository an update is refused;
- weekly database cleanup for WordPress sites, off by default: expired transients go and each
  post keeps its last N revisions, after a dry run and, the first time, a tested snapshot;
  `cgctl cleanup`;
- email from sites: PHP's `mail()` and `wp_mail` now go out through an SMTP relay, server-wide or
  per site, with a test button and an SPF/DKIM/DMARC checklist; staging copies still send nothing;
- email is a second channel for alerts, next to the webhook;
- a maintenance page per site, from the Protection page or `cgctl maintenance`: visitors get a
  503 with `Retry-After` and your title and message, while listed addresses and a preview link
  still see the site;
- development mode: a site's page cache is bypassed for 15, 30 or 60 minutes, then turns back
  on by itself;
- Ubuntu updates from the panel: the pending packages, security ones first, installed all at once
  or only the security ones, and a notice when the server needs a reboot;
- What's new: after an update, the panel shows what changed, from this changelog.

## v0.3.2

A new logo, and the panel in the colours of cloudground.it. What changes for a person running
the panel:

- the new CloudGround logo, in the panel, its browser and home-screen icons and the README;
- the colours match cloudground.it and its documentation: the logo's blue for the main action
  of a screen, links and focus, cooler greys; every text keeps WCAG AA contrast in both themes;
- every row of tabs (a site's tools, logs, settings, the new-site wizard) looks and behaves
  the same, and the top bar has the height and spacing of the site's.

## v0.3.1

The panel in a new look, and a wizard to create a site. What changes for a person running the panel:

- the panel has a new visual design;
- a new site is created with a wizard on its own page, `/sites/new`, in 5 steps, with the logos
  of the services it offers;
- the Domain step accepts aliases and checks that the domain's DNS points at the server
  (`GET /api/sites/dns-check`);
- fixed: the alert that offers to restore the wizard's saved draft had a close button without
  a proper label.

## v0.3.0

Site security. What changes for a person running the panel:

- WordPress sites are checked against a public vulnerability database (WPVulnerability): the
  panel lists the core, plugin and theme versions with known issues and the version that fixes
  them;
- malware scan of a site's files, opt-in, with ClamAV: scheduled or on demand, quarantine of a
  flagged file, alerts;
- two-factor sign-in for WordPress accounts, per site, from the panel;
- one-click WordPress hardening, each item on its own and reversible;
- a block list: addresses that keep failing to sign in (panel, SSH, wp-login, xmlrpc) are blocked
  at the firewall for a while, with an allowlist; see them on the new Blocking page or with
  `cgctl blocklist`;
- per-site blocking of visitors by address or network, user agent or country (country data:
  DB-IP Lite, CC BY 4.0);
- the agent's long jobs (backups, restores, imports, scans) run in units of their own, so the
  agent keeps a memory limit; Ubuntu's needrestart no longer restarts them halfway;
- a self-update installs the system packages a new release needs;
- fixed: in v0.2.0 every alert webhook was refused, public ones included, because the API could
  not list the server's addresses;
- when the panel's service fails to start, the installer names it and shows its log;
- interface: the sidebar shows the version once, empty states sit higher, tab bars are no longer
  clipped.

## v0.2.0

Security, data safety and per-site resources. What changes for a person running the panel:

- per-site resources: CPU, memory, tasks and disk use with a 24-hour and 30-day history in a new
  Resources tab; memory, CPU and task limits that an admin sets and that apply without a restart;
  alerts when a site stays over its threshold or its disk use passes a set size;
- uptime monitoring per site, off by default: a check every minute from the server, an alert
  after two failures in a row, cleared when the site answers again;
- protect any path of a site with a username and password, from the panel or `cgctl protect`;
- choose the hour of daily and weekly backups; list and install common PHP extensions per PHP
  version;
- restore only some of a site's databases, from the restore dialog or with
  `cgctl backup restore --database`;
- turn the firewall on from the panel: it shows the rules first (the real SSH port, the web ports
  and the panel's) and allows them before enabling;
- SFTP accounts can be set to SSH keys only, the default when a key is given;
- a migration import takes a safety snapshot first, as restores and Safe Push already did;
- alerts, webhooks and the task list name operations in words, in the reader's language; webhook
  bodies keep the raw fields in a `cloudground` object;
- hardening: tighter bounds on what the agent captures from the commands it runs; each site gets
  its own `/dev/shm` and IPC namespace; backup, dump and editor leftovers in a site's public folder
  answer 404; read-only accounts see cron commands shortened; a password inside a backup
  repository URL is stored sealed, and `cgctl uninstall` no longer prints it;
- long sign-in waits and long requests (database queries, repository tests, uploads) now finish
  instead of failing at 30 seconds;
- faster uncached PHP: the tracing JIT is really on;
- one copy of @vueuse in the interface.

## v0.1.0

The first public release. What CloudGround does:

- one command to install the panel on Ubuntu 24.04 or 26.04 LTS, with signed releases; updates
  from the panel or with `cgctl update`, which keep a snapshot of the panel's state and roll back
  by themselves when the new release does not come up; `cgctl uninstall`, which keeps the sites,
  their databases and the backup credentials unless told otherwise;
- WordPress, WooCommerce, PHP, Laravel, static and reverse-proxy sites, each with its own Unix
  user, PHP-FPM master and systemd sandbox;
- nginx page cache with precise purges from the WordPress connector, compressed once and served
  stale while it refreshes; PHP, OPcache and MariaDB sized from the machine;
- per-site databases and database users, created when you want them, with all or read-only
  privileges; the database name, user and password chosen when the site is created, and a
  summary to keep;
- CloudGround Quarry, the database tool at `/quarry`: open it from the panel or sign in with a
  database user's own password; browse, edit, search, SQL with Explain and Simulate (run a write
  in a transaction that is always rolled back), import and export;
- staging copies and Safe Push, which promotes a release only when its measurements hold up;
- encrypted off-site backups to S3-compatible storage or this server, with scheduled test
  restores and a guided setup;
- sign-in protection that slows the network guessing a password without locking the owner out,
  2FA, roles, API tokens and an audit log;
- the web interface in English, Italian, French, Spanish and German, the HTTP API, and `cgctl`.
