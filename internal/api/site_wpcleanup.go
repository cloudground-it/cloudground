package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// A WordPress site's scheduled database cleanup (SPEC.md §3.17, issue
// #78): once a week, at the site's own slot, its expired transients are
// deleted and its revisions trimmed, behind a dry run that is logged and, for
// the first run of a site, a tested snapshot. Off by default.

const wpCleanupEvery = 7 * 24 * time.Hour

func wpCleanupKey(id int64) string  { return "wpcleanup:site:" + strconv.FormatInt(id, 10) }
func wpCleanupDone(id int64) string { return "wpcleanup:done:" + strconv.FormatInt(id, 10) }
func wpCleanupSnap(id int64) string { return "wpcleanup:snapshot:" + strconv.FormatInt(id, 10) }

// wpCleanupSlot is where in the week a site's cleanup runs, spread by id
// like the malware scan's but at other hours.
func wpCleanupSlot(id int64) time.Duration {
	return time.Duration((id*53+60)%168) * time.Hour
}

// lastCleanupSlot is the newest slot of a site at or before now.
func lastCleanupSlot(id int64, now time.Time) time.Time {
	u := now.UTC()
	monday := time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -((int(u.Weekday()) + 6) % 7))
	s := monday.Add(wpCleanupSlot(id))
	if s.After(u) {
		s = s.Add(-wpCleanupEvery)
	}
	return s
}

// wpCleanupView is what the routes answer.
type wpCleanupView struct {
	Enabled       bool       `json:"enabled"`
	KeepRevisions int        `json:"keep_revisions"`
	LastRun       *time.Time `json:"last_run"`
	NextRun       *time.Time `json:"next_run"`
	// BackupsReady is whether the off-site repository is set: the first run
	// of a site takes a snapshot, so a site cannot be scheduled without.
	BackupsReady bool `json:"backups_ready"`
}

func (s *Server) wpCleanupView(st state.Site) (wpCleanupView, error) {
	v := wpCleanupView{Enabled: st.Config.WPCleanup.Enabled, KeepRevisions: site.KeepRevisions(st.Config.WPCleanup)}
	repo, password, err := s.backupRepository()
	if err != nil {
		return v, err
	}
	v.BackupsReady = repo != "" && password != ""
	if last, err := s.Store.LastScheduleRun(wpCleanupDone(st.ID)); err != nil {
		return v, err
	} else if !last.IsZero() {
		v.LastRun = &last
	}
	if v.Enabled {
		next := lastCleanupSlot(st.ID, time.Now()).Add(wpCleanupEvery)
		v.NextRun = &next
	}
	return v, nil
}

func (s *Server) handleGetWPCleanup(w http.ResponseWriter, r *http.Request, c *call) {
	if !isWordPress(c.Site.Type) {
		fail(w, errNotWordPress)
		return
	}
	v, err := s.wpCleanupView(c.Site)
	if err != nil {
		internalError(w, "wpCleanupView", err, "")
		return
	}
	respond(w, http.StatusOK, v)
}

