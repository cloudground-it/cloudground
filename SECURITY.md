# Security policy

CloudGround's agent runs as root on the server it manages. A flaw in it can mean someone else
controls that server, so this page covers both how to report one and what CloudGround does and
does not promise.

## Where it stands

- **No external security audit so far.** The code had a full internal review in October 2026; its
  findings, and what changed after each, are in
  [docs/security-model.md](./docs/security-model.md#findings-of-the-internal-review).
- Until an independent review has happened, run CloudGround on servers you can rebuild, keep the
  backups on storage outside the server, and weigh that before trusting it with something critical.

## Checking what you install

The binaries run as root, so knowing where they came from matters more than usual.

- Every release carries `SHA256SUMS`, the SHA-256 of each file, and `SHA256SUMS.sig`, an ed25519
  signature of that list. The public keys a release may be signed with are compiled into the
  binaries (`internal/version`): an install or an update refuses a release whose list is not signed
  by one of them, or whose files do not match it.
- A release's `install.sh` names the SHA-256 of the `cgctl` it downloads and runs nothing else.
  Fetching the script itself trusts the place it came from; to trust a value of your own instead, set
  `CLOUDGROUND_CGCTL_SHA256` or `CLOUDGROUND_RELEASE_PUBKEY` (internal/install/SPEC.md §5).
- Releases are built twice from a clean cache and must match byte for byte, and GitHub attests the
  build: `gh attestation verify <file> --repo cloudground-it/cloudground`.

## Reporting a vulnerability

**Never in a public issue.** Open a private advisory on GitHub instead: **Security → Report a
vulnerability** at <https://github.com/cloudground-it/cloudground>. Only the maintainers see it.

Useful to include:

- what an attacker gains, and what they need first (nothing? a site's SFTP login? a read-only
  account in the panel?);
- the steps, tried on a throwaway server if you can;
- `cgctl --version` and the Ubuntu release.

You can expect a first answer within **72 hours** and an assessment within **7 days**. Please give
a fix time before going public: **90 days** unless we agree otherwise, sooner if the fix ships
earlier or the flaw is already being used. The advisory and the changelog credit you, unless you
prefer not to be named.

## In scope

Anything that gets past a boundary the panel claims to enforce:

- from a site's user, SFTP account or shell to another site or to root;
- one site reading or changing another's files, database, cache or logs;
- sign-in, sessions, two-factor codes, API tokens, the setup token, WordPress sign-in links;
- an `operator` or `readonly` account, or an API token, doing what its role does not allow;
- an unprivileged local process reaching the agent's socket or using it;
- command or SQL injection through a domain, a database name, a path or a site setting;
- the file manager, the SQL console, Quarry, cron and the site shell, which are powerful on
  purpose and confined on purpose;
- a release or an update that installs code nobody signed.

## Out of scope

- An attacker who already has root on the server.
- A malicious panel administrator: an `admin` can do what root can, by design.
- Vulnerable code a site runs itself, such as an outdated plugin. The sandbox limits how far it
  reaches; getting out of the sandbox, though, is in scope.
- Hardening that would be nice but has no attack behind it: open an issue for that.
- Plain traffic floods.

## The model in five lines

The full map is [docs/security-model.md](./docs/security-model.md).

- **One Unix user, one PHP-FPM master and one systemd sandbox per site.** The sandbox's mount
  namespace hides every other site, the panel's files and the system's configuration.
- **Two processes.** `panel-api` has no privileges; root work goes to `panel-agent` over a Unix
  socket that checks the caller's uid and a token, as a fixed set of typed methods, never a shell
  string.
- **Sessions are random tokens kept as hashes** and can be revoked; passwords use argon2id, and an
  unknown account takes as long to refuse as a known one.
- **Secrets are sealed** with AES-256-GCM under a key file of their own.
- **Rendered configuration is hashed**, and a change made outside the panel is reported as drift.

A case where one of these lines does not hold in practice is exactly the report we want.

## Supported versions

Security fixes go into a new release of the latest 1.x only. Updating within 1.x keeps the API,
`cgctl`, the MCP tools and the backups compatible (`internal/install/SPEC.md` §11), so a fix is
one update away: `cgctl update`, or the button in the panel.
