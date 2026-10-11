package state

// Versions 16 to 19: Phase 4's second wave (R-4.3).

// Version 16: TOTP secrets leave users for the sealed secret store
// (SPEC.md). Sealing needs the key and steps have none, so the plaintext
// waits in totp_import, one row per account and slot; Open with the key
// seals the rows, compacts the file and drops the table (moveLegacyTOTP).
var stepTOTPToSecrets = schemaStep{version: 16, stmts: []string{
	`CREATE TABLE totp_import (
		user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		slot    TEXT NOT NULL CHECK (slot IN ('active', 'pending')),
		secret  TEXT NOT NULL,
		PRIMARY KEY (user_id, slot)
	)`,
	`INSERT INTO totp_import (user_id, slot, secret)
		SELECT id, 'active', totp_secret FROM users WHERE totp_secret <> ''
		UNION ALL
		SELECT id, 'pending', totp_pending FROM users WHERE totp_pending <> ''`,
	`ALTER TABLE users DROP COLUMN totp_secret`,
	`ALTER TABLE users DROP COLUMN totp_pending`,
}}

// Version 17 changes nothing. The number was set aside for the sites
// stream, which needed no schema change; the version is the position in
// the list, so the empty step keeps 18 and 19 where they were released.
var stepReservedSites = schemaStep{version: 17}

// Version 18: deployments become a state machine (SPEC.md "Deployments"),
// with at most one promoted (live) row per site, which a unique partial
// index now enforces. The old panel left earlier releases promoted after a
// new promote: of a site's promoted rows the newest (by promotion time,
// else creation time, then id) stays and the rest become superseded. A
// safe push left guarding had built its release and never finished: it is
// created when it has a path and failed when it has none. New columns:
// rollback_to, the release that was live when this one was promoted;
// regressed, set when a safe push rolled it back for a regression (never a
// rollback target); note, what a person should know (it raises an alert).
var stepDeploymentMachine = schemaStep{version: 18, stmts: []string{
	`UPDATE deployments SET status = 'superseded'
	WHERE status = 'promoted' AND EXISTS (
		SELECT 1 FROM deployments AS newer
		WHERE newer.site_id = deployments.site_id AND newer.status = 'promoted'
			AND (IFNULL(newer.promoted_at, newer.created_at), newer.id)
				> (IFNULL(deployments.promoted_at, deployments.created_at), deployments.id)
	)`,
	`UPDATE deployments SET status = iif(path = '', 'failed', 'created') WHERE status = 'guarding'`,
	`CREATE UNIQUE INDEX idx_deployments_live ON deployments (site_id) WHERE status = 'promoted'`,
	`ALTER TABLE deployments ADD COLUMN rollback_to TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE deployments ADD COLUMN regressed INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE deployments ADD COLUMN note TEXT NOT NULL DEFAULT ''`,
}}

// Version 19: alerts are rows (SPEC.md invariant 6), replacing computed
// sets and the table of dismissed hashes, whose rows do not carry over;
// and a connector token's SHA-256 sits beside its sealed value, so a purge
// finds its site with one indexed read (invariant 7).
var stepAlertsTable = schemaStep{version: 19, stmts: []string{
	`CREATE TABLE alerts (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		key          TEXT NOT NULL UNIQUE,
		public_id    TEXT NOT NULL UNIQUE,
		kind         TEXT NOT NULL,
		site_id      INTEGER REFERENCES sites(id) ON DELETE CASCADE,
		subject      TEXT NOT NULL DEFAULT '',
		level        TEXT NOT NULL,
		fingerprint  TEXT NOT NULL DEFAULT '',
		message      TEXT NOT NULL,
		text_key     TEXT NOT NULL DEFAULT '',
		text_count   INTEGER NOT NULL DEFAULT -1,
		text_args    TEXT NOT NULL DEFAULT '[]',
		detail_key   TEXT NOT NULL DEFAULT '',
		detail_args  TEXT NOT NULL DEFAULT '[]',
		at           TEXT NOT NULL DEFAULT '',
		raised_at    TEXT NOT NULL,
		seen_at      TEXT NOT NULL,
		dismissed_at TEXT,
		notified_at  TEXT
	)`,
	`CREATE INDEX idx_alerts_kind ON alerts (kind)`,
	`CREATE INDEX idx_alerts_site ON alerts (site_id)`,
	`DROP TABLE alert_dismissals`,
	// A panel being upgraded (it has accounts) would announce again what
	// it already announced: until the first evaluation ends, new rows are
	// born notified.
	`INSERT INTO meta (key, value) SELECT 'alerts_upgraded', '1' FROM users LIMIT 1`,
	`ALTER TABLE secrets ADD COLUMN lookup TEXT`,
	`CREATE UNIQUE INDEX idx_secrets_lookup ON secrets (lookup) WHERE lookup IS NOT NULL`,
}}
