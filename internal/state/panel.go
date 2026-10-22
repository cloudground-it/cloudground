package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/api/events"
)

// DefaultProbeTarget is where Performance Guard sends its requests when no
// probe target is configured.
const DefaultProbeTarget = "https://127.0.0.1"

// PanelSettings is the panel's own configuration. Secrets are not here: they
// are in the secret store (Secret, SetSecret).
type PanelSettings struct {
	ACMEEmail string
	// PanelDomain is the domain the panel holds a certificate for. It is set
	// by the operation that issues the certificate, not typed by anyone.
	PanelDomain string
	// ProbeTarget is the origin Performance Guard probes; "" means
	// DefaultProbeTarget.
	ProbeTarget       string
	BackupRepository  string
	BackupS3AccessKey string
	BackupS3Region    string
	Require2FAAdmin   bool
	UpdateCheck       bool
	// UpdateChannel is the release channel the panel follows: "stable" or
	// "beta" (internal/version, SPEC §4.1).
	UpdateChannel string
	// VulnerabilityCheck turns on the daily lookup of the WordPress sites'
	// core, plugin and theme versions in a public database (api SPEC §11.6).
	VulnerabilityCheck bool
	// MalwareScan turns on the scheduled malware scan of every site (api
	// SPEC §11.7); off by default, since it installs ClamAV.
	MalwareScan bool
	// Mail is the server-wide mail relay (api SPEC §14.1; its password is
	// the secret SecretMailPassword) and AlertEmails who gets the alerts
	// by email (§14.4), addresses joined by ", " so the settings stay
	// comparable. Both are the JSON column mail.
	Mail        MailRelay
	AlertEmails string
}

// AlertEmailList is AlertEmails one address each.
func (p PanelSettings) AlertEmailList() []string { return splitEmails(p.AlertEmails) }

// splitEmails reads addresses separated by commas or spaces.
func splitEmails(v string) []string {
	return strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' })
}

// panelMail is the JSON of the mail column.
type panelMail struct {
	Relay       MailRelay `json:"relay,omitzero"`
	AlertEmails []string  `json:"alert_emails,omitempty"`
}

// EffectiveProbeTarget is ProbeTarget, or the default when unset.
func (p PanelSettings) EffectiveProbeTarget() string {
	if p.ProbeTarget == "" {
		return DefaultProbeTarget
	}
	return p.ProbeTarget
}

const panelCols = `acme_email, panel_domain, probe_target, backup_repository, backup_s3_access_key, backup_s3_region, require_2fa_admin, update_check, update_channel, vulnerability_check, malware_scan, mail`

func scanPanel(row interface{ Scan(...any) error }) (PanelSettings, error) {
	var p PanelSettings
	var mail string
	err := row.Scan(&p.ACMEEmail, &p.PanelDomain, &p.ProbeTarget, &p.BackupRepository,
		&p.BackupS3AccessKey, &p.BackupS3Region, &p.Require2FAAdmin, &p.UpdateCheck, &p.UpdateChannel, &p.VulnerabilityCheck, &p.MalwareScan, &mail)
	if err != nil {
		return p, err
	}
	var m panelMail
	if err := json.Unmarshal([]byte(mail), &m); err != nil {
		return p, fmt.Errorf("panel settings: the mail column: %w", err)
	}
	p.Mail, p.AlertEmails = m.Relay, strings.Join(m.AlertEmails, ", ")
	return p, nil
}

// PanelSettings returns the panel's configuration.
func (s *Store) PanelSettings() (PanelSettings, error) {
	p, err := scanPanel(s.db.QueryRow(`SELECT ` + panelCols + ` FROM panel_settings WHERE id=1`))
	if err != nil {
		return p, err
	}
	p.BackupRepository, err = s.openRepo(p.BackupRepository)
	return p, err
}

