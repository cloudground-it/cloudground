package state

// Versions 12 to 15: Phase 4's first additions.

// Version 12: the interface language an account chose (it, en, fr, es,
// de); empty until they choose, and the panel follows the browser.
var stepUserLocale = schemaStep{version: 12, stmts: []string{
	`ALTER TABLE users ADD COLUMN locale TEXT NOT NULL DEFAULT ''`,
}}

// Version 13: API tokens for cgctl and scripts (R-4.6). A token is kept as
// its SHA-256 only, with a hint to tell it apart; revoking deletes it.
var stepAPITokens = schemaStep{version: 13, stmts: []string{
	`CREATE TABLE api_tokens (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		name         TEXT NOT NULL,
		token_hash   TEXT NOT NULL UNIQUE,
		hint         TEXT NOT NULL,
		created_at   TEXT NOT NULL,
		expires_at   TEXT NOT NULL DEFAULT '',
		last_used_at TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX idx_api_tokens_user ON api_tokens (user_id)`,
}}

// Version 14: a task's failure as a translation key and its values (JSON)
// beside the English text, so each reader gets it in their language (R-4.4).
var stepTaskErrorKeys = schemaStep{version: 14, stmts: []string{
	`ALTER TABLE tasks ADD COLUMN error_key TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE tasks ADD COLUMN error_args TEXT NOT NULL DEFAULT ''`,
}}

// Version 15: the persistent job queue (R-4.2, internal/jobs/SPEC.md)
// takes over from tasks, keeping their IDs. A task still pending or running
// belonged to a process that no longer exists: it ends failed, with a
// reason when it had none, finished when it was created if it never
// finished. The message keeps its first 2 KiB. A log within the job log's
// 64 KiB bound is kept whole as the head; a longer one keeps its first
// 16 KiB and its last 48 KiB and counts the bytes between. Lengths are in
// bytes, so a cut can split a character (the log is for reading).
var stepJobQueue = schemaStep{version: 15, stmts: []string{
	`CREATE TABLE jobs (
		id               INTEGER PRIMARY KEY AUTOINCREMENT,
		kind             TEXT NOT NULL,
		site_id          INTEGER NOT NULL DEFAULT 0,
		status           TEXT NOT NULL,
		progress         INTEGER NOT NULL DEFAULT 0,
		message          TEXT NOT NULL DEFAULT '',
		log_head         TEXT NOT NULL DEFAULT '',
		log_tail         TEXT NOT NULL DEFAULT '',
		log_dropped      INTEGER NOT NULL DEFAULT 0,
		error            TEXT NOT NULL DEFAULT '',
		error_key        TEXT NOT NULL DEFAULT '',
		error_args       TEXT NOT NULL DEFAULT '',
		payload          TEXT NOT NULL DEFAULT '{}',
		locks            TEXT NOT NULL DEFAULT '[]',
		dedup_key        TEXT,
		quiet            INTEGER NOT NULL DEFAULT 0,
		attempts         INTEGER NOT NULL DEFAULT 0,
		cancel_requested INTEGER NOT NULL DEFAULT 0,
		background       INTEGER NOT NULL DEFAULT 0,
		not_before       TEXT,
		created_at       TEXT NOT NULL,
		started_at       TEXT,
		finished_at      TEXT
	)`,
	// At most one live job per deduplication key.
	`CREATE UNIQUE INDEX idx_jobs_dedup ON jobs (dedup_key)
		WHERE dedup_key IS NOT NULL AND status IN ('queued', 'running')`,
	`CREATE INDEX idx_jobs_status ON jobs (status)`,
	`CREATE INDEX idx_jobs_site ON jobs (site_id)`,
	`CREATE INDEX idx_jobs_listed ON jobs (quiet, id)`,
	`INSERT INTO jobs (id, kind, site_id, status, progress, message, log_head, log_tail, log_dropped,
		error, error_key, error_args, attempts, created_at, started_at, finished_at)
	SELECT id, kind, site_id,
		CASE WHEN orphan THEN 'failed' ELSE status END,
		progress,
		CAST(substr(raw_message, 1, 2048) AS TEXT),
		CASE WHEN log_bytes > 65536 THEN CAST(substr(raw_log, 1, 16384) AS TEXT) ELSE log END,
		CASE WHEN log_bytes > 65536 THEN CAST(substr(raw_log, -49152) AS TEXT) ELSE '' END,
		max(log_bytes - 65536, 0),
		CASE WHEN orphan AND error = '' THEN 'interrupted by panel restart' ELSE error END,
		error_key, error_args,
		started_at IS NOT NULL,
		created_at, started_at,
		CASE WHEN orphan THEN IFNULL(finished_at, created_at) ELSE finished_at END
	FROM (
		SELECT *, status IN ('pending', 'running') AS orphan,
			CAST(message AS BLOB) AS raw_message,
			CAST(log AS BLOB) AS raw_log,
			length(CAST(log AS BLOB)) AS log_bytes
		FROM tasks
	)`,
	`DROP TABLE tasks`,
}}
