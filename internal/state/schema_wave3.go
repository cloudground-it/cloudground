package state

// Phase 6 and Phase 8 steps. Versions are positions in schemaSteps, so a
// number reserved for a stream that has not landed yet still needs a step.

// Version 20: reserved for the UI stream (P8). It changes nothing, so the
// release channel below keeps the number it was given; the UI's own change,
// when it comes, is a new step after the last.
var stepReservedUI = schemaStep{version: 20, stmts: nil}

// Version 21: the release channel the panel follows (R-6.3). stable takes
// only vX.Y.Z releases, beta also vX.Y.Z-beta.N; the CHECK refuses any
// other value, so a typo cannot leave a panel following nothing.
var stepUpdateChannel = schemaStep{version: 21, stmts: []string{
	`ALTER TABLE panel_settings ADD COLUMN update_channel TEXT NOT NULL DEFAULT 'stable'
		CHECK (update_channel IN ('stable', 'beta'))`,
}}

// Version 22: a site has zero or more databases and zero or more database
// accounts, each account granted all privileges or read only on some of
// the site's databases (Fase 9b, SPEC.md "Site databases"). Names are
// unique across the server: MariaDB has one namespace for each. An
// account's password is a secret; secret names the row of secrets that
// holds it (an account made before this step keeps the site's
// site.<id>.db_password, which is sealed under that name). Every site with
// a local managed database gets it as its first database, its account as
// its first account, granted all of it: names and password unchanged.
var stepSiteDatabases = schemaStep{version: 22, stmts: []string{
	`CREATE TABLE site_databases (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		site_id    INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
		name       TEXT NOT NULL UNIQUE,
		created_at TEXT NOT NULL
	)`,
	`CREATE INDEX idx_site_databases_site ON site_databases(site_id)`,
	`CREATE TABLE site_db_users (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		site_id    INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
		name       TEXT NOT NULL UNIQUE,
		secret     TEXT NOT NULL,
		created_at TEXT NOT NULL
	)`,
	`CREATE INDEX idx_site_db_users_site ON site_db_users(site_id)`,
	`CREATE TABLE site_db_grants (
		user_id     INTEGER NOT NULL REFERENCES site_db_users(id) ON DELETE CASCADE,
		database_id INTEGER NOT NULL REFERENCES site_databases(id) ON DELETE CASCADE,
		level       TEXT NOT NULL CHECK (level IN ('all', 'read')),
		PRIMARY KEY (user_id, database_id)
	)`,
	`CREATE INDEX idx_site_db_grants_database ON site_db_grants(database_id)`,
	`INSERT OR IGNORE INTO site_databases (site_id, name, created_at)
		SELECT id, json_extract(config, '$.database.name'), created_at FROM sites
		WHERE json_extract(config, '$.database.enabled') = 1
		  AND IFNULL(json_extract(config, '$.database.external'), 0) = 0
		  AND IFNULL(json_extract(config, '$.database.name'), '') <> ''`,
	`INSERT OR IGNORE INTO site_db_users (site_id, name, secret, created_at)
		SELECT id, json_extract(config, '$.database.user'), 'site.' || id || '.db_password', created_at FROM sites
		WHERE json_extract(config, '$.database.enabled') = 1
		  AND IFNULL(json_extract(config, '$.database.external'), 0) = 0
		  AND IFNULL(json_extract(config, '$.database.user'), '') <> ''`,
	`INSERT OR IGNORE INTO site_db_grants (user_id, database_id, level)
		SELECT u.id, d.id, 'all' FROM sites s
		JOIN site_databases d ON d.site_id = s.id AND d.name = json_extract(s.config, '$.database.name')
		JOIN site_db_users u ON u.site_id = s.id AND u.name = json_extract(s.config, '$.database.user')`,
}}

// Version 23: a backup record keeps the names of the databases its
// snapshot holds, so a restore can offer to choose among them (#15).
// NULL is a record from before, which does not know them.
var stepBackupDatabases = schemaStep{version: 23, stmts: []string{
	`ALTER TABLE backups ADD COLUMN databases TEXT`,
}}

