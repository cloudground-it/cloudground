package state

// Version 11: state v1 (R-2.1, SPEC.md). The settings table mixed panel
// configuration, secrets, per-site observations and bookkeeping under
// untyped keys. This step creates their typed homes and moves everything
// that needs no key: the panel settings, the certificate state of sites
// that still exist, the alert dismissals and the rendered version. It then
// deletes what it moved, and with it the certificate rows of sites deleted
// long ago. Secrets stay for Open to seal (moveLegacySecrets).
//
// Prefixes are compared after lower-casing, as the LIKE patterns this step
// was first written with did.
var stepStateV1 = schemaStep{version: 11, stmts: []string{
	`CREATE TABLE secrets (
		name       TEXT PRIMARY KEY,
		site_id    INTEGER REFERENCES sites(id) ON DELETE CASCADE,
		sealed     BLOB NOT NULL,
		updated_at TEXT NOT NULL
	)`,
	`CREATE INDEX idx_secrets_site ON secrets (site_id)`,
	`CREATE TABLE meta (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`,
	`CREATE TABLE panel_settings (
		id                   INTEGER PRIMARY KEY CHECK (id = 1),
		acme_email           TEXT NOT NULL DEFAULT '',
		panel_domain         TEXT NOT NULL DEFAULT '',
		probe_target         TEXT NOT NULL DEFAULT '',
		backup_repository    TEXT NOT NULL DEFAULT '',
		backup_s3_access_key TEXT NOT NULL DEFAULT '',
		backup_s3_region     TEXT NOT NULL DEFAULT '',
		require_2fa_admin    INTEGER NOT NULL DEFAULT 0,
		update_check         INTEGER NOT NULL DEFAULT 1
	)`,
	// One row whatever settings holds: an aggregate over no rows still
	// answers once, and every missing key falls back to its default.
	// require_2fa_admin is on only for "1" or "true"; update_check is off
	// only for "0" or "false".
	`INSERT INTO panel_settings (id, acme_email, panel_domain, probe_target, backup_repository,
		backup_s3_access_key, backup_s3_region, require_2fa_admin, update_check)
	SELECT 1,
		IFNULL(MAX(CASE key WHEN 'acme_email' THEN value END), ''),
		IFNULL(MAX(CASE key WHEN 'panel_domain' THEN value END), ''),
		IFNULL(MAX(CASE key WHEN 'probe_target' THEN value END), ''),
		IFNULL(MAX(CASE key WHEN 'backup_repository' THEN value END), ''),
		IFNULL(MAX(CASE key WHEN 'backup_s3_access_key' THEN value END), ''),
		IFNULL(MAX(CASE key WHEN 'backup_s3_region' THEN value END), ''),
		IFNULL(MAX(key = 'require_2fa_admin' AND value IN ('1', 'true')), 0),
		1 - IFNULL(MAX(key = 'update_check' AND value IN ('0', 'false')), 0)
	FROM settings`,
	`CREATE TABLE site_status (
		site_id     INTEGER PRIMARY KEY REFERENCES sites(id) ON DELETE CASCADE,
		cert_expiry TEXT NOT NULL DEFAULT '',
		cert_error  TEXT NOT NULL DEFAULT ''
	)`,
	// A site gets a row when either of its two keys holds something.
	`INSERT INTO site_status (site_id, cert_expiry, cert_error)
	SELECT site_id,
		IFNULL(MAX(CASE WHEN field = 'expiry' THEN value END), ''),
		IFNULL(MAX(CASE WHEN field = 'error' THEN value END), '')
	FROM (
		SELECT sites.id AS site_id, settings.value AS value,
			CASE settings.key WHEN 'cert_expiry.' || sites.id THEN 'expiry' ELSE 'error' END AS field
		FROM sites JOIN settings ON settings.key IN ('cert_expiry.' || sites.id, 'cert_error.' || sites.id)
	)
	GROUP BY site_id
	HAVING MAX(value <> '') = 1`,
	`CREATE TABLE alert_dismissals (
		alert_id     TEXT PRIMARY KEY,
		site_id      INTEGER REFERENCES sites(id) ON DELETE CASCADE,
		dismissed_at TEXT NOT NULL
	)`,
	`CREATE INDEX idx_alert_dismissals_site ON alert_dismissals (site_id)`,
	`INSERT INTO alert_dismissals (alert_id, site_id, dismissed_at)
	SELECT substr(key, 17), NULL, value FROM settings
	WHERE lower(substr(key, 1, 16)) = 'alert_dismissed.' AND value <> ''`,
	`INSERT INTO meta (key, value)
	SELECT key, value FROM settings WHERE key = 'rendered_version'`,
	`DELETE FROM settings
	WHERE key IN ('acme_email', 'panel_domain', 'probe_target', 'backup_repository', 'backup_s3_access_key',
			'backup_s3_region', 'require_2fa_admin', 'update_check', 'rendered_version')
		OR lower(substr(key, 1, 12)) = 'cert_expiry.'
		OR lower(substr(key, 1, 11)) = 'cert_error.'
		OR lower(substr(key, 1, 16)) = 'alert_dismissed.'`,
	// Desired and applied generations (SPEC.md invariant 5): a site that
	// was active is taken as rendered at its first generation.
	`ALTER TABLE sites ADD COLUMN desired_generation INTEGER NOT NULL DEFAULT 1`,
	`ALTER TABLE sites ADD COLUMN applied_generation INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE sites ADD COLUMN last_apply_error TEXT NOT NULL DEFAULT ''`,
	`UPDATE sites SET applied_generation = 1 WHERE status = 'active'`,
}}
