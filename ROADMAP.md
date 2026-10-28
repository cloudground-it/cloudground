# Roadmap

Where CloudGround is going, what it will not do, and where a hand helps most. It is short on
purpose, and it has no dates: one person maintains the project, and a date that is guessed is a
promise that is not kept. Items are listed roughly in order of priority.

Concrete work is tracked as GitHub issues, grouped in [milestones](https://github.com/cloudground-it/cloudground/milestones); this page holds only the direction.

## On the way to 1.0

What stands between today and 1.0 is trust in the code and in the backups.

- **A second security review.** The internal review is documented in
  [SECURITY.md](./SECURITY.md); before 1.0 the parts that hold root get a second, full pass. It is
  not an external audit, and an outside one is still welcome.
- **Restores across upgrades and servers.** Test restores already run on a schedule; what is
  missing is proof that a backup taken on one release restores on the next, a way to move the
  backup repository, and a way to restore sites on a new server from another server's backups.

1.0 does not mean years in production. The end-to-end suite runs green on both supported Ubuntu
releases; a long record on many real servers comes after it, not before.

## Wanted

- **ARM64.** Only amd64 is built today. The Go code is portable; the tuning and the e2e suite
  have to be shown to hold on ARM servers.
- **A demo to click through.** A public instance of the real panel is out of the question (its file
  manager, SQL console and cron run code), but the interface running against sample data is not.
- **A stopped-on-purpose state for services**, so a service an administrator turned off no longer
  looks like one that crashed.
- **A worked example for object storage as a backup target**, with the S3-compatible services that
  do not charge for downloads, where a restore is cheapest.
- **A flatter release layout** for uncached PHP renders, measured at about 8 % faster, once it can
  be done without weakening releases, rollback or isolation.

## Not planned

These are decisions about what CloudGround is.

- **Mail, DNS and domain registration.** Each one is a product of its own.
- **Billing and resellers.** CloudGround runs a server, not a hosting business.
- **Several servers under one panel.** One panel manages one machine; a cluster would change every
  assumption of the security model.
- **Windows and other Linux distributions.** Two Ubuntu LTS releases are a promise that can be
  tested; a long list is not.

## Where help counts most

1. **Install it on a throwaway server and report what broke**, especially on hardware or workloads
   unlike a small cloud VM. A bug with steps to reproduce is worth more than a feature.
2. **Try to cross the privilege boundary.** `panel-api` has no privileges, `panel-agent` is root,
   and only typed calls pass between them. A way across is the most valuable report there is: send
   it privately, as [SECURITY.md](./SECURITY.md) says.
3. **Fix the documentation where it confused you.** Whoever was just confused writes the best
   version of that page.
4. **Pick an issue labelled
   [good first issue](https://github.com/cloudground-it/cloudground/issues?q=is%3Aissue+is%3Aopen+label%3A%22good+first+issue%22).**

Before starting something structural, open an issue: a short exchange can save a lot of work.