// UpdatePanelSettings applies fn to the settings as stored now and saves the
// result in one write transaction. A non-nil error from fn aborts the change
// and is returned as is. fn must not call back into the Store.
func (s *Store) UpdatePanelSettings(fn func(*PanelSettings) error) (PanelSettings, error) {
	var out PanelSettings
	err := s.writeTx(func(conn *sql.Conn, ctx context.Context) error {
		q := connExec{conn: conn, ctx: ctx}
		p, err := scanPanel(conn.QueryRowContext(ctx, `SELECT `+panelCols+` FROM panel_settings WHERE id=1`))
		if err == nil {
			p.BackupRepository, err = s.openRepoFrom(q, p.BackupRepository)
		}
		if err != nil {
			return err
		}
		if err := fn(&p); err != nil {
			return err
		}
		repo, err := s.storedRepo(q, p.BackupRepository)
		if err != nil {
			return err
		}
		mail, err := json.Marshal(panelMail{Relay: p.Mail, AlertEmails: splitEmails(p.AlertEmails)})
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `UPDATE panel_settings SET acme_email=?, panel_domain=?, probe_target=?,
			backup_repository=?, backup_s3_access_key=?, backup_s3_region=?, require_2fa_admin=?, update_check=?, update_channel=?, vulnerability_check=?, malware_scan=?, mail=? WHERE id=1`,
			p.ACMEEmail, p.PanelDomain, p.ProbeTarget, repo, p.BackupS3AccessKey, p.BackupS3Region,
			p.Require2FAAdmin, p.UpdateCheck, p.UpdateChannel, p.VulnerabilityCheck, p.MalwareScan, string(mail)); err != nil {
			return fmt.Errorf("update panel settings: %w", err)
		}
		out = p
		return nil
	})
	if err == nil {
		s.publish(events.SettingsChanged, 0, nil)
	}
	return out, err
}

// writeTx runs fn inside BEGIN IMMEDIATE on a dedicated connection, so the
// row fn reads is the row it writes: another writer cannot slip in between.
func (s *Store) writeTx(fn func(conn *sql.Conn, ctx context.Context) error) error {
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, `ROLLBACK`)
		}
	}()
	if err := fn(conn, ctx); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}

// CertState is what the panel last observed about a site's certificate.
type CertState struct {
	// Expiry is the live certificate's notAfter; zero when unknown.
	Expiry time.Time
	// Error is the last renewal failure; "" after a success.
	Error string
}

// SetCertExpiry records the live certificate's expiry for a site.
func (s *Store) SetCertExpiry(siteID int64, notAfter time.Time) error {
	res, err := s.db.Exec(`INSERT INTO site_status (site_id, cert_expiry) VALUES (?, ?)
		ON CONFLICT(site_id) DO UPDATE SET cert_expiry=excluded.cert_expiry WHERE cert_expiry IS NOT excluded.cert_expiry`,
		siteID, notAfter.UTC().Format(time.RFC3339))
	s.publishCert(siteID, res, err)
	return err
}

// publishCert announces a certificate state write that changed something
// (the scheduler records every expiry again twice a day).
func (s *Store) publishCert(siteID int64, res sql.Result, err error) {
	if err != nil {
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		s.publish(events.CertChanged, siteID, map[string]any{})
	}
}

// SetCertError records the last certificate renewal error of a site; ""
// clears it.
func (s *Store) SetCertError(siteID int64, msg string) error {
	res, err := s.db.Exec(`INSERT INTO site_status (site_id, cert_error) VALUES (?, ?)
		ON CONFLICT(site_id) DO UPDATE SET cert_error=excluded.cert_error WHERE cert_error IS NOT excluded.cert_error`, siteID, msg)
	s.publishCert(siteID, res, err)
	return err
}

// CertStates returns the observed certificate state of every site that has
// one.
func (s *Store) CertStates() (map[int64]CertState, error) {
	rows, err := s.db.Query(`SELECT site_id, cert_expiry, cert_error FROM site_status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]CertState{}
	for rows.Next() {
		var id int64
		var exp, msg string
		if err := rows.Scan(&id, &exp, &msg); err != nil {
			return nil, err
		}
		st := CertState{Error: msg}
		if exp != "" {
			st.Expiry, _ = time.Parse(time.RFC3339, exp)
		}
		out[id] = st
	}
	return out, rows.Err()
}

// CertStateOf returns one site's observed certificate state.
func (s *Store) CertStateOf(siteID int64) (CertState, error) {
	var exp, msg string
	err := s.db.QueryRow(`SELECT cert_expiry, cert_error FROM site_status WHERE site_id=?`, siteID).Scan(&exp, &msg)
	if errors.Is(err, sql.ErrNoRows) {
		return CertState{}, nil
	}
	if err != nil {
		return CertState{}, err
	}
	st := CertState{Error: msg}
	if exp != "" {
		st.Expiry, _ = time.Parse(time.RFC3339, exp)
	}
	return st, nil
}

const metaRenderedVersion = "rendered_version"

// RenderedVersion is what the last complete re-render of every site was
// done for ("<version>/<active sites>"), or "" if none was.
func (s *Store) RenderedVersion() (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key=?`, metaRenderedVersion).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetRenderedVersion records a complete re-render of every site.
func (s *Store) SetRenderedVersion(v string) error {
	_, err := s.db.Exec(`INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		metaRenderedVersion, v)
	return err
}
