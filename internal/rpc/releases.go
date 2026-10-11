package rpc

import (
	"fmt"

	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// DeployParams builds an immutable release of a site from a directory the
// API prepared (an extracted upload, a git checkout, a staging tree).
type DeployParams struct {
	Site      state.Site `json:"site"`
	SourceDir string     `json:"source_dir"`
	// ReleaseID is the new release's name, chosen by the API
	// (site.NewReleaseID).
	ReleaseID string `json:"release_id"`
	// SourceSite, on a safe push, is the staging site SourceDir lies in. A
	// source outside Site's own tree is accepted only inside this site, and
	// only when it is a staging copy of Site.
	SourceSite *state.Site `json:"source_site,omitempty"`
	// Overlay, with SourceSite, makes a selective push: the release is
	// SourceDir (the site's own live release) with only these paths,
	// relative to the release root, taken from SourceSite's live release.
	Overlay []string `json:"overlay,omitempty"`
}

// Sites lists the site deployed to, then the staging site the source is
// taken from (nil when there is none).
func (p *DeployParams) Sites() []*state.Site { return []*state.Site{&p.Site, p.SourceSite} }

// Validate requires a well-formed release ID.
func (p *DeployParams) Validate() error {
	if !site.ValidReleaseID(p.ReleaseID) {
		return fmt.Errorf("invalid release id %q", p.ReleaseID)
	}
	return nil
}

// DeployResult is the release built.
type DeployResult struct {
	ReleaseID string `json:"release_id"`
	Path      string `json:"path"`
	Checksum  string `json:"checksum"`
	// Managed lists the panel's files the deploy wrote again (the
	// connector), so the API refreshes their recorded hashes instead of
	// reporting them as drift.
	Managed []ManagedFile `json:"managed,omitempty"`
}

// ReleaseParams promotes a release, or rolls back: to ReleaseID, or with
// an empty one to the newest release older than the live one.
type ReleaseParams struct {
	Site      state.Site `json:"site"`
	ReleaseID string     `json:"release_id"`
}

// Sites lists the site whose live release changes.
func (p *ReleaseParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// Validate accepts no release ID (a rollback to the previous release), the
// provisioning release, or a well-formed release ID.
func (p *ReleaseParams) Validate() error {
	if p.ReleaseID == "" || p.ReleaseID == site.InitialRelease || site.ValidReleaseID(p.ReleaseID) {
		return nil
	}
	return fmt.Errorf("invalid release id %q", p.ReleaseID)
}

// MigrationParams imports a site from another host: Archive (the files)
// and SQL (the database dump), both already uploaded inside the site's
// tree, become release ReleaseID. OldDomain, when set, is rewritten to the
// site's domain in the database.
type MigrationParams struct {
	Site    state.Site `json:"site"`
	Archive string     `json:"archive"`
	SQL     string     `json:"sql,omitempty"`
	// Database is the site's database SQL goes into; "" is its primary.
	// A WordPress site's dump goes into its primary only (its URLs are
	// rewritten there).
	Database  string `json:"database,omitempty"`
	OldDomain string `json:"old_domain,omitempty"`
	ReleaseID string `json:"release_id"`
	// MaxRatio raises (or lowers) the archive's expansion limit, in times
	// its compressed size; 0 keeps the agent's default (ops/migration SPEC
	// §3). The API lets only an administrator set it.
	MaxRatio int64 `json:"max_ratio,omitempty"`
}

// MaxMigrationRatio is the highest MigrationParams.MaxRatio.
const MaxMigrationRatio = 1000

// Sites lists the site migrated into.
func (p *MigrationParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// Validate requires a well-formed release ID.
func (p *MigrationParams) Validate() error {
	if !site.ValidReleaseID(p.ReleaseID) {
		return fmt.Errorf("invalid migration release id")
	}
	return nil
}

// MigrationResult is the release the import made and what went into it.
type MigrationResult struct {
	ReleaseID string `json:"release_id"`
	Files     int    `json:"files"`
	Bytes     int64  `json:"bytes"`
	// Skipped counts archive entries left out: links that would leave the
	// tree, and hard links to nothing a release can hold.
	Skipped int `json:"skipped,omitempty"`
}

// CurrentReleaseParams names the site whose live release is read.
type CurrentReleaseParams struct {
	Site state.Site `json:"site"`
}

// Sites lists the site read.
func (p *CurrentReleaseParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// CurrentReleaseResult is the release the site's current link names, ""
// when it names none (no link, or one that does not lead to a release
// directory).
type CurrentReleaseResult struct {
	ReleaseID string `json:"release_id"`
}

// RemoveReleasesParams removes releases of a site the panel knows were
// never made live: a build a restart interrupted after it had published
// its directory.
type RemoveReleasesParams struct {
	Site       state.Site `json:"site"`
	ReleaseIDs []string   `json:"release_ids"`
}

// Sites lists the site whose releases are removed.
func (p *RemoveReleasesParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// Validate requires well-formed release IDs (never initial), at most 100.
func (p *RemoveReleasesParams) Validate() error {
	if len(p.ReleaseIDs) > 100 {
		return fmt.Errorf("at most 100 releases at a time")
	}
	for _, id := range p.ReleaseIDs {
		if !site.ValidReleaseID(id) {
			return fmt.Errorf("invalid release id %q", id)
		}
	}
	return nil
}

// RemoveReleasesResult lists the releases removed: those that existed and
// were not live.
type RemoveReleasesResult struct {
	Removed []string `json:"removed"`
}
