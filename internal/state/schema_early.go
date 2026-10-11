package state

// Versions 2 to 10: the additions made before state v1, one concern each.

// Version 2: a second factor for accounts, per-site cron jobs, the
// database console's sessions (dropped again by 10) and the scheduler's
// record of when each automatic job last ran.
var stepAuthCronSchedule = schemaStep{version: 2, stmts: []string{
	`ALTER TABLE users ADD COLUMN totp_secret TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE users ADD COLUMN totp_enabled INTEGER NOT NULL DEFAULT 0`,
	`CREATE TABLE cron_jobs (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		site_id     INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
		schedule    TEXT NOT NULL,
		command     TEXT NOT NULL,
		description TEXT NOT NULL DEFAULT '',
		enabled     INTEGER NOT NULL DEFAULT 1,
		last_run    TEXT,
		last_status TEXT NOT NULL DEFAULT '',
		created_at  TEXT NOT NULL
	)`,
	`CREATE INDEX idx_cron_site ON cron_jobs (site_id)`,
	`CREATE TABLE adminer_sessions (
		token_hash TEXT PRIMARY KEY,
		site_id    INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
		expires_at TEXT NOT NULL,
		created_at TEXT NOT NULL
	)`,
	`CREATE TABLE schedule_state (
		key      TEXT PRIMARY KEY,
		last_run TEXT NOT NULL
	)`,
}}

// Version 3: an operator reaches only the sites assigned to them.
var stepOperatorSites = schemaStep{version: 3, stmts: []string{
	`CREATE TABLE user_sites (
		user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		site_id INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
		PRIMARY KEY (user_id, site_id)
	)`,
	`CREATE INDEX idx_user_sites_site ON user_sites (site_id)`,
}}

// Version 4: a day of server headroom samples (288 at five minutes).
var stepMetricHistory = schemaStep{version: 4, stmts: []string{
	`CREATE TABLE metric_samples (
		id                INTEGER PRIMARY KEY AUTOINCREMENT,
		cpu_headroom_pct  INTEGER NOT NULL,
		mem_headroom_pct  INTEGER NOT NULL,
		disk_headroom_pct INTEGER NOT NULL,
		load1             REAL NOT NULL,
		sampled_at        TEXT NOT NULL
	)`,
	`CREATE INDEX idx_metric_samples_time ON metric_samples (sampled_at)`,
}}

// Version 5: the newest TOTP time-step an account has used. A verifier
// must refuse a code it already accepted (RFC 6238 §5.2); without this a
// code seen once stays good for the whole acceptance window.
var stepTOTPReplay = schemaStep{version: 5, stmts: []string{
	`ALTER TABLE users ADD COLUMN totp_last_step INTEGER NOT NULL DEFAULT 0`,
}}

// Version 6: a secret being enrolled waits beside the active one, so that
// starting an enrolment never turns off a working second factor; sessions
// are found by account (to list and revoke them).
var stepTOTPEnrolment = schemaStep{version: 6, stmts: []string{
	`ALTER TABLE users ADD COLUMN totp_pending TEXT NOT NULL DEFAULT ''`,
	`CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions (user_id)`,
}}

// Version 7: when a session was last used, so an idle one ends early.
var stepSessionIdle = schemaStep{version: 7, stmts: []string{
	`ALTER TABLE sessions ADD COLUMN last_seen_at TEXT NOT NULL DEFAULT ''`,
}}

// Version 8: the browser and address a session signed in from, for its
// owner to recognise it.
var stepSessionClient = schemaStep{version: 8, stmts: []string{
	`ALTER TABLE sessions ADD COLUMN user_agent TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE sessions ADD COLUMN ip TEXT NOT NULL DEFAULT ''`,
}}

// Version 9: accounts get a name and can be disabled instead of deleted.
var stepUserProfile = schemaStep{version: 9, stmts: []string{
	`ALTER TABLE users ADD COLUMN first_name TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE users ADD COLUMN last_name TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE users ADD COLUMN disabled INTEGER NOT NULL DEFAULT 0`,
}}

// Version 10: two things that were written and never read go. Nothing
// looked up a console session by its row (the agent checks the token),
// and no code ever filled a drift event's diff.
var stepDropWriteOnly = schemaStep{version: 10, stmts: []string{
	`DROP TABLE IF EXISTS adminer_sessions`,
	`ALTER TABLE drift_events DROP COLUMN diff`,
}}