// handleSetWPCleanup turns the schedule on or off and sets how many
// revisions of a post stay (a field left out keeps its value).
func (s *Server) handleSetWPCleanup(w http.ResponseWriter, r *http.Request, c *call) {
	if !isWordPress(c.Site.Type) {
		fail(w, errNotWordPress)
		return
	}
	var req struct {
		Enabled       *bool `json:"enabled"`
		KeepRevisions *int  `json:"keep_revisions"`
	}
	if !decodeOr400(w, r, &req) {
		return
	}
	if req.Enabled == nil && req.KeepRevisions == nil {
		respondErr(w, http.StatusBadRequest, "name at least one setting to change")
		return
	}
	cur, unlock, err := s.freshSite(c.Site.ID)
	defer unlock()
	if err != nil {
		internalError(w, "GetSite", err, "could not read the site")
		return
	}
	next := cur.Config.WPCleanup
	if req.Enabled != nil {
		next.Enabled = *req.Enabled
	}
	if req.KeepRevisions != nil {
		next.KeepRevisions = *req.KeepRevisions
	}
	if err := site.CheckWPCleanup(next); err != nil {
		respondErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if next.Enabled && !cur.Config.WPCleanup.Enabled {
		if repo, password, err := s.backupRepository(); err != nil {
			internalError(w, "backupRepository", err, "")
			return
		} else if repo == "" || password == "" {
			respondErr(w, http.StatusPreconditionFailed, msgCleanupNeedsBackups)
			return
		}
	}
	stored, err := s.Store.MutateSiteApplied(cur.ID, func(st *state.Site) error {
		st.Config.WPCleanup = next
		return nil
	})
	if err != nil {
		internalError(w, "MutateSiteApplied", err, "")
		return
	}
	if next.Enabled && !cur.Config.WPCleanup.Enabled {
		// The first scheduled run is at the next slot, not at once.
		s.logIfErr(s.Store.MarkScheduleRun(wpCleanupKey(cur.ID)), "MarkScheduleRun")
	}
	detail := fmt.Sprintf("enabled=%t keep_revisions=%d", next.Enabled, site.KeepRevisions(next))
	s.audit(c.Actor(), "site.wpcleanup", cur.Domain, detail)
	v, err := s.wpCleanupView(stored)
	if err != nil {
		internalError(w, "wpCleanupView", err, "")
		return
	}
	respond(w, http.StatusOK, v)
}

// msgCleanupNeedsBackups is why a cleanup cannot be scheduled or run yet.
const msgCleanupNeedsBackups = "configure backup_repository and backup_password in settings first: the first cleanup of a site takes a tested snapshot"

// handleWPCleanupPreview counts what a cleanup would delete, changing
// nothing.
func (s *Server) handleWPCleanupPreview(w http.ResponseWriter, r *http.Request, c *call) {
	if !isWordPress(c.Site.Type) {
		fail(w, errNotWordPress)
		return
	}
	var res rpc.DBStudioCleanupResult
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.DBStudioWPCleanup, rpc.DBStudioCleanupParams{
		Site: c.Site, KeepRevisions: site.KeepRevisions(c.Site.Config.WPCleanup), DryRun: true,
	}, &res); err != nil {
		respondAgentErr(w, 0, "", err)
		return
	}
	respond(w, http.StatusOK, res)
}

// handleWPCleanupRun queues a cleanup now.
func (s *Server) handleWPCleanupRun(w http.ResponseWriter, r *http.Request, c *call) {
	if !isWordPress(c.Site.Type) {
		fail(w, errNotWordPress)
		return
	}
	if repo, password, err := s.backupRepository(); err != nil {
		internalError(w, "backupRepository", err, "")
		return
	} else if repo == "" || password == "" {
		respondErr(w, http.StatusPreconditionFailed, msgCleanupNeedsBackups)
		return
	}
	s.audit(c.Actor(), "site.wpcleanup.run", c.Site.Domain, "")
	j, err := s.enqueueWPCleanup(c.Site.ID, c.Actor(), false)
	if err != nil {
		respondJobErr(w, err)
		return
	}
	accepted(w, jobs.View(j), nil)
}

// enqueueWPCleanup queues a site's cleanup under its lock and the
// repository's; one already queued or running is joined.
func (s *Server) enqueueWPCleanup(siteID int64, actor string, background bool) (state.Job, error) {
	locks := siteLocks(siteID)
	if repo, _, err := s.backupRepository(); err == nil && repo != "" {
		locks = append(locks, jobs.RepoLock(repo))
	}
	const kind = "wp.cleanup"
	record := jobRecord{SiteID: siteID}
	if st, err := s.Store.GetSite(siteID); err == nil {
		record.Domain = st.Domain
	}
	return s.Jobs.Enqueue(jobs.Spec{
		Kind: kind, SiteID: siteID, Locks: locks, Background: background,
		DedupKey: kind + ":" + strconv.FormatInt(siteID, 10), Payload: record,
		Func: func(ctx context.Context, r *jobs.Run) error {
			st, err := s.jobSite(siteID, kind)
			if err != nil {
				return err
			}
			return s.cleanupWork(ctx, st, actor, r.Progress)
		},
	})
}

