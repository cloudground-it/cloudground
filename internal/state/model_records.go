package state

import "time"

// The records kept beside the desired state (SPEC.md "Records").

// CronJob is a command run on a schedule as its site's Unix user.
type CronJob struct {
	ID     int64 `json:"id"`
	SiteID int64 `json:"site_id"`
	// Schedule is a five-field cron expression.
	Schedule    string     `json:"schedule"`
	Command     string     `json:"command"`
	Description string     `json:"description"`
	Enabled     bool       `json:"enabled"`
	LastRun     *time.Time `json:"last_run,omitempty"`
	LastStatus  string     `json:"last_status"`
	CreatedAt   time.Time  `json:"created_at"`
}

// MetricSample is one small snapshot of the server's headroom. A day of
// them is kept, which needs no time-series service and never grows.
type MetricSample struct {
	CPUHeadroomPct  int       `json:"cpu_headroom_pct"`
	MemHeadroomPct  int       `json:"mem_headroom_pct"`
	DiskHeadroomPct int       `json:"disk_headroom_pct"`
	Load1           float64   `json:"load1"`
	SampledAt       time.Time `json:"sampled_at"`
}

// AuditEvent is one entry of the audit trail, never changed once written.
type AuditEvent struct {
	ID        int64     `json:"id"`
	Actor     string    `json:"actor"`
	Action    string    `json:"action"`
	Subject   string    `json:"subject"`
	Detail    string    `json:"detail,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// DriftStatus is how a drift event stands: open until restored or accepted.
type DriftStatus string

const (
	DriftOpen     DriftStatus = "open"
	DriftRestored DriftStatus = "restored"
	DriftAccepted DriftStatus = "accepted"
)

// DriftEvent is a managed file found changed by something other than the
// panel.
type DriftEvent struct {
	ID           int64       `json:"id"`
	Path         string      `json:"path"`
	ExpectedHash string      `json:"expected_hash"`
	ActualHash   string      `json:"actual_hash"`
	Status       DriftStatus `json:"status"`
	DetectedAt   time.Time   `json:"detected_at"`
	ResolvedAt   *time.Time  `json:"resolved_at,omitempty"`
}
