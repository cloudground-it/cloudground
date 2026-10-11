package state

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Vulnerability checks (#31, api SPEC §11.6): each WordPress site's last
// check of its core, plugin and theme versions against a public database.
// One row per site: a check replaces what the previous one found, and a
// check that failed keeps it, with its error beside it.

// VulnFinding is one known vulnerability of something a site has installed.
type VulnFinding struct {
	// ID is the database's identifier of the vulnerability.
	ID string `json:"id"`
	// Component is core, plugin or theme; Slug its directory name (empty
	// for core) and Version the version installed.
	Component string `json:"component"`
	Slug      string `json:"slug,omitempty"`
	Version   string `json:"version"`
	Title     string `json:"title"`
	// Severity is critical, high, medium, low or "" when the database
	// gives no score.
	Severity string `json:"severity,omitempty"`
	CVE      string `json:"cve,omitempty"`
	// FixedIn is the first version without it, when the database names
	// one; Unfixed says no version fixes it yet.
	FixedIn string `json:"fixed_in,omitempty"`
	Unfixed bool   `json:"unfixed,omitempty"`
	Link    string `json:"link,omitempty"`
}

// VulnReport is a site's last vulnerability check.
type VulnReport struct {
	SiteID int64 `json:"-"`
	// CheckedAt is when the last complete check ended (zero: none yet);
	// AttemptedAt when the last one ended, complete or not, and Error why
	// that one failed ("" when it did not).
	CheckedAt   time.Time `json:"checked_at"`
	AttemptedAt time.Time `json:"attempted_at"`
	Error       string    `json:"error,omitempty"`
	// Core is the WordPress version checked; Components how many things
	// (core, plugins, themes) were looked up.
	Core       string        `json:"core,omitempty"`
	Components int           `json:"components"`
	Findings   []VulnFinding `json:"findings"`
}

// vulnBody is what the report column holds.
type vulnBody struct {
	Core       string        `json:"core,omitempty"`
	Components int           `json:"components"`
	Findings   []VulnFinding `json:"findings"`
}

// RecordVulnCheck stores a complete check of a site, replacing the previous
// one. A site that no longer exists is skipped.
func (s *Store) RecordVulnCheck(r VulnReport) error {
	body, err := json.Marshal(vulnBody{Core: r.Core, Components: r.Components, Findings: r.Findings})
	if err != nil {
		return err
	}
	at := now()
	_, err = s.db.Exec(`INSERT INTO site_vulnerabilities (site_id, checked_at, attempted_at, error, report)
		SELECT ?, ?, ?, '', ? WHERE EXISTS (SELECT 1 FROM sites WHERE id = ?)
		ON CONFLICT (site_id) DO UPDATE SET checked_at = excluded.checked_at, attempted_at = excluded.attempted_at,
			error = '', report = excluded.report`, r.SiteID, at, at, string(body), r.SiteID)
	return err
}

// RecordVulnCheckError notes a check of a site that could not complete; what
// the last complete one found stays.
func (s *Store) RecordVulnCheckError(siteID int64, why string) error {
	_, err := s.db.Exec(`INSERT INTO site_vulnerabilities (site_id, attempted_at, error)
		SELECT ?, ?, ? WHERE EXISTS (SELECT 1 FROM sites WHERE id = ?)
		ON CONFLICT (site_id) DO UPDATE SET attempted_at = excluded.attempted_at, error = excluded.error`,
		siteID, now(), why, siteID)
	return err
}

func scanVuln(sc interface{ Scan(...any) error }) (VulnReport, error) {
	var r VulnReport
	var checked, attempted, body string
	if err := sc.Scan(&r.SiteID, &checked, &attempted, &r.Error, &body); err != nil {
		return r, err
	}
	r.CheckedAt, r.AttemptedAt = parseTime(checked), parseTime(attempted)
	var b vulnBody
	if err := json.Unmarshal([]byte(body), &b); err != nil {
		return r, err
	}
	r.Core, r.Components, r.Findings = b.Core, b.Components, b.Findings
	if r.Findings == nil {
		r.Findings = []VulnFinding{}
	}
	return r, nil
}

const vulnCols = `site_id, checked_at, attempted_at, error, report`

// SiteVulnReport is a site's last check; ok is false when it has none.
func (s *Store) SiteVulnReport(siteID int64) (r VulnReport, ok bool, err error) {
	r, err = scanVuln(s.db.QueryRow(`SELECT `+vulnCols+` FROM site_vulnerabilities WHERE site_id = ?`, siteID))
	if errors.Is(err, sql.ErrNoRows) {
		return VulnReport{SiteID: siteID, Findings: []VulnFinding{}}, false, nil
	}
	return r, err == nil, err
}

// VulnReports is every site's last check, by site.
func (s *Store) VulnReports() (map[int64]VulnReport, error) {
	rows, err := s.db.Query(`SELECT ` + vulnCols + ` FROM site_vulnerabilities`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]VulnReport{}
	for rows.Next() {
		r, err := scanVuln(rows)
		if err != nil {
			return nil, err
		}
		out[r.SiteID] = r
	}
	return out, rows.Err()
}
