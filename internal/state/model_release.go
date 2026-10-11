package state

import "time"

// DeploymentStatus is where a release is in the deployment state machine
// (SPEC.md "Deployments"). The statuses below are the original five;
// deployments.go adds the rest of the machine's.
type DeploymentStatus string

const (
	DeployCreated    DeploymentStatus = "created"
	DeployGuarding   DeploymentStatus = "guarding"
	DeployBlocked    DeploymentStatus = "blocked"
	DeployPromoted   DeploymentStatus = "promoted"
	DeployRolledBack DeploymentStatus = "rolled_back"
)

// Deployment is a release of a site's code; once recorded it never changes.
type Deployment struct {
	ID     int64 `json:"id"`
	SiteID int64 `json:"site_id"`
	// ReleaseID names the release directory, e.g. 20260711-151844.
	ReleaseID string           `json:"release_id"`
	Path      string           `json:"path"`
	Checksum  string           `json:"checksum"`
	Status    DeploymentStatus `json:"status"`
	// GuardJSON is Performance Guard's report, serialised.
	GuardJSON  string     `json:"guard_json,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	PromotedAt *time.Time `json:"promoted_at,omitempty"`
}

// BackupVerify is what the latest restore test of a backup found.
type BackupVerify string

const (
	VerifyPending BackupVerify = "pending"
	VerifyPassed  BackupVerify = "passed"
	VerifyFailed  BackupVerify = "failed"
)

// Backup is one restic snapshot and how its restore test went.
type Backup struct {
	ID         int64  `json:"id"`
	SiteID     int64  `json:"site_id"`
	SnapshotID string `json:"snapshot_id"`
	Repository string `json:"repository"`
	SizeBytes  int64  `json:"size_bytes"`
	// Kind is files, database or full, or what a safety snapshot was
	// taken before (pre-restore, pre-database-push, pre-migration).
	Kind         string       `json:"kind"`
	VerifyStatus BackupVerify `json:"verify_status"`
	VerifiedAt   *time.Time   `json:"verified_at,omitempty"`
	RestoreEstMS int64        `json:"restore_estimate_ms,omitempty"`
	CreatedAt    time.Time    `json:"created_at"`
}
