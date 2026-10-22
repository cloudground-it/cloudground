package api

import "net/http"

//go:generate go run ./gendoc -o ../../docs/api.md

// The docs/api.md sections, in the order apidoc.md.tmpl places them.
const (
	secSession  = "Setup and session"
	secAccount  = "Account"
	secUsers    = "Users"
	secTokens   = "API tokens"
	secSites    = "Sites"
	secReleases = "Releases, staging and Safe Push"
	secBackups  = "Backups"
	secFiles    = "Files"
	secDatabase = "Database"
	secDBStudio = "Quarry"
	secWP       = "WordPress"
	secSFTP     = "SFTP and SSH keys"
	secCron     = "Cron"
	secServer   = "Server"
	secTasks    = "Tasks and events"
	secConn     = "Connector"
)

// Body limits that differ from defaultMaxBody (SPEC.md §1.6).
const (
	smallBody      = 8 << 10   // credentials and profile fields
	blockingBody   = 64 << 10  // a site's blocking rules: 500 networks, 50 user agents
	certBody       = 256 << 10 // a PEM chain and key
	dbEditBody     = 16 << 20  // a batch of row changes
	fileUploadBody = maxPanelTransfer
	dbChunkBody    = uploadChunk
)

// routeTable is every route the panel serves (SPEC.md §1). Routes and
// ConnectorRoutes register nothing else, and docs/api.md is generated from
// it (go generate).
func (s *Server) routeTable() []route {
	const (
		get, post, put, patch, del = http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete
	)
	return []route{
		// Setup and session.
		{Method: get, Path: "/api/bootstrap", Policy: policyOpen, Handle: plain(s.handleBootstrap),
			Section: secSession, Doc: "has the first admin been created yet"},
		{Method: post, Path: "/api/setup", Policy: policyOpen, MaxBody: smallBody, Handle: plain(s.handleSetup),
			Section: secSession, Doc: "create the first admin with the one-time setup token"},
		{Method: post, Path: "/api/login", Policy: policyOpen, MaxBody: smallBody, Handle: plain(s.handleLogin),
			Section: secSession, Doc: "start a session"},
		{Method: post, Path: "/api/logout", Policy: policySignedIn, SessionOnly: true, Enrol: true, Handle: plain(s.handleLogout),
			Section: secSession, Doc: "sign out and delete the session"},
		// The UI's first request: it must not answer 401, which would be a
		// red error in the console on every visit to the sign-in page.
		{Method: get, Path: "/api/session", Policy: policyOpen, Handle: plain(s.handleSession),
			Section: secSession, Doc: "the signed-in account, or `{\"user\": null}`"},
		{Method: get, Path: "/healthz", Policy: policyOpen, Loopback: true, Handle: plain(s.handleHealthz),
			Section: secSession, Doc: "liveness: `200` when the agent answers, `503` when it does not"},
		{Path: "/", Policy: policyOpen, Handle: plain(s.handleUI),
			Section: secSession, Doc: "any other path: the web interface"},

		// Account: a user may always manage their own.
		{Method: post, Path: "/api/account/password", Policy: policySignedIn, SessionOnly: true, MaxBody: smallBody, Handle: s.handleChangePassword,
			Section: secAccount, Doc: "change your own password"},
		{Method: post, Path: "/api/account/2fa/begin", Policy: policySignedIn, SessionOnly: true, Enrol: true, MaxBody: smallBody, Handle: s.handleTOTPBegin,
			Section: secAccount, Doc: "start TOTP enrolment (requires your password); returns the secret"},
		{Method: post, Path: "/api/account/2fa/confirm", Policy: policySignedIn, SessionOnly: true, Enrol: true, Handle: s.handleTOTPConfirm,
			Section: secAccount, Doc: "confirm with a code and switch 2FA on"},
		{Method: post, Path: "/api/account/2fa/disable", Policy: policySignedIn, SessionOnly: true, MaxBody: smallBody, Handle: s.handleTOTPDisable,
			Section: secAccount, Doc: "disable 2FA (requires your password and a current code)"},
		{Method: get, Path: "/api/account/sessions", Policy: policySignedIn, SessionOnly: true, Enrol: true, Handle: s.handleListSessions,
			Section: secAccount, Doc: "your active sessions"},
		{Method: del, Path: "/api/account/sessions/{id}", Policy: policySignedIn, SessionOnly: true, Handle: s.handleRevokeSession,
			Section: secAccount, Doc: "revoke one"},
		{Method: put, Path: "/api/account/profile", Policy: policySignedIn, SessionOnly: true, MaxBody: smallBody, Handle: s.handleUpdateProfile,
			Section: secAccount, Doc: "your first and last name and interface language"},

		// Users.
		{Method: get, Path: "/api/users", Policy: policyGlobal, Handle: plain(s.handleListUsers),
			Section: secUsers, Doc: "every account"},
		{Method: post, Path: "/api/users", Policy: policyAdmin, SessionOnly: true, MaxBody: smallBody, Handle: s.handleCreateUser,
			Section: secUsers, Doc: "create one — `role` is `admin`, `operator` or `readonly`; an operator needs `site_ids`"},
		{Method: put, Path: "/api/users/{id}", Policy: policyAdmin, SessionOnly: true, Handle: s.handleUpdateUser,
			Section: secUsers, Doc: "change an account's role, sites or name, or disable it"},
		{Method: post, Path: "/api/users/{id}/password", Policy: policyAdmin, SessionOnly: true, MaxBody: smallBody, Handle: s.handleResetUserPassword,
			Section: secUsers, Doc: "set a new password for another account"},
		{Method: del, Path: "/api/users/{id}", Policy: policyAdmin, SessionOnly: true, Handle: s.handleDeleteUser,
			Section: secUsers, Doc: "delete an account"},

		// API tokens for cgctl and scripts (apitokens.go): managed from a
		// signed-in session only.
		{Method: get, Path: "/api/tokens", Policy: policyAdmin, SessionOnly: true, Handle: plain(s.handleListAPITokens),
			Section: secTokens, Doc: "every token: name, owner, hint, created, expires, last used — never the secret"},
		{Method: post, Path: "/api/tokens", Policy: policyAdmin, SessionOnly: true, MaxBody: smallBody, Handle: s.handleCreateAPIToken,
			Section: secTokens, Doc: "create one: `name` (1–64 characters), `expires_in_days` (0 = never, at most 365); answers `201` with the secret in `token`, shown once"},
		{Method: del, Path: "/api/tokens/{id}", Policy: policyAdmin, SessionOnly: true, Handle: s.handleRevokeAPIToken,
			Section: secTokens, Doc: "revoke it; it is refused from the next request"},

		// Sites.
		{Method: get, Path: "/api/sites", Policy: policySignedIn, Handle: s.handleListSites,
			Section: secSites, Doc: "the sites the caller may see"},
		{Method: post, Path: "/api/sites", Policy: policyAdmin, AdminOnly: "create sites", Handle: s.handleCreateSite,
			Section: secSites, Doc: "create (the database name, user and password may be chosen) — returns `202`, a task and the summary, the only answer with the database password"},
		{Method: get, Path: "/api/sites/dns-check", Query: "domain=", Policy: policyManage, Handle: s.handleDNSCheck,
			Section: secSites, Doc: "whether a host name already points at this server (the certificate's readiness): `status` `here`, `partial`, `elsewhere`, `proxied` (a CDN hides the origin), `unresolved` or `unknown`, its A and AAAA `addresses` (a private one unnamed) and the server's public addresses; a lookup only, 3 s at most, 30 a minute per account"},
		{Method: get, Path: "/api/sites/{id}", Policy: policySignedIn, Load: loadSite, Handle: s.handleGetSite,
			Section: secSites, Doc: "a site and its settings"},
		{Method: del, Path: "/api/sites/{id}", Policy: policyAdmin, Load: loadSite, Handle: s.handleDeleteSite,
			Section: secSites, Doc: "delete — returns `202`"},
		{Method: patch, Path: "/api/sites/{id}", Policy: policyManage, Load: loadSite, Handle: s.handlePatchSite,
			Section: secSites, Doc: "change any of the site's settings in one request (fields below) — returns `202`, the site and one task"},
		// The routing block is nginx configuration: an administrator's,
		// from a session (internal/engine SPEC §7).
		{Method: post, Path: "/api/sites/{id}/protected-paths", Policy: policyManage, MaxBody: smallBody, Load: loadSite, Handle: s.handleSetProtectedPath,
			Section: secSites, Doc: "protect a path with a username and password, or change them (`{\"path\": \"/wp-admin\", \"username\": …, \"password\": …}`); answers the site's protected paths"},
		{Method: del, Path: "/api/sites/{id}/protected-paths", Policy: policyManage, MaxBody: smallBody, Load: loadSite, Handle: s.handleDeleteProtectedPath,
			Section: secSites, Doc: "stop protecting a path (`{\"path\": \"/wp-admin\"}`); answers the site's protected paths"},
		{Method: put, Path: "/api/sites/{id}/blocking", Policy: policyManage, MaxBody: blockingBody, Load: loadSite, Handle: s.handleSetBlocking,
			Section: secSites, Doc: "the visitors the site refuses with 403 (`{\"addresses\": [\"203.0.113.0/24\"], \"user_agents\": [\"GPTBot\"], \"countries\": [\"CN\"], \"allow_countries_only\": false}`; empty lists refuse nobody); live at once, answers the rules"},
		{Method: put, Path: "/api/sites/{id}/dev-mode", Policy: policyManage, MaxBody: smallBody, Load: loadSite, Handle: s.handleSetDevMode,
			Section: secSites, Doc: "development mode: the page cache is bypassed for `{\"minutes\": 15}`, 30 or 60 (again while it runs: the time starts over), then switched back on by the panel, which empties the cache and warms it again; `0` ends it now; answers `dev_mode_until` (null when off); 409 on a site with no page cache"},
		{Method: get, Path: "/api/sites/{id}/maintenance", Policy: policyManage, Load: loadSite, Handle: s.handleGetMaintenance,
			Section: secSites, Doc: "the maintenance page: `enabled`, `title`, `message`, `retry_after_sec`, `allow` and, while it is on, the `preview_url` that lets a browser through"},
		{Method: put, Path: "/api/sites/{id}/maintenance", Policy: policyManage, MaxBody: smallBody, Load: loadSite, Handle: s.handleSetMaintenance,
			Section: secSites, Doc: "change the maintenance page (a field left out keeps its value; `{\"enabled\": true, \"title\": …, \"message\": …, \"retry_after_sec\": 3600, \"allow\": [\"203.0.113.0/24\"], \"allow_my_address\": false}`): on, visitors get a 503 with Retry-After and the page cache is bypassed; the addresses in `allow`, this server and a browser that opened `preview_url` still see the site; live at once, answers the settings"},
		{Method: get, Path: "/api/sites/{id}/routing", Policy: policyAdmin, SessionOnly: true, Load: loadSite, Handle: s.handleGetRouting,
			Section: secSites, Doc: "the site's routing block (`routing`, `default`, `custom`, `editable`) and the panel's vhost around it (`before`, `after`)"},
		{Method: put, Path: "/api/sites/{id}/routing", Policy: policyAdmin, SessionOnly: true, MaxBody: routingBody, Load: loadSite, Handle: s.handleSetRouting,
			Section: secSites, Doc: "replace the routing block (`{\"routing\": \"…\"}`, `\"\"` restores the default): checked, then live only if `nginx -t` passes"},
		{Method: get, Path: "/api/sites/{id}/certificate", Policy: policySignedIn, Load: loadSite, Handle: s.handleCertificateStatus,
			Section: secSites, Doc: "whether the site has a certificate, its issuer and expiry, and the last renewal error"},
		{Method: post, Path: "/api/sites/{id}/certificate", Policy: policyManage, Load: loadSite, Handle: s.handleIssueCertificate,
			Section: secSites, Doc: "request a Let's Encrypt certificate now"},
		{Method: put, Path: "/api/sites/{id}/certificate/custom", Policy: policyManage, Load: loadSite, MaxBody: certBody, Handle: s.handleCustomCertificate,
			Section: secSites, Doc: "install your own certificate: `{fullchain, key}` in PEM"},
		{Method: del, Path: "/api/sites/{id}/certificate/custom", Policy: policyManage, Load: loadSite, Handle: s.handleCustomCertificate,
			Section: secSites, Doc: "remove it and go back to Let's Encrypt: the certificate for the site's current names is requested (`{status, task}`; `{status, warning}` without an ACME e-mail)"},
		{Method: post, Path: "/api/sites/{id}/purge", Policy: policyManage, Load: loadSite, Handle: s.handlePurge,
			Section: secSites, Doc: "empty the site's page cache"},
		{Method: post, Path: "/api/sites/{id}/warm", Policy: policyManage, Load: loadSite, Handle: s.handleWarmCache,
			Section: secSites, Doc: "fill the page cache from the sitemap"},
		{Method: get, Path: "/api/sites/{id}/cache-stats", Policy: policySignedIn, Load: loadSite, Handle: s.handleCacheStats,
			Section: secSites, Doc: "APCu hits, misses and memory in use"},
		{Method: get, Path: "/api/sites/{id}/logs", Query: "source=", Policy: policySignedIn, Load: loadSite, Handle: s.handleSiteLog,
			Section: secSites, Doc: "tail one of the site's own logs: `access`, `error` or `php`"},
		{Method: post, Path: "/api/sites/{id}/migration", Policy: policyManage, Load: loadSite, Handle: s.handleImportMigration,
			Section: secSites, Doc: "bring in a site from a files archive and a SQL dump"},

		// Releases, staging and Safe Push.
		{Method: get, Path: "/api/sites/{id}/deployments", Policy: policySignedIn, Load: loadSite, Handle: s.handleListDeployments,
			Section: secReleases, Doc: "past releases and the guard's verdict on each"},
		{Method: post, Path: "/api/sites/{id}/deployments", Policy: policyManage, Load: loadSite, Handle: s.handleDeploy,
			Section: secReleases, Doc: "create a release from `source_dir`, which must be inside the site's own directory"},
		{Method: post, Path: "/api/deployments/{id}/promote", Policy: policyManage, Load: loadDeployment, Handle: s.handlePromote,
			Section: secReleases, Doc: "promote a built release (`created`, `superseded`, `rolled_back` or the live one); 409 for any other status"},
		{Method: post, Path: "/api/sites/{id}/rollback", Policy: policyManage, Load: loadSite, Handle: s.handleRollback,
			Section: secReleases, Doc: "roll back to the release that was live before the current one, or to `deployment_id` (one that was live); 409 when there is none"},
		{Method: post, Path: "/api/sites/{id}/staging", Policy: policyManage, Load: loadSite, Handle: s.handleCreateStaging,
			Section: secReleases, Doc: "make a staging copy of the site"},
		{Method: post, Path: "/api/sites/{id}/staging/recreate", Policy: policyManage, Load: loadSite, Handle: s.handleRecreateStaging,
			Section: secReleases, Doc: "delete the staging copy and clone production again"},
		{Method: get, Path: "/api/sites/{id}/staging/credentials", Policy: policyManage, Load: loadSite, Handle: s.handleStagingCredentials,
			Section: secReleases, Doc: "the staging copy's URL and the user and password protecting it"},
		{Method: get, Path: "/api/sites/{id}/staging/diff", Policy: policyManage, Load: loadSite, Handle: s.handleStagingDiff,
			Section: secReleases, Doc: "what differs between staging and production"},
		{Method: get, Path: "/api/sites/{id}/staging/tables", Policy: policySignedIn, Load: loadSite, Handle: s.handleStagingTables,
			Section: secReleases, Doc: "what a database sync would copy"},
		{Method: post, Path: "/api/sites/{id}/staging/database", Policy: policyManage, Load: loadSite, Handle: s.handlePushStagingDatabase,
			Section: secReleases, Doc: "copy production's database to staging again"},
		{Method: post, Path: "/api/sites/{id}/safe-push", Policy: policyManage, Load: loadSite, Handle: s.handleSafePush,
			Section: secReleases, Doc: "promote staging if its measurements pass"},

		// Backups.
		{Method: get, Path: "/api/sites/{id}/backups", Policy: policySignedIn, Load: loadSite, Handle: s.handleListBackups,
			Section: secBackups, Doc: "the site's backups, each with the `databases` its snapshot holds (`null` for one from before they were recorded)"},
		{Method: post, Path: "/api/sites/{id}/backups", Policy: policyManage, Load: loadSite, Handle: s.handleRunBackup,
			Section: secBackups, Doc: "take one, then restore-test it"},
		{Method: post, Path: "/api/backups/{id}/verify", Policy: policyManage, Load: loadBackup, Handle: s.handleVerifyBackup,
			Section: secBackups, Doc: "test-restore the backup to a scratch place"},
		{Method: post, Path: "/api/backups/{id}/restore", Policy: policyManage, Load: loadBackup, Handle: s.handleRestoreBackup,
			Section: secBackups, Doc: "restore in place — `mode` is `full`, `files` or `database`; `confirm` is the domain; `databases`, optional, restores only these of the snapshot's (a backup lists them in `databases`)"},
		{Method: del, Path: "/api/backups/{id}", Policy: policyAdmin, Load: loadBackup, Handle: s.handleDeleteBackup,
			Section: secBackups, Doc: "delete the snapshot from the repository (queued under its lock); refused for one whose restore test has not run and for the site's newest verified one"},
		{Method: post, Path: "/api/backups/test", Policy: policyAdmin, Handle: s.handleTestBackupRepo,
			Section: secBackups, Doc: "try the backup repository's credentials"},
		{Method: get, Path: "/api/sites/{id}/usage", Policy: policySignedIn, Load: loadSite, Handle: s.handleSiteUsage,
			Section: secSites, Doc: "the site's resource usage for `range` `24h` (five-minute samples) or `30d` (hourly averages, with the hour's CPU peak): CPU as a share of its allowance, memory, IO and disk; its effective `limits`; the `latest` sample"},
		{Method: get, Path: "/api/sites/{id}/vulnerabilities", Policy: policySignedIn, Load: loadSite, Handle: s.handleGetVulnerabilities,
			Section: secSites, Doc: "a WordPress site's last vulnerability check: `enabled` (the panel's switch), `checked`, `checked_at`, `attempted_at`, `error`, `core`, `components` and the `findings` (`component`, `slug`, `version`, `title`, `severity`, `cve`, `fixed_in`, `unfixed`, `link`); `source` names the database"},
		{Method: post, Path: "/api/sites/{id}/vulnerabilities/check", Policy: policyManage, Load: loadSite, Handle: s.handleCheckVulnerabilities,
			Section: secSites, Doc: "check a WordPress site's core, plugins and themes against the vulnerability database now; 202 with the task"},
		{Method: get, Path: "/api/sites/{id}/malware", Policy: policySignedIn, Load: loadSite, Handle: s.handleGetMalware,
			Section: secSites, Doc: "the site's malware scans: `enabled` (the panel's switch), `scanned`, `next_scan`, the last `scan` (`scanned_at`, `attempted_at`, `error`, `files`, `signatures`, `duration_ms`, `engine`, `database`) and the `findings` (`id`, `path`, `signature`, `status` open, quarantined or gone, `first_seen`, `last_seen`, `quarantined_at`)"},
		{Method: post, Path: "/api/sites/{id}/malware/scan", Policy: policyManage, Load: loadSite, Handle: s.handleScanMalware,
			Section: secSites, Doc: "scan the site's files for malware now (needs `malware_scan` on); 202 with the task"},
		{Method: post, Path: "/api/sites/{id}/malware/findings/{finding}/quarantine", Policy: policyManage, Load: loadSite, Handle: s.handleQuarantine,
			Section: secSites, Doc: "move an open finding's file out of the site into the agent's quarantine; 200 with the finding"},
		{Method: get, Path: "/api/sites/{id}/uptime", Policy: policySignedIn, Load: loadSite, Handle: s.handleGetUptime,
			Section: secSites, Doc: "the site's uptime check: `enabled`, `path`, `url`, `interval_seconds` and the `last` result (`up`, `status`, `latency_ms`, `error`, `down_since`)"},
		{Method: put, Path: "/api/sites/{id}/uptime", Policy: policyManage, MaxBody: smallBody, Load: loadSite, Handle: s.handleSetUptime,
			Section: secSites, Doc: "turn the uptime check on or off (`enabled`) and choose the path it requests (`path`, default `/`)"},
		{Method: put, Path: "/api/sites/{id}/backups/policy", Policy: policyManage, MaxBody: smallBody, Load: loadSite, Handle: s.handleBackupPolicy,
			Section: secBackups, Doc: "the site's backup policy: any of `enabled`, `schedule` (`hourly`, `daily`, `weekly`), `hour` (0–23, the panel's time zone, for daily and weekly; -1 for none), `retention_days` (0 keeps every snapshot, at most 3650; changing it is an admin's); answers the policy"},
		{Method: post, Path: "/api/sites/{id}/backups/retention", Policy: policyAdmin, Load: loadSite, Handle: s.handleBackupRetention,
			Section: secBackups, Doc: "apply the site's retention window now (it also runs daily)"},

		// Files. Reading a file needs site management rights: a site's tree
		// holds its wp-config.php (database password, salts, connector
		// token) and its database exports, enough together to sign in as
		// any of its users. Read-only accounts can still list.
		{Method: get, Path: "/api/sites/{id}/files", Query: "path=", Policy: policySignedIn, Load: loadSite, Handle: s.handleListFiles,
			Section: secFiles, Doc: "entries of a folder"},
		{Method: get, Path: "/api/sites/{id}/files/read", Query: "path=", Policy: policyManage, Load: loadSite, Handle: s.handleReadFile,
			Section: secFiles, Doc: "read a text file (2 MB cap)"},
		{Method: post, Path: "/api/sites/{id}/files/write", Policy: policyManage, Load: loadSite, Handle: s.handleWriteFile,
			Section: secFiles, Doc: "save a text file"},
		{Method: get, Path: "/api/sites/{id}/files/download", Query: "path=", Policy: policyManage, Load: loadSite, Handle: s.handleDownloadFile,
			Section: secFiles, Doc: "download (16 MB cap; use SFTP above that)"},
		{Method: post, Path: "/api/sites/{id}/files/upload", Query: "path=", Policy: policyManage, Load: loadSite, Body: bodyUpload, MaxBody: fileUploadBody, Handle: s.handleUploadFile,
			Section: secFiles, Doc: "upload the raw file as `application/octet-stream` (16 MB cap)"},
		{Method: post, Path: "/api/sites/{id}/files/manage", Policy: policyManage, Load: loadSite, Handle: s.handleManageFile,
			Section: secFiles, Doc: "remove, rename or create a folder"},

		// Database: a site's databases and database users (Fase 9b).
		{Method: get, Path: "/api/sites/{id}/databases", Policy: policySignedIn, Load: loadSite, Handle: s.handleListDatabases,
			Section: secDatabase, Doc: "the site's databases (`name`, `primary`, `size_bytes`, `tables`; -1 when unknown) and database users with their `grants` (`database`, `level`: `all` or `read`)"},
		{Method: post, Path: "/api/sites/{id}/databases", Policy: policyManage, Load: loadSite, Handle: s.handleCreateDatabase,
			Section: secDatabase, Doc: "create a database: `name` (3–48 of `a-z0-9_`, a letter first, never a reserved or system name, unique on the server) — returns `202` and a task"},
		{Method: del, Path: "/api/sites/{id}/databases/{name}", Query: "confirm=", Policy: policyManage, Load: loadSite, Handle: s.handleDropDatabase,
			Section: secDatabase, Doc: "delete a database (not the primary); `confirm` is its name; its backups are kept — returns `202` and a task"},
		{Method: post, Path: "/api/sites/{id}/database-users", Policy: policyManage, Load: loadSite, Handle: s.handleCreateDBUser,
			Section: secDatabase, Doc: "create a database user: `name`, `password` (12+ characters; empty generates one, returned once in `password`), `grants`"},
		{Method: del, Path: "/api/sites/{id}/database-users/{name}", Query: "confirm=", Policy: policyManage, Load: loadSite, Handle: s.handleDropDBUser,
			Section: secDatabase, Doc: "delete a database user (not the primary); `confirm` is its name"},
		{Method: post, Path: "/api/sites/{id}/database-users/{name}/password", Policy: policyManage, Load: loadSite, Handle: s.handleSetDBUserPassword,
			Section: secDatabase, Doc: "set its password (`password`; empty generates one, returned once); the primary user of a WordPress site gets it in `wp-config.php` too"},
		{Method: put, Path: "/api/sites/{id}/database-users/{name}/grants", Policy: policyManage, Load: loadSite, Handle: s.handleSetDBUserGrants,
			Section: secDatabase, Doc: "replace its `grants` on the site's databases; the primary user keeps `all` on the primary database"},
		// The tools on one database (`database`, or `?database=` on GET; empty is the primary).
		{Method: get, Path: "/api/sites/{id}/database", Query: "database=", Policy: policySignedIn, Load: loadSite, Handle: s.handleDatabaseInfo,
			Section: secDatabase, Doc: "each table with its rows and size"},
		{Method: post, Path: "/api/sites/{id}/database/query", Policy: policyManage, Load: loadSite, Statements: true, Handle: s.handleDatabaseQuery,
			Section: secDatabase, Doc: "run one SQL statement"},
		{Method: post, Path: "/api/sites/{id}/database/export", Policy: policyManage, Load: loadSite, Handle: s.handleDatabaseExport,
			Section: secDatabase, Doc: "write a dump into the site's `shared/exports/`"},
		{Method: post, Path: "/api/sites/{id}/database/import", Policy: policyManage, Load: loadSite, Handle: s.handleDatabaseImport,
			Section: secDatabase, Doc: "load a `.sql` file found in the site"},

		// Quarry, a service of its own at /quarry (SPEC.md §6.4): its
		// own session and cookie. A panel session reaches none of these
		// routes, a Quarry session none of the panel's. Tables and
		// structure for every session; rows, SQL, search, transfers and
		// writes for a session that may read the data (a database user
		// signed in with its password, whose grants decide; or one opened
		// from the panel by an admin or operator: SR-T4).
		{Method: post, Path: "/api/quarry/login", Policy: policyOpen, MaxBody: smallBody, Handle: plain(s.handleStudioLogin),
			Section: secDBStudio, Doc: "sign in with a database user's own `user` and `password` (and optionally the `database` to open); the server checks them, and the session sees what that user is granted"},
		{Method: post, Path: "/api/sites/{id}/quarry/handoff", Policy: policySignedIn, SessionOnly: true, MaxBody: smallBody, Load: loadSite, Handle: s.handleStudioHandoffMint,
			Section: secDBStudio, Doc: "a one-time link into Quarry for this site (`{database}` optional): `token`, valid 60 seconds, for this panel session only"},
		{Method: post, Path: "/api/quarry/handoff", Policy: policyOpen, MaxBody: smallBody, Handle: plain(s.handleStudioHandoff),
			Section: secDBStudio, Doc: "exchange a handoff `token` for a Quarry session, with the panel session that asked for it"},
		{Method: get, Path: "/api/quarry/session", Policy: policyOpen, Handle: plain(s.handleStudioSession),
			Section: secDBStudio, Doc: "the Quarry session (`mode` `login` or `panel`, `user`, `site`, `database`, `read_only`), or `{\"session\": null}`"},
		{Method: post, Path: "/api/quarry/logout", Policy: policyStudio, Handle: s.handleStudioLogout,
			Section: secDBStudio, Doc: "sign out of Quarry"},
		{Method: get, Path: "/api/quarry/databases", Policy: policyStudio, Handle: s.handleStudioDatabases,
			Section: secDBStudio, Doc: "the databases the session may open"},
		{Method: get, Path: "/api/quarry/db/{db}/tables", Query: "columns=", Policy: policyStudio, Handle: s.handleStudioTables,
			Section: secDBStudio, Doc: "tables and views (rows, size, engine, collation) and routines (`columns=1` adds every table's columns)"},
		{Method: get, Path: "/api/quarry/db/{db}/tables/{table}", Policy: policyStudio, Handle: s.handleStudioStructure,
			Section: secDBStudio, Doc: "structure: columns, indexes, foreign keys, primary key, `CREATE` statement"},
		{Method: post, Path: "/api/quarry/db/{db}/tables/{table}/rows", Policy: policyStudioData, Statements: true, Handle: s.handleStudioRows,
			Section: secDBStudio, Doc: "a page of rows: `{page, page_size, sort: [{column, desc}], filters: [{column, op, value}], search}`"},
		{Method: post, Path: "/api/quarry/db/{db}/tables/{table}/edit", Policy: policyStudioData, MaxBody: dbEditBody, Statements: true, Handle: s.handleStudioEdit,
			Section: secDBStudio, Doc: "insert, update or delete by primary key in one transaction: `{changes: [{op, key, values}], dry_run}`"},
		{Method: post, Path: "/api/quarry/db/{db}/tables/{table}/alter", Policy: policyStudioData, Statements: true, Handle: s.handleStudioAlter,
			Section: secDBStudio, Doc: "add a column or an index, or empty or drop the table: `{kind: add_column \\| add_index \\| truncate \\| drop, column, index, confirm, dry_run}` (`confirm` is the table's name)"},
		{Method: post, Path: "/api/quarry/db/{db}/query", Policy: policyStudioData, Statements: true, Handle: s.handleStudioQuery,
			Section: secDBStudio, Doc: "the SQL editor: `{sql, allow_writes, explain}` (`allow_writes` for a session opened from the panel)"},
		{Method: post, Path: "/api/quarry/db/{db}/simulate", Policy: policyStudioData, Statements: true, Handle: s.handleStudioSimulate,
			Section: secDBStudio, Doc: "simulate one UPDATE, DELETE, INSERT or REPLACE in a transaction that is always rolled back: `{sql}` → `{affected, preview, refused, message}`"},
		{Method: post, Path: "/api/quarry/db/{db}/search", Policy: policyStudioData, Statements: true, Handle: s.handleStudioSearch,
			Section: secDBStudio, Doc: "count the rows containing `term` in each table (or `tables`)"},
		{Method: post, Path: "/api/quarry/db/{db}/export", Policy: policyStudioData, Handle: s.handleStudioExport,
			Section: secDBStudio, Doc: "export, as a task: `{format: sql \\| csv, tables}` (CSV: one table)"},
		{Method: post, Path: "/api/quarry/db/{db}/import", Policy: policyStudioData, Handle: s.handleStudioImport,
			Section: secDBStudio, Doc: "import an upload, as a task: `{name, format: sql, confirm: <database>}` or `{name, format: csv, table}`"},
		{Method: get, Path: "/api/quarry/files", Policy: policyStudioData, Handle: s.handleStudioFiles,
			Section: secDBStudio, Doc: "the exports and uploads the session may see (a database user: its own)"},
		{Method: get, Path: "/api/quarry/exports/{name}", Policy: policyStudioData, Handle: s.handleStudioDownload,
			Section: secDBStudio, Doc: "download an export, streamed"},
		{Method: del, Path: "/api/quarry/files/{dir}/{name}", Policy: policyStudioData, Handle: s.handleStudioDeleteFile,
			Section: secDBStudio, Doc: "delete an export or an upload (`dir`: `exports` or `imports`)"},
		{Method: post, Path: "/api/quarry/import/upload", Query: "name=&offset=", Policy: policyStudioData, Body: bodyUpload, MaxBody: dbChunkBody, Handle: s.handleStudioUpload,
			Section: secDBStudio, Doc: "one chunk (at most 8 MiB, `application/octet-stream`) of an upload to `shared/imports`"},
		{Method: get, Path: "/api/quarry/tasks/{id}", Policy: policyStudio, Handle: s.handleStudioTask,
			Section: secDBStudio, Doc: "one of the tasks the session started"},
		// The WordPress tools stay the panel's: they run wp-cli as the site.
		{Method: get, Path: "/api/sites/{id}/quarry/wp/autoload", Policy: policySignedIn, Load: loadSite, Handle: s.handleDBStudioAutoload,
			Section: secDBStudio, Doc: "WordPress: size of the autoloaded options and the largest ones"},
		{Method: post, Path: "/api/sites/{id}/quarry/wp/search-replace", Policy: policyManage, Load: loadSite, Handle: s.handleDBStudioSearchReplace,
			Section: secDBStudio, Doc: "WordPress: serialized-safe search and replace through wp-cli: `{from, to, dry_run}`"},
		{Method: post, Path: "/api/sites/{id}/quarry/wp/transients", Policy: policyManage, Load: loadSite, Handle: s.handleDBStudioTransients,
			Section: secDBStudio, Doc: "WordPress: delete (or with `dry_run`, count) the expired transients"},

		// WordPress.
		{Method: post, Path: "/api/sites/{id}/wp/login", Policy: policyManage, Load: loadSite, Handle: s.handleWPMagicLogin,
			Section: secWP, Doc: "a sign-in link for wp-admin, valid once for 5 minutes"},
		{Method: get, Path: "/api/sites/{id}/wp/plugins", Policy: policySignedIn, Load: loadSite, Handle: s.handleWPPlugins,
			Section: secWP, Doc: "installed plugins and themes, and their updates"},
		{Method: post, Path: "/api/sites/{id}/wp/update", Policy: policyManage, Load: loadSite, Handle: s.handleWPUpdate,
			Section: secWP, Doc: "apply updates to `core`, `plugins`, `themes` or `all`"},
		{Method: put, Path: "/api/sites/{id}/wp/hardening", Policy: policyManage, MaxBody: smallBody, Load: loadSite, Handle: s.handleSetWPHardening,
			Section: secWP, Doc: "WordPress hardening, each item on its own: `{\"xmlrpc_off\", \"xmlrpc_allow\": [addresses or networks], \"file_edit_off\", \"hide_version\", \"block_readme\", \"login_path\"}`; answers the hardening"},
		{Method: put, Path: "/api/sites/{id}/wp/two-factor", Policy: policyManage, MaxBody: smallBody, Load: loadSite, Handle: s.handleSetWPTwoFactor,
			Section: secWP, Doc: "two-factor sign-in for the site's accounts: `{\"mode\": \"off\"}`, `optional` (each account can set it up) or `required` (also compulsory for administrators and shop managers); answers the mode"},

		// SFTP and SSH keys.
		{Method: post, Path: "/api/sites/{id}/sftp", Policy: policyManage, Load: loadSite, Handle: s.handleSetSFTP,
			Section: secSFTP, Doc: "turn SFTP on (`keys_only` and an optional `public_key`, or a new `password`) or off (`enable`)"},
		{Method: get, Path: "/api/sites/{id}/ssh-keys", Policy: policySignedIn, Load: loadSite, Handle: s.handleListSSHKeys,
			Section: secSFTP, Doc: "the site's SSH keys"},
		{Method: post, Path: "/api/sites/{id}/ssh-keys", Policy: policyManage, Load: loadSite, Handle: s.handleAddSSHKey,
			Section: secSFTP, Doc: "add a key (`public_key`, `access`: `sftp` or `shell`; a `shell` key only from an admin's session)"},
		{Method: del, Path: "/api/sites/{id}/ssh-keys/{fingerprint}", Policy: policyManage, Load: loadSite, Handle: s.handleDeleteSSHKey,
			Section: secSFTP, Doc: "remove that key"},
		{Method: post, Path: "/api/sites/{id}/ssh-shell", Policy: policyAdmin, SessionOnly: true, Load: loadSite, Handle: s.handleSetShell,
			Section: secSFTP, Doc: "turn the site's SSH shell on or off (`{\"enable\": bool}`)"},

		// Cron.
		{Method: get, Path: "/api/sites/{id}/cron", Policy: policySignedIn, Load: loadSite, Handle: s.handleListCron,
			Section: secCron, Doc: "this site's jobs; `last_run` and `last_status` are the latest run, scheduled or on demand; for a read-only account `command` is redacted as in the audit log (first word, word count, fingerprint)"},
		{Method: post, Path: "/api/sites/{id}/cron", Policy: policyManage, Load: loadSite, Handle: s.handleCreateCron,
			Section: secCron, Doc: "create one (`schedule`, `command`, `description`, `enabled`)"},
		{Method: patch, Path: "/api/cron/{id}", Policy: policyManage, Load: loadCron, Handle: s.handleUpdateCron,
			Section: secCron, Doc: "change any of those fields; a disabled job leaves the schedule"},
		{Method: del, Path: "/api/cron/{id}", Policy: policyManage, Load: loadCron, Handle: s.handleDeleteCron,
			Section: secCron, Doc: "remove the job"},
		{Method: post, Path: "/api/cron/{id}/run", Policy: policyManage, Load: loadCron, Handle: s.handleRunCron,
			Section: secCron, Doc: "start the job at once"},

		// Server.
		{Method: get, Path: "/api/system/status", Policy: policyGlobal, Handle: plain(s.handleSystemStatus),
			Section: secServer, Doc: "free CPU, memory and disk, and the services"},
		{Method: get, Path: "/api/system/metrics", Policy: policyGlobal, Handle: plain(s.handleSystemMetrics),
			Section: secServer, Doc: "capacity samples of the last 24 hours"},
		// The panel's own version, shown to everyone.
		{Method: get, Path: "/api/system/capabilities", Policy: policySignedIn, Handle: plain(s.handleCapabilities),
			Section: secServer, Doc: "what the agent reports this server has: installed PHP versions, services, roots"},
		{Method: get, Path: "/api/system/update", Policy: policySignedIn, Handle: s.handleUpdateStatus,
			Section: secServer, Doc: "the running version and the newest release (`?refresh=1`, admins only, checks now)"},
		{Method: get, Path: "/api/system/alerts", Policy: policySignedIn, Handle: s.handleAlerts,
			Section: secServer, Doc: "what needs attention, filtered to the caller's sites"},
		{Method: post, Path: "/api/system/alerts/{id}/dismiss", Policy: policyManage, Handle: s.handleDismissAlert,
			Section: secServer, Doc: "dismiss one the caller can see"},
		{Method: get, Path: "/api/system/drift", Policy: policyGlobal, Handle: plain(s.handleListDrift),
			Section: secServer, Doc: "managed files changed outside the panel"},
		{Method: post, Path: "/api/system/drift/{id}/resolve", Policy: policyAdmin, Handle: s.handleResolveDrift,
			Section: secServer, Doc: "`{\"action\":\"restore\"}` puts the file back, `{\"action\":\"accept\"}` keeps it (accept: a signed-in session only, an API token gets 403)"},
		{Method: post, Path: "/api/system/maintenance", Policy: policyAdmin, Handle: s.handleRunMaintenance,
			Section: secServer, Doc: "run one maintenance tick now and wait for it"},
		{Method: get, Path: "/api/php/{version}/extensions", Policy: policySignedIn, Handle: s.handlePHPExtensions,
			Section: secServer, Doc: "what a PHP version loads: every module, and the common extensions with `installed`"},
		{Method: post, Path: "/api/php/{version}/extensions", Policy: policyAdmin, Handle: s.handleInstallPHPExtension,
			Section: secServer, Doc: "install one of the common extensions (`{\"extension\": \"redis\"}`) and restart the PHP of the sites on that version; answers the task"},
		{Method: get, Path: "/api/services", Policy: policyGlobal, Handle: plain(s.handleListServices),
			Section: secServer, Doc: "whether nginx, PHP-FPM, MariaDB and Redis run"},
		{Method: post, Path: "/api/services/{name}/restart", Policy: policyAdmin, Handle: s.handleRestartService,
			Section: secServer, Doc: "restart that service"},
		{Method: get, Path: "/api/logs", Policy: policyGlobal, Handle: plain(s.handleReadLog),
			Section: secServer, Doc: "the last lines of a server log"},
		{Method: get, Path: "/api/firewall", Policy: policyGlobal, Handle: plain(s.handleFirewallStatus),
			Section: secServer, Doc: "the firewall's rules"},
		// The panel's own governance (its firewall, certificate and
		// binary) is a session's: an API token automates sites
		// (SPEC.md §1.2 step 6).
		{Method: post, Path: "/api/firewall/rule", Policy: policyAdmin, SessionOnly: true, Handle: s.handleFirewallRule,
			Section: secServer, Doc: "add a firewall rule or take one away"},
		{Method: post, Path: "/api/firewall/enable", Policy: policyAdmin, SessionOnly: true, Handle: s.handleFirewallEnable,
			Section: secServer, Doc: "turn ufw on: allow the rules `GET /api/firewall` listed as `plan` (`{\"expect\": [\"22/tcp\", …]}`), then enable"},
		// The block list (#21, SPEC.md §7.4): reading it is server-wide
		// data; changing it is the panel's governance, a session's.
		{Method: get, Path: "/api/blocklist", Policy: policyGlobal, Handle: plain(s.handleGetBlocklist),
			Section: secServer, Doc: "the block list: blocked addresses (by hand or for failed sign-ins) with their end, the allowlist, the automatic blocking's settings, whether netfilter enforces it, and the caller's own address"},
		{Method: post, Path: "/api/blocklist/blocks", Policy: policyAdmin, SessionOnly: true, MaxBody: smallBody, Handle: s.handleBlock,
			Section: secServer, Doc: "block an address or a network (`{\"address\": \"203.0.113.0/24\", \"note\": …, \"minutes\": 0}`, 0 is until removed); answers the block list"},
		{Method: del, Path: "/api/blocklist/blocks", Policy: policyAdmin, SessionOnly: true, MaxBody: smallBody, Handle: s.handleUnblock,
			Section: secServer, Doc: "remove a block (`{\"address\": …}`); answers the block list"},
		{Method: post, Path: "/api/blocklist/allowed", Policy: policyAdmin, SessionOnly: true, MaxBody: smallBody, Handle: s.handleAllow,
			Section: secServer, Doc: "never block an address or a network (`{\"address\": …, \"note\": …}`); its automatic blocks end; answers the block list"},
		{Method: del, Path: "/api/blocklist/allowed", Policy: policyAdmin, SessionOnly: true, MaxBody: smallBody, Handle: s.handleDisallow,
			Section: secServer, Doc: "take an address off the allowlist (`{\"address\": …}`); answers the block list"},
		{Method: put, Path: "/api/blocklist/settings", Policy: policyAdmin, SessionOnly: true, MaxBody: smallBody, Handle: s.handlePutBlocklistSettings,
			Section: secServer, Doc: "the automatic blocking: `enabled`, `max_failures` (3–100) in `window_minutes` (1–1440) block for `ban_minutes` (1–43200); answers the block list"},
		{Method: get, Path: "/api/audit", Policy: policyGlobal, Handle: plain(s.handleAudit),
			Section: secServer, Doc: "the audit log: actor, action, address"},
		{Method: get, Path: "/api/settings", Policy: policyGlobal, Handle: s.handleGetSettings,
			Section: secServer, Doc: "the panel's settings"},
		{Method: put, Path: "/api/settings", Policy: policyAdmin, Handle: s.handlePutSettings,
			Section: secServer, Doc: "change the panel's settings (an API token may change only `acme_email` and `probe_target`)"},
		{Method: post, Path: "/api/panel/certificate", Policy: policyAdmin, SessionOnly: true, Handle: s.handlePanelCertificate,
			Section: secServer, Doc: "give the panel its own certificate"},
		{Method: post, Path: "/api/panel/update", Policy: policyAdmin, SessionOnly: true, Handle: s.handleSelfUpdate,
			Section: secServer, Doc: "record an update request for the panel"},

		// Tasks and events. Task lists are filtered to an operator's own
		// sites in the handler; so is the event stream, per event.
		{Method: get, Path: "/api/tasks", Policy: policySignedIn, Handle: s.handleListTasks,
			Section: secTasks, Doc: "the latest tasks"},
		{Method: get, Path: "/api/tasks/names", Policy: policySignedIn, Handle: s.handleTaskNames,
			Section: secTasks, Doc: "every task kind in words, in the reader's language (`{\"backup.run\": \"Backup\"}`)"},
		{Method: get, Path: "/api/tasks/{id}", Policy: policySignedIn, Handle: s.handleGetTask,
			Section: secTasks, Doc: "a task and its log"},
		// The task's site is checked in the handler, as for the reads; a
		// server-wide task is an admin's to cancel.
		{Method: post, Path: "/api/tasks/{id}/cancel", Policy: policyManage, Handle: s.handleCancelTask,
			Section: secTasks, Doc: "cancel a queued task, or ask a running one to stop"},
		{Method: get, Path: "/api/events", Policy: policySignedIn, SameOriginRead: true, Handle: plain(s.handleEvents),
			Section: secTasks, Doc: "server-sent events stream for live updates, filtered per user"},

		// Connector: a per-site bearer token, not a session.
		{Method: post, Path: "/api/connector/purge", Policy: policyToken, Loopback: true, Handle: plain(s.handleConnectorPurge),
			Section: secConn, Doc: "the WordPress connector's purge request"},
	}
}
