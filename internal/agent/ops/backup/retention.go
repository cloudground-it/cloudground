package backup

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

// TestBackup checks a repository can be used, initialising it when it is new
// (SPEC §7): an operator learns their destination works before relying on it.
func (o *Ops) TestBackup(ctx context.Context, p rpc.TestBackupParams) (map[string]any, error) {
	repo, err := o.repoOf(ctx, p.Repository, p.Password, p.Env)
	if err != nil {
		return nil, err
	}
	if err := o.ensureRepo(ctx, repo); err != nil {
		return nil, err
	}
	unlock, err := o.lock(ctx, repo.URL, false)
	if err != nil {
		return nil, err
	}
	defer unlock()
	out, err := o.run(ctx, repo, metaTimeout, "snapshots", "--json")
	if err != nil {
		return nil, err
	}
	// "Reachable, 0 snapshots" for a list that could not be read would tell
	// the operator their backups are fine.
	snaps, err := host.DecodeSnapshots(out)
	if err != nil {
		return nil, fmt.Errorf("read the repository's snapshot list: %w", err)
	}
	return map[string]any{"reachable": true, "snapshots": len(snaps)}, nil
}

// BackupRetention forgets a site's snapshots outside its retention window,
// except the protected ones and the database snapshots of every file
// snapshot that stays (SPEC §8).
func (o *Ops) BackupRetention(ctx context.Context, p rpc.RetentionParams) (rpc.RetentionResult, error) {
	repo, err := o.repoOf(ctx, p.Repository, p.Password, p.Env)
	if err != nil {
		return rpc.RetentionResult{}, err
	}
	if p.KeepWithinDays < 1 {
		return rpc.RetentionResult{}, agentrpc.Invalid("retention must keep at least one day of snapshots")
	}
	for _, id := range p.Protect {
		if !ValidSnapshotID(id) {
			return rpc.RetentionResult{}, agentrpc.Invalid("invalid protected snapshot id")
		}
	}
	hn, err := o.hostname()
	if err != nil {
		return rpc.RetentionResult{}, fmt.Errorf("cannot scope retention to this host: %v", err)
	}

	// A prune holds restic's exclusive lock: a backup started meanwhile
	// would fail.
	unlock, err := o.lock(ctx, repo.URL, true)
	if err != nil {
		return rpc.RetentionResult{}, err
	}
	defer unlock()
	out, err := o.run(ctx, repo, planTimeout, "forget", "--dry-run", "--json",
		"--tag", tagSite+p.Site.Domain, "--host", hn, "--keep-within", fmt.Sprintf("%dd", p.KeepWithinDays))
	if err != nil {
		return rpc.RetentionResult{}, err
	}
	groups, err := host.DecodeForgetPlan(out)
	if err != nil {
		return rpc.RetentionResult{}, err
	}
	// The plan's snapshots with the IDs they had before a copy (SPEC §5),
	// from the site's own list: what the panel's records and db: tags
	// may name. Nothing is planned on a list that could not be read.
	if err := o.withOriginals(ctx, repo, p.Site.Domain, groups); err != nil {
		return rpc.RetentionResult{}, err
	}
	res, err := planRetention(groups, p.Protect)
	if err != nil {
		return rpc.RetentionResult{}, err
	}
	if len(res.Forgotten) == 0 {
		return res, nil
	}
	if _, err := o.run(ctx, repo, pruneTimeout, append([]string{"forget", "--prune"}, res.Forgotten...)...); err != nil {
		return rpc.RetentionResult{}, err
	}
	return res, nil
}

// withOriginals fills in the original ID of each planned snapshot that
// has one, from the site's snapshot list.
func (o *Ops) withOriginals(ctx context.Context, repo host.Repo, domain string, groups []host.ForgetGroup) error {
	out, err := o.run(ctx, repo, metaTimeout, "snapshots", "--json", "--tag", tagSite+domain)
	if err != nil {
		return err
	}
	snaps, err := host.DecodeSnapshots(out)
	if err != nil {
		return fmt.Errorf("read the site's snapshots: %w", err)
	}
	original := map[string]string{}
	for _, s := range snaps {
		if s.Original != "" {
			original[s.ID] = s.Original
		}
	}
	for g := range groups {
		for _, list := range [][]host.Snapshot{groups[g].Keep, groups[g].Remove} {
			for i := range list {
				if list[i].Original == "" {
					list[i].Original = original[list[i].ID]
				}
			}
		}
	}
	return nil
}

// planRetention decides, from restic's plan, what is forgotten and what is
// kept.
func planRetention(groups []host.ForgetGroup, protect []string) (rpc.RetentionResult, error) {
	res := rpc.RetentionResult{Forgotten: []string{}, Kept: []string{}}
	tags := map[string][]string{}
	original := map[string]string{}
	var remove []string
	for _, g := range groups {
		for _, s := range g.Keep {
			tags[s.ID] = s.Tags
			res.Kept = append(res.Kept, s.ID)
		}
		for _, s := range g.Remove {
			tags[s.ID] = s.Tags
			original[s.ID] = s.Original
			switch {
			case len(g.Keep) == 0:
				// keep-within always keeps a group's newest: this is not
				// the output it looks like, and deleting on a misread is
				// how backups disappear.
				res.Kept = append(res.Kept, s.ID)
			case !ValidSnapshotID(s.ID):
				return rpc.RetentionResult{}, fmt.Errorf("restic's retention plan has an invalid snapshot id")
			case protected(s.ID, protect), s.Original != "" && protected(s.Original, protect):
				// The panel's record of a copied snapshot holds its
				// original ID (SPEC §5).
				res.Kept = append(res.Kept, s.ID)
			default:
				remove = append(remove, s.ID)
			}
		}
	}
	paired := pairedDatabases(res.Kept, tags)
	for _, id := range remove {
		if protected(id, paired) || original[id] != "" && protected(original[id], paired) {
			res.Kept = append(res.Kept, id)
		} else {
			res.Forgotten = append(res.Forgotten, id)
			if o := original[id]; o != "" && ValidSnapshotID(o) {
				res.Originals = append(res.Originals, o)
			}
		}
	}
	return res, nil
}

// pairedDatabases lists the parts the kept file snapshots name: their
// database snapshots and their descriptions (manifest:).
func pairedDatabases(kept []string, tags map[string][]string) []string {
	var out []string
	for _, id := range kept {
		for _, t := range tags[id] {
			v, ok := strings.CutPrefix(t, tagDB)
			if !ok {
				v, ok = strings.CutPrefix(t, tagManifest)
			}
			if ok && ValidSnapshotID(v) {
				out = append(out, v)
			}
		}
	}
	return out
}

// protected reports whether id is one of ids; either may be the other's
// prefix (a short and a full ID).
func protected(id string, ids []string) bool {
	id = strings.ToLower(id)
	for _, p := range ids {
		p = strings.ToLower(p)
		if strings.HasPrefix(id, p) || strings.HasPrefix(p, id) {
			return true
		}
	}
	return false
}
