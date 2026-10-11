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
