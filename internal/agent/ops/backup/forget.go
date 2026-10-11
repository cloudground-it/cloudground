package backup

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

// ForgetSnapshot deletes one of a site's backups: its files snapshot and
// the database snapshot that one names (SPEC §9); their data goes at the
// next prune (PruneRepository). The
// API holds its repository lock; here the exclusive lock is taken, as
// for a retention prune.
func (o *Ops) ForgetSnapshot(ctx context.Context, p rpc.ForgetSnapshotParams) (rpc.ForgetSnapshotResult, error) {
	if !ValidSnapshotID(p.SnapshotID) {
		return rpc.ForgetSnapshotResult{}, agentrpc.Invalid("invalid snapshot id")
	}
	repo, err := o.repoOf(ctx, p.Repository, p.Password, p.Env)
	if err != nil {
		return rpc.ForgetSnapshotResult{}, err
	}
	unlock, err := o.lock(ctx, repo.URL, true)
	if err != nil {
		return rpc.ForgetSnapshotResult{}, err
	}
	defer unlock()

	// The site's own snapshots only: an ID of another site's backup is not
	// found here, whatever it names.
	out, err := o.run(ctx, repo, metaTimeout, "snapshots", "--json", "--tag", tagSite+p.Site.Domain)
	if err != nil {
		return rpc.ForgetSnapshotResult{}, err
	}
	snaps, err := host.DecodeSnapshots(out)
	if err != nil {
		return rpc.ForgetSnapshotResult{}, fmt.Errorf("read the repository's snapshot list: %w", err)
	}
	files, err := matchSnapshot(snaps, p.SnapshotID)
	if err != nil {
		return rpc.ForgetSnapshotResult{}, err
	}
	if files == nil {
		// Already gone. A database half no files snapshot names any more
		// (the files half was forgotten without it) goes now: nothing can
		// restore from it. The exclusive lock means no backup is between
		// its two snapshots.
		orphans := orphanDatabases(snaps)
		if len(orphans) > 0 {
			if _, err := o.run(ctx, repo, planTimeout, append([]string{"forget"}, orphans...)...); err != nil {
				return rpc.ForgetSnapshotResult{}, err
			}
		}
		return rpc.ForgetSnapshotResult{Forgotten: append([]string{}, orphans...), Missing: true}, nil
	}
	if slices.Contains(files.Tags, tagDatabase) {
		return rpc.ForgetSnapshotResult{}, agentrpc.Invalid("snapshot %s is the database half of a backup; delete the backup it belongs to", short(files.ID))
	}
	doomed := []string{files.ID}
	for _, t := range files.Tags {
		dbID, ok := strings.CutPrefix(t, tagDB)
		if !ok || !ValidSnapshotID(dbID) {
			continue
		}
		// Only a database snapshot of this site goes with it; one the tag
		// names that is not there is already gone.
		if d, err := matchSnapshot(snaps, dbID); err == nil && d != nil && slices.Contains(d.Tags, tagDatabase) {
			doomed = append(doomed, d.ID)
		}
	}
	for _, id := range doomed {
		if !ValidSnapshotID(id) {
			return rpc.ForgetSnapshotResult{}, fmt.Errorf("the repository lists an invalid snapshot id")
		}
	}
	// No --prune: a prune rewrites the repository's packs and can take
	// hours; the data goes at the next scheduled prune (SPEC §10).
	if _, err := o.run(ctx, repo, planTimeout, append([]string{"forget"}, doomed...)...); err != nil {
		return rpc.ForgetSnapshotResult{}, err
	}
	return rpc.ForgetSnapshotResult{Forgotten: doomed}, nil
}

// matchSnapshot finds the snapshot id names among snaps (either may be a
// prefix of the other); nil when none does. Two matches are refused: a
// short ID that names two snapshots names neither.
func matchSnapshot(snaps []host.Snapshot, id string) (*host.Snapshot, error) {
	var found *host.Snapshot
	for i := range snaps {
		if !protected(snaps[i].ID, []string{id}) || snaps[i].ID == "" {
			continue
		}
		if found != nil {
			return nil, agentrpc.Invalid("snapshot id %s is ambiguous", id)
		}
		found = &snaps[i]
	}
	return found, nil
}

// orphanDatabases lists the database snapshots of snaps that no files
// snapshot of snaps names in its db: tag.
func orphanDatabases(snaps []host.Snapshot) []string {
	var named []string
	for _, s := range snaps {
		for _, t := range s.Tags {
			if v, ok := strings.CutPrefix(t, tagDB); ok && ValidSnapshotID(v) {
				named = append(named, v)
			}
		}
	}
	var out []string
	for _, s := range snaps {
		if slices.Contains(s.Tags, tagDatabase) && ValidSnapshotID(s.ID) && !protected(s.ID, named) {
			out = append(out, s.ID)
		}
	}
	return out
}

// PruneRepository removes the data of forgotten snapshots (SPEC §10), under
// the exclusive lock.
func (o *Ops) PruneRepository(ctx context.Context, p rpc.TestBackupParams) (map[string]any, error) {
	repo, err := o.repoOf(ctx, p.Repository, p.Password, p.Env)
	if err != nil {
		return nil, err
	}
	unlock, err := o.lock(ctx, repo.URL, true)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if _, err := o.run(ctx, repo, pruneTimeout, "prune"); err != nil {
		return nil, err
	}
	return map[string]any{"pruned": true}, nil
}

func short(id string) string { return id[:min(len(id), 8)] }