// cleanupWork is the wp.cleanup job: a dry run, logged; for the site's first
// cleanup a tested off-site snapshot; the cleanup; its result in the job, the
// audit trail and the site's cleanup log.
func (s *Server) cleanupWork(ctx context.Context, st state.Site, actor string, progress func(int, string)) error {
	repo, password, err := s.backupRepository()
	if err != nil {
		return err
	}
	if repo == "" || password == "" {
		return fmt.Errorf("%s", msgCleanupNeedsBackups)
	}
	keep := site.KeepRevisions(st.Config.WPCleanup)
	progress(10, "Counting what a cleanup would delete")
	var dry rpc.DBStudioCleanupResult
	if err := agentrpc.Into(ctx, s.Agent, agentrpc.DBStudioWPCleanup, rpc.DBStudioCleanupParams{Site: st, KeepRevisions: keep, DryRun: true}, &dry); err != nil {
		return err
	}
	progress(20, fmt.Sprintf("Dry run: %d expired transients and %d revisions beyond %d per post would be deleted; %d autoloaded options, %s",
		dry.ExpiredTransients, dry.ExcessRevisions, keep, dry.AutoloadCount, cleanupSize(dry.AutoloadBytes)))
	if last, err := s.Store.LastScheduleRun(wpCleanupSnap(st.ID)); err != nil {
		return err
	} else if last.IsZero() {
		// The first cleanup of a site is behind a snapshot restored to a
		// scratch place and compared, as the other changes to a database are.
		if _, err := s.safetySnapshot(ctx, st, repo, password, "pre-cleanup", progress); err != nil {
			return err
		}
		s.logIfErr(s.Store.MarkScheduleRun(wpCleanupSnap(st.ID)), "MarkScheduleRun")
	}
	if err := checkpoint(ctx, progress, "the cleanup"); err != nil {
		return err
	}
	progress(50, "Deleting expired transients and surplus revisions")
	var res rpc.DBStudioCleanupResult
	if err := agentrpc.Into(ctx, s.Agent, agentrpc.DBStudioWPCleanup, rpc.DBStudioCleanupParams{Site: st, KeepRevisions: keep}, &res); err != nil {
		return err
	}
	s.logIfErr(s.Store.MarkScheduleRun(wpCleanupDone(st.ID)), "MarkScheduleRun")
	summary := fmt.Sprintf("%d expired transients and %d revisions deleted; %d autoloaded options, %s",
		res.DeletedTransients, res.DeletedRevisions, res.AutoloadCount, cleanupSize(res.AutoloadBytes))
	s.audit(actor, "site.wpcleanup.done", st.Domain, summary)
	progress(100, "Done: "+summary)
	return nil
}

// cleanupSize writes a size for a log line.
func cleanupSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

// runWPCleanupSweep is the wpcleanup.sweep job (every maintenance tick).
func (s *Server) runWPCleanupSweep(ctx context.Context, r *jobs.Run) error {
	sites, err := s.Store.ListSites()
	if err != nil {
		return err
	}
	queued, err := s.queueWPCleanups(sites, time.Now())
	if err != nil {
		return err
	}
	r.Progress(100, fmt.Sprintf("%d database cleanups queued", queued))
	return nil
}

// queueWPCleanups queues the cleanup of each scheduled site whose slot has
// come since it was last queued. A site the sweep sees for the first time
// is only noted: its first cleanup is at its next slot.
func (s *Server) queueWPCleanups(sites []state.Site, now time.Time) (int, error) {
	queued := 0
	for _, st := range sites {
		if st.Status != state.SiteActive || !st.Config.WPCleanup.Enabled || !isWordPress(st.Type) {
			continue
		}
		key := wpCleanupKey(st.ID)
		last, err := s.Store.LastScheduleRun(key)
		if err != nil {
			return queued, err
		}
		if last.IsZero() {
			s.logIfErr(s.Store.MarkScheduleRun(key), "MarkScheduleRun")
			continue
		}
		if !last.Before(lastCleanupSlot(st.ID, now)) {
			continue
		}
		if _, err := s.enqueueWPCleanup(st.ID, "system", true); err != nil {
			return queued, err
		}
		s.logIfErr(s.Store.MarkScheduleRun(key), "MarkScheduleRun")
		queued++
	}
	return queued, nil
}
