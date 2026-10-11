# Changelog

What changes in each CloudGround release, newest first. Every release has a heading of the form
`## vX.Y.Z`: the panel's update notice links straight to it, so the heading carries the tag and
nothing else.

Entries say what a person running the panel will notice. The reasons and the measurements are in
the commits.

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
