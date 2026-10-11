package rpc

import "github.com/cloudground-it/cloudground/internal/state"

// StagingParams names a production site and its staging copy. CreateStaging
// clones the first into the second, with the staging copy's own database
// password and connector token; StagingDiff and StagingTables only read.
type StagingParams struct {
	Production     state.Site `json:"production"`
	Staging        state.Site `json:"staging"`
	DBPassword     string     `json:"db_password"`
	ConnectorToken string     `json:"connector_token"`
}

// Sites lists production, then staging.
func (p *StagingParams) Sites() []*state.Site { return []*state.Site{&p.Production, &p.Staging} }

// StagingDiffResult is how staging's live release differs from
// production's, as paths relative to the release root.
type StagingDiffResult struct {
	Added    []string `json:"added"`
	Removed  []string `json:"removed"`
	Modified []string `json:"modified"`
	// Scanned counts the files compared; Truncated says the comparison
	// stopped at its limit.
	Scanned   int  `json:"scanned"`
	Truncated bool `json:"truncated,omitempty"`
}

// StagingTablesResult lists the staging database's tables, each with the
// push policy's verdict.
type StagingTablesResult struct {
	Database string         `json:"database"`
	Tables   []StagingTable `json:"tables"`
}

// StagingTable is one table of the staging database. A Protected table is
// never pushed over production, for the Reason given.
type StagingTable struct {
	Name      string `json:"name"`
	Protected bool   `json:"protected"`
	Reason    string `json:"reason,omitempty"`
}

// SyncStagingDBParams pushes the named tables of staging's database over
// production's.
type SyncStagingDBParams struct {
	Production state.Site `json:"production"`
	Staging    state.Site `json:"staging"`
	Tables     []string   `json:"tables"`
}

// Sites lists production, then staging.
func (p *SyncStagingDBParams) Sites() []*state.Site {
	return []*state.Site{&p.Production, &p.Staging}
}
