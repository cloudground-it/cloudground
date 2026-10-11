package rpc

import (
	"errors"
	"strings"

	"github.com/cloudground-it/cloudground/internal/state"
)

// A restic repository is named by its URL (a local path, s3:…, b2:…) and
// opened with its password. Env carries the object store's credentials,
// which reach restic through its environment and never its command line.

// BackupParams takes a snapshot of a site: Kind "files", "database", or
// "full" (both; also the meaning of an empty Kind).
type BackupParams struct {
	Site       state.Site        `json:"site"`
	Repository string            `json:"repository"`
	Password   string            `json:"password"`
	Kind       string            `json:"kind"`
	Env        map[string]string `json:"env,omitempty"`
}

// Sites lists the site backed up.
func (p *BackupParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// Validate requires a known kind.
func (p *BackupParams) Validate() error {
	switch p.Kind {
	case "", "full", "files", "database":
		return nil
	}
	return errors.New("backup kind must be full, files, or database")
}

// BackupResult is the snapshot taken.
type BackupResult struct {
	SnapshotID string `json:"snapshot_id"`
	SizeBytes  int64  `json:"size_bytes"`
	// Databases are the names of the databases the snapshot holds, which
	// a restore may choose among (RestoreParams.Databases).
	Databases []string `json:"databases"`
}

// RestoreParams names a snapshot of a site. RestoreSnapshot puts it back
// over the live site (Mode "files", "database", or "full", the default);
// VerifyBackup restores it aside and checks it.
type RestoreParams struct {
	Site       state.Site        `json:"site"`
	Repository string            `json:"repository"`
	Password   string            `json:"password"`
	SnapshotID string            `json:"snapshot_id"`
	Mode       string            `json:"mode,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	// Databases limits a database or full restore to these of the
	// snapshot's databases; empty restores every one the site still has.
	Databases []string `json:"databases,omitempty"`
}

// Sites lists the site restored.
func (p *RestoreParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// Validate requires a known mode, in any case and spacing.
func (p *RestoreParams) Validate() error {
	switch strings.ToLower(strings.TrimSpace(p.Mode)) {
	case "", "full", "files", "database":
		return nil
	}
	return errors.New("restore mode must be full, files, or database")
}

// VerifyResult is a restore test's verdict and how long the restore took.
type VerifyResult struct {
	Passed        bool   `json:"passed"`
	RestoreMillis int64  `json:"restore_millis"`
	Detail        string `json:"detail,omitempty"`
}

// TestBackupParams names a repository to reach, and to initialise when it
// is empty.
type TestBackupParams struct {
	Repository string            `json:"repository"`
	Password   string            `json:"password"`
	Env        map[string]string `json:"env,omitempty"`
}

// RetentionParams forgets a site's snapshots older than KeepWithinDays.
// Protect lists snapshot IDs (full or short) kept whatever their age: the
// panel names the site's newest verified snapshot, so failed verifications
// can never leave a site without a restore point known to work.
type RetentionParams struct {
	Site           state.Site        `json:"site"`
	Repository     string            `json:"repository"`
	Password       string            `json:"password"`
	KeepWithinDays int               `json:"keep_within_days"`
	Protect        []string          `json:"protect,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
}

// Sites lists the site whose snapshots are pruned.
func (p *RetentionParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// RetentionResult lists, by full ID, the snapshots forgotten and the ones
// kept.
type RetentionResult struct {
	Forgotten []string `json:"forgotten"`
	Kept      []string `json:"kept"`
}

// ForgetSnapshotParams deletes one backup of a site from its repository:
// the files snapshot SnapshotID (full or short) and the database snapshot
// its db: tag names, then prunes their data (R-4.3).
type ForgetSnapshotParams struct {
	Site       state.Site        `json:"site"`
	Repository string            `json:"repository"`
	Password   string            `json:"password"`
	SnapshotID string            `json:"snapshot_id"`
	Env        map[string]string `json:"env,omitempty"`
}

// Sites lists the site whose backup is deleted.
func (p *ForgetSnapshotParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// Validate requires a snapshot ID: 8 to 64 hex digits.
func (p *ForgetSnapshotParams) Validate() error {
	if !snapshotID(p.SnapshotID) {
		return errors.New("invalid snapshot id")
	}
	return nil
}

// snapshotID reports whether id is a restic snapshot ID, full or short.
func snapshotID(id string) bool {
	if len(id) < 8 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
			return false
		}
	}
	return true
}

// ForgetSnapshotResult lists, by full ID, the snapshots forgotten. Missing
// is true when the repository has no such snapshot of the site (nothing
// was forgotten: it is already gone).
type ForgetSnapshotResult struct {
	Forgotten []string `json:"forgotten"`
	Missing   bool     `json:"missing,omitempty"`
}