// stepSiteUsage keeps each site's resource usage (#18): a five-minute sample
// table for the last day, and one row per site and hour for 30 days, both
// pruned as they are written (site_usage.go).
var stepSiteUsage = schemaStep{version: 24, stmts: []string{
	`CREATE TABLE site_usage (
		id             INTEGER PRIMARY KEY AUTOINCREMENT,
		site_id        INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
		sampled_at     TEXT NOT NULL,
		cpu_usec       INTEGER NOT NULL,
		io_read_bytes  INTEGER NOT NULL,
		io_write_bytes INTEGER NOT NULL,
		cpu_cores      REAL NOT NULL,
		mem_bytes      INTEGER NOT NULL,
		mem_max_bytes  INTEGER NOT NULL,
		io_read_bps    REAL NOT NULL,
		io_write_bps   REAL NOT NULL,
		tasks          INTEGER NOT NULL,
		oom_kills      INTEGER NOT NULL,
		disk_bytes     INTEGER NOT NULL,
		disk_inodes    INTEGER NOT NULL
	)`,
	`CREATE INDEX idx_site_usage_site_time ON site_usage(site_id, sampled_at)`,
	`CREATE TABLE site_usage_hourly (
		site_id       INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
		hour          TEXT NOT NULL,
		n             INTEGER NOT NULL,
		cpu_cores_sum REAL NOT NULL,
		cpu_cores_max REAL NOT NULL,
		mem_bytes_sum REAL NOT NULL,
		mem_bytes_max INTEGER NOT NULL,
		mem_max_bytes INTEGER NOT NULL,
		io_read_sum   REAL NOT NULL,
		io_write_sum  REAL NOT NULL,
		disk_bytes    INTEGER NOT NULL,
		PRIMARY KEY (site_id, hour)
	)`,
}}

// stepVulnerabilities keeps each WordPress site's last vulnerability check
// (#31, site_vulnerabilities.go): one row per site, the findings as JSON,
// and the panel-wide switch of the check (on by default, like the release
// check: it sends the installed slugs and versions to a public database).
var stepVulnerabilities = schemaStep{version: 25, stmts: []string{
	`CREATE TABLE site_vulnerabilities (
		site_id      INTEGER PRIMARY KEY REFERENCES sites(id) ON DELETE CASCADE,
		checked_at   TEXT NOT NULL DEFAULT '',
		attempted_at TEXT NOT NULL,
		error        TEXT NOT NULL DEFAULT '',
		report       TEXT NOT NULL DEFAULT '{}'
	)`,
	`ALTER TABLE panel_settings ADD COLUMN vulnerability_check INTEGER NOT NULL DEFAULT 1`,
}}

// stepMalware keeps each site's last malware scan and what it found (#76,
// site_malware.go): one row per site for the scan, one per finding (a file
// and the signature that matched it), and the panel's switch of the
// scheduled scans (off by default: turning it on installs ClamAV).
var stepMalware = schemaStep{version: 26, stmts: []string{
	`CREATE TABLE site_malware_scans (
		site_id      INTEGER PRIMARY KEY REFERENCES sites(id) ON DELETE CASCADE,
		scanned_at   TEXT NOT NULL DEFAULT '',
		attempted_at TEXT NOT NULL,
		error        TEXT NOT NULL DEFAULT '',
		files        INTEGER NOT NULL DEFAULT 0,
		signatures   INTEGER NOT NULL DEFAULT 0,
		duration_ms  INTEGER NOT NULL DEFAULT 0,
		engine       TEXT NOT NULL DEFAULT '',
		database     TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE TABLE site_malware_findings (
		id             INTEGER PRIMARY KEY AUTOINCREMENT,
		site_id        INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
		path           TEXT NOT NULL,
		signature      TEXT NOT NULL,
		status         TEXT NOT NULL CHECK (status IN ('open', 'quarantined', 'gone')),
		first_seen     TEXT NOT NULL,
		last_seen      TEXT NOT NULL,
		quarantined_at TEXT NOT NULL DEFAULT '',
		quarantine_id  TEXT NOT NULL DEFAULT '',
		UNIQUE (site_id, path, signature)
	)`,
	`ALTER TABLE panel_settings ADD COLUMN malware_scan INTEGER NOT NULL DEFAULT 0`,
}}

// stepWhatsNew remembers, per account, the release whose changelog entry it
// last saw (#106); empty until it has seen one.
var stepWhatsNew = schemaStep{version: 28, stmts: []string{
	`ALTER TABLE users ADD COLUMN whats_new_seen TEXT NOT NULL DEFAULT ''`,
}}

