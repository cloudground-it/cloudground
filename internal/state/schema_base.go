package state

// Version 1: the first schema. Accounts and their sign-in material, the
// legacy key/value settings, sites with their JSON configuration, the task
// list, releases, backups, the audit trail, and the record of the files
// the agent writes with the drift found in them.
var stepInitial = schemaStep{version: 1, stmts: []string{
	// Panel accounts. role is admin, operator or readonly.
	`CREATE TABLE users (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		email         TEXT NOT NULL UNIQUE,
		password_hash TEXT NOT NULL,
		role          TEXT NOT NULL DEFAULT 'admin',
		created_at    TEXT NOT NULL
	)`,
	// A browser session, stored by the SHA-256 of its cookie.
	`CREATE TABLE sessions (
		token_hash TEXT PRIMARY KEY,
		user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		expires_at TEXT NOT NULL,
		created_at TEXT NOT NULL
	)`,
	// The one-time token the installer prints for the first account.
	`CREATE TABLE setup_tokens (
		token_hash TEXT PRIMARY KEY,
		expires_at TEXT NOT NULL,
		used       INTEGER NOT NULL DEFAULT 0
	)`,
	// Everything else the panel kept, untyped; emptied by version 11.
	`CREATE TABLE settings (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`,
	// A managed site. staging_of names the production site of a staging
	// copy (0 for none); config is the SiteConfig as JSON.
	`CREATE TABLE sites (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		domain      TEXT NOT NULL UNIQUE,
		aliases     TEXT NOT NULL DEFAULT '[]',
		type        TEXT NOT NULL,
		profile     TEXT NOT NULL,
		engine      TEXT NOT NULL DEFAULT 'nginx',
		php_version TEXT NOT NULL DEFAULT '',
		system_user TEXT NOT NULL,
		root_path   TEXT NOT NULL,
		status      TEXT NOT NULL,
		staging_of  INTEGER NOT NULL DEFAULT 0,
		config      TEXT NOT NULL DEFAULT '{}',
		created_at  TEXT NOT NULL,
		updated_at  TEXT NOT NULL
	)`,
	`CREATE INDEX idx_sites_staging_of ON sites (staging_of)`,
	// Background work as the UI listed it; replaced by jobs in version 15.
	`CREATE TABLE tasks (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		kind        TEXT NOT NULL,
		site_id     INTEGER NOT NULL DEFAULT 0,
		status      TEXT NOT NULL,
		progress    INTEGER NOT NULL DEFAULT 0,
		message     TEXT NOT NULL DEFAULT '',
		log         TEXT NOT NULL DEFAULT '',
		error       TEXT NOT NULL DEFAULT '',
		created_at  TEXT NOT NULL,
		started_at  TEXT,
		finished_at TEXT
	)`,
	`CREATE INDEX idx_tasks_status ON tasks (status)`,
	`CREATE INDEX idx_tasks_site ON tasks (site_id)`,
	// One release of a site's code; a site names each release once.
	`CREATE TABLE deployments (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		site_id     INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
		release_id  TEXT NOT NULL,
		path        TEXT NOT NULL,
		checksum    TEXT NOT NULL DEFAULT '',
		status      TEXT NOT NULL,
		guard_json  TEXT NOT NULL DEFAULT '',
		created_at  TEXT NOT NULL,
		promoted_at TEXT,
		UNIQUE (site_id, release_id)
	)`,
	// One restic snapshot and the outcome of its restore test.
	`CREATE TABLE backups (
		id             INTEGER PRIMARY KEY AUTOINCREMENT,
		site_id        INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
		snapshot_id    TEXT NOT NULL,
		repository     TEXT NOT NULL,
		size_bytes     INTEGER NOT NULL DEFAULT 0,
		kind           TEXT NOT NULL,
		verify_status  TEXT NOT NULL DEFAULT 'pending',
		verified_at    TEXT,
		restore_est_ms INTEGER NOT NULL DEFAULT 0,
		created_at     TEXT NOT NULL
	)`,
	`CREATE INDEX idx_backups_site ON backups (site_id)`,
	// Who did what to which subject, append only.
	`CREATE TABLE audit_events (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		actor      TEXT NOT NULL,
		action     TEXT NOT NULL,
		subject    TEXT NOT NULL,
		detail     TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL
	)`,
	// A managed file found changed outside the panel. diff went in 10.
	`CREATE TABLE drift_events (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		path          TEXT NOT NULL,
		expected_hash TEXT NOT NULL,
		actual_hash   TEXT NOT NULL,
		diff          TEXT NOT NULL DEFAULT '',
		status        TEXT NOT NULL DEFAULT 'open',
		detected_at   TEXT NOT NULL,
		resolved_at   TEXT
	)`,
	// What the agent last wrote to each file it manages.
	`CREATE TABLE managed_files (
		path       TEXT PRIMARY KEY,
		sha256     TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`,
}}
