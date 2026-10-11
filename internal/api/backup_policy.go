package api

// A site's backup policy (SPEC.md §5.8): whether the scheduled backups run,
// how often, and how many days of snapshots the retention keeps. The policy
// is read by the backup and retention sweeps (backup_retention.go) and
// renders nothing, so the change is a store write, not a job.

import (
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/cloudground-it/cloudground/internal/state"
)

// maxRetentionDays bounds the retention window: ten years.
const maxRetentionDays = 3650

// backupSchedules are the spacings the backup sweep knows (backupEvery).
var backupSchedules = []string{"hourly", "daily", "weekly"}

// policyChange is the body of PUT /api/sites/{id}/backups/policy. A nil
// field keeps its value.
type policyChange struct {
	Enabled       *bool   `json:"enabled"`
	Schedule      *string `json:"schedule"`
	RetentionDays *int    `json:"retention_days"`
}

// Messages of the policy route.
const (
	msgPolicyStaging   = "a staging site has no backup schedule of its own"
	msgPolicyEmpty     = "name at least one of enabled, schedule, retention_days"
	msgPolicyRetention = "only an administrator can change how long a site's backups are kept"
)

// check says what is wrong with the change for site, field by field, and
// whether the caller may make it at all ("" and nil when it is fine).
func (pc policyChange) check(site state.Site, admin bool) (kind problemKind, msg string, fields map[string]string) {
	if site.StagingOf != 0 {
		return probInvalid, msgPolicyStaging, nil
	}
	if pc.Enabled == nil && pc.Schedule == nil && pc.RetentionDays == nil {
		return probInvalid, msgPolicyEmpty, nil
	}
	// A shorter window deletes snapshots, as deleting one does: an
	// administrator's call.
	if pc.RetentionDays != nil && *pc.RetentionDays != site.Config.Backups.RetentionDays && !admin {
		return probForbidden, msgPolicyRetention, map[string]string{"retention_days": msgPolicyRetention}
	}
	fields = map[string]string{}
	if pc.Schedule != nil && !slices.Contains(backupSchedules, *pc.Schedule) {
		fields["schedule"] = "schedule must be hourly, daily or weekly"
	}
	if pc.RetentionDays != nil && (*pc.RetentionDays < 0 || *pc.RetentionDays > maxRetentionDays) {
		fields["retention_days"] = fmt.Sprintf("retention must be 0–%d days (0 keeps every snapshot)", maxRetentionDays)
	}
	for _, name := range []string{"schedule", "retention_days"} {
		if m, ok := fields[name]; ok {
			return probInvalid, m, fields
		}
	}
	return 0, "", nil
}

// apply writes the change into a policy.
func (pc policyChange) apply(p *state.BackupPolicy) {
	if pc.Enabled != nil {
		p.Enabled = *pc.Enabled
	}
	if pc.Schedule != nil {
		p.Schedule = *pc.Schedule
	}
	if pc.RetentionDays != nil {
		p.RetentionDays = *pc.RetentionDays
	}
	if p.Schedule == "" {
		p.Schedule = "daily" // what the sweep runs for an empty one
	}
}

// handleBackupPolicy is PUT /api/sites/{id}/backups/policy. It answers the
// site's policy as stored.
func (s *Server) handleBackupPolicy(w http.ResponseWriter, r *http.Request, c *call) {
	var pc policyChange
	if err := decode(r, &pc); err != nil {
		respondErr(w, http.StatusBadRequest, "malformed request: "+err.Error())
		return
	}
	if kind, msg, fields := pc.check(c.Site, c.User.Role == roleAdmin); msg != "" {
		replyFailure(w, &problem{kind: kind, msg: msg, fields: fields})
		return
	}
	// Nothing on the server changes: a site that was applied stays applied.
	site, err := s.Store.MutateSiteApplied(c.Site.ID, func(st *state.Site) error {
		pc.apply(&st.Config.Backups)
		return nil
	})
	if errors.Is(err, state.ErrNotFound) {
		respondErr(w, http.StatusNotFound, "site not found")
		return
	}
	if err != nil {
		internalError(w, "MutateSiteApplied", err, "")
		return
	}
	p := site.Config.Backups
	s.audit(c.Actor(), "backup.policy", site.Domain,
		fmt.Sprintf("enabled=%t schedule=%s retention_days=%d", p.Enabled, p.Schedule, p.RetentionDays))
	respond(w, http.StatusOK, p)
}