// stepDBServers registers external MariaDB and MySQL servers (#30, api SPEC
// §6.5.1) and says which one each database and account is on (0: the box's
// own, as every row from before). A server's password is a sealed secret.
var stepDBServers = schemaStep{version: 33, stmts: []string{
	`CREATE TABLE db_servers (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		name       TEXT NOT NULL UNIQUE,
		kind       TEXT NOT NULL DEFAULT 'mariadb',
		host       TEXT NOT NULL,
		port       INTEGER NOT NULL,
		tls        TEXT NOT NULL DEFAULT 'required' CHECK (tls IN ('required', 'off')),
		ca_cert    TEXT NOT NULL DEFAULT '',
		admin_user TEXT NOT NULL,
		created_at TEXT NOT NULL
	)`,
	`ALTER TABLE site_databases ADD COLUMN server_id INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE site_db_users ADD COLUMN server_id INTEGER NOT NULL DEFAULT 0`,
}}

// stepPostgres lets an external server be PostgreSQL (#81, api SPEC §6.5.2):
// whether a site's backup dumps its databases or the provider backs the
// server up (every server before is MariaDB and dumped).
var stepPostgres = schemaStep{version: 34, stmts: []string{
	`ALTER TABLE db_servers ADD COLUMN backup TEXT NOT NULL DEFAULT 'dump' CHECK (backup IN ('dump', 'provider'))`,
}}

// stepFunctions makes functions a server-wide resource (#143 v0.9.0,
// internal/functions SPEC §1, api SPEC §17): one row per function, its own
// host names, the functions domain and the v0.8.x migration's marker. A
// secret's value is the sealed fn.<id>.<NAME>, its token fn.<id>.token.
var stepFunctions = schemaStep{version: 35, stmts: []string{
	`CREATE TABLE functions (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		name         TEXT NOT NULL UNIQUE,
		runtime      TEXT NOT NULL,
		version      TEXT NOT NULL,
		memory_mb    INTEGER NOT NULL,
		timeout_sec  INTEGER NOT NULL,
		schedule     TEXT NOT NULL DEFAULT '',
		env          TEXT NOT NULL DEFAULT '{}',
		secrets      TEXT NOT NULL DEFAULT '[]',
		auth         TEXT NOT NULL DEFAULT 'public' CHECK (auth IN ('public', 'token')),
		cors_origins TEXT NOT NULL DEFAULT '[]',
		release      TEXT NOT NULL DEFAULT '',
		created_at   TEXT NOT NULL,
		updated_at   TEXT NOT NULL
	)`,
	`CREATE TABLE function_domains (
		function_id INTEGER NOT NULL REFERENCES functions(id) ON DELETE CASCADE,
		domain      TEXT NOT NULL UNIQUE
	)`,
	`CREATE INDEX idx_function_domains_function ON function_domains (function_id)`,
	`ALTER TABLE panel_settings ADD COLUMN functions_domain TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE panel_settings ADD COLUMN functions_migration TEXT NOT NULL DEFAULT ''`,
}}

// stepMail keeps the server-wide mail relay and the alert recipients
// (#77, api SPEC §14) as one JSON value; the relay's password is a secret.
var stepMail = schemaStep{version: 29, stmts: []string{
	`ALTER TABLE panel_settings ADD COLUMN mail TEXT NOT NULL DEFAULT '{}'`,
}}

// stepGitDeploy keeps a site's git deploy settings and the webhook
// deliveries it received (#24, api SPEC §15). The webhook secret and the
// deploy key are sealed secrets, not columns.
var stepGitDeploy = schemaStep{version: 31, stmts: []string{
	`CREATE TABLE site_git_deploys (
		site_id       INTEGER PRIMARY KEY REFERENCES sites(id) ON DELETE CASCADE,
		enabled       INTEGER NOT NULL DEFAULT 0,
		repo          TEXT NOT NULL DEFAULT '',
		branch        TEXT NOT NULL DEFAULT 'main',
		build_command TEXT NOT NULL DEFAULT '',
		public_key    TEXT NOT NULL DEFAULT '',
		hook_id       TEXT NOT NULL UNIQUE
	)`,
	`CREATE TABLE site_git_deliveries (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		site_id       INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
		at            TEXT NOT NULL,
		forge         TEXT NOT NULL,
		event         TEXT NOT NULL DEFAULT '',
		ref           TEXT NOT NULL DEFAULT '',
		commit_sha    TEXT NOT NULL DEFAULT '',
		outcome       TEXT NOT NULL,
		detail        TEXT NOT NULL DEFAULT '',
		deployment_id INTEGER REFERENCES deployments(id) ON DELETE SET NULL,
		task_id       INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX site_git_deliveries_site ON site_git_deliveries (site_id, id)`,
}}

