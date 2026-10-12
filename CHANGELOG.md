# Changelog

What changes in each CloudGround release, newest first. Every release has a heading of the form
`## vX.Y.Z`: the panel's update notice links straight to it, so the heading carries the tag and
nothing else.

Entries say what a person running the panel will notice. The reasons and the measurements are in
the commits.

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