// stepBlocklist keeps the server's block list (#21, blocklist.go): the
// addresses blocked by hand or for repeated failed sign-ins, the ones never
// blocked, and the automatic blocking's settings (one JSON row; none is the
// defaults).
var stepBlocklist = schemaStep{version: 27, stmts: []string{
	`CREATE TABLE blocklist (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		kind       TEXT NOT NULL CHECK (kind IN ('block', 'allow')),
		prefix     TEXT NOT NULL,
		source     TEXT NOT NULL,
		note       TEXT NOT NULL DEFAULT '',
		failures   INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL,
		expires_at TEXT,
		UNIQUE (kind, prefix)
	)`,
	`CREATE TABLE blocklist_settings (
		id       INTEGER PRIMARY KEY CHECK (id = 1),
		settings TEXT NOT NULL
	)`,
}}

// stepPasskeys adds panel passkeys and recovery codes (#156, api SPEC
// §2.10–2.11): whether an account's password signs in, its WebAuthn user
// handle, its passkeys and recovery codes, and the "every account" half of
// the two-factor requirement.
var stepPasskeys = schemaStep{version: 30, stmts: []string{
	`ALTER TABLE users ADD COLUMN password_signin INTEGER NOT NULL DEFAULT 1`,
	`ALTER TABLE users ADD COLUMN webauthn_id BLOB`,
	`CREATE UNIQUE INDEX idx_users_webauthn_id ON users (webauthn_id) WHERE webauthn_id IS NOT NULL`,
	`CREATE TABLE user_passkeys (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		credential_id   BLOB NOT NULL UNIQUE,
		public_key      BLOB NOT NULL,
		sign_count      INTEGER NOT NULL DEFAULT 0,
		aaguid          BLOB,
		transports      TEXT NOT NULL DEFAULT '',
		backup_eligible INTEGER NOT NULL DEFAULT 0,
		backup_state    INTEGER NOT NULL DEFAULT 0,
		rp_id           TEXT NOT NULL,
		name            TEXT NOT NULL,
		created_at      TEXT NOT NULL,
		last_used_at    TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX idx_user_passkeys_user ON user_passkeys (user_id)`,
	`CREATE TABLE user_recovery_codes (
		id        INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id   INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		code_hash TEXT NOT NULL,
		used_at   TEXT
	)`,
	`CREATE INDEX idx_user_recovery_codes_user ON user_recovery_codes (user_id, code_hash)`,
	`ALTER TABLE panel_settings ADD COLUMN require_2fa_all INTEGER NOT NULL DEFAULT 0`,
}}

// stepMCP adds the remote MCP server (#70, api SPEC §16): its switch, the
// OAuth clients that registered, the grants a person consented to (their
// tokens as hashes) and the destructive actions waiting for a person.
var stepMCP = schemaStep{version: 32, stmts: []string{
	`ALTER TABLE panel_settings ADD COLUMN mcp_enabled INTEGER NOT NULL DEFAULT 0`,
	`CREATE TABLE mcp_clients (
		id            TEXT PRIMARY KEY,
		name          TEXT NOT NULL,
		redirect_uris TEXT NOT NULL,
		created_at    TEXT NOT NULL,
		last_used_at  TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE TABLE mcp_grants (
		id                INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id           INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		client_id         TEXT NOT NULL REFERENCES mcp_clients(id) ON DELETE CASCADE,
		scopes            TEXT NOT NULL,
		access_hash       TEXT NOT NULL UNIQUE,
		access_expires    TEXT NOT NULL,
		refresh_hash      TEXT NOT NULL UNIQUE,
		refresh_expires   TEXT NOT NULL,
		prev_refresh_hash TEXT NOT NULL DEFAULT '',
		created_at        TEXT NOT NULL,
		last_used_at      TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX idx_mcp_grants_user ON mcp_grants (user_id)`,
	`CREATE INDEX idx_mcp_grants_prev ON mcp_grants (prev_refresh_hash)`,
	`CREATE TABLE mcp_actions (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		grant_id   INTEGER NOT NULL REFERENCES mcp_grants(id) ON DELETE CASCADE,
		user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		tool       TEXT NOT NULL,
		args       TEXT NOT NULL,
		summary    TEXT NOT NULL,
		status     TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'done', 'failed', 'rejected', 'expired')),
		result     TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		expires_at TEXT NOT NULL,
		decided_at TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX idx_mcp_actions_user ON mcp_actions (user_id)`,
}}
