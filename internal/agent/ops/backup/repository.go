package backup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

// Moving the repository (SPEC §13, #192): what the panel's move asks of the
// agent. RepositoryInfo reads a repository; CopyRepository copies every
// snapshot of one into another with `restic copy`.

// copyTimeout bounds one `restic copy` (SPEC §2): a whole repository over
// the network.
const copyTimeout = 12 * time.Hour

// RepositoryInfo reads a repository's ID and snapshots, and which of the
// expected snapshot IDs it does not hold (SPEC §13). It never initialises
// one.
func (o *Ops) RepositoryInfo(ctx context.Context, p rpc.RepositoryInfoParams) (rpc.RepositoryInfoResult, error) {
	repo, err := o.repoOf(ctx, p.Repository, p.Password, p.Env)
	if err != nil {
		return rpc.RepositoryInfoResult{}, err
	}
	for _, id := range p.Expect {
		if !ValidSnapshotID(id) {
			return rpc.RepositoryInfoResult{}, agentrpc.Invalid("invalid snapshot id")
		}
	}
	unlock, err := o.lock(ctx, repo.URL, false)
	if err != nil {
		return rpc.RepositoryInfoResult{}, err
	}
	defer unlock()
	cfg, err := o.repoConfig(ctx, repo)
	if err != nil {
		return rpc.RepositoryInfoResult{}, err
	}
	snaps, err := o.allSnapshots(ctx, repo)
	if err != nil {
		return rpc.RepositoryInfoResult{}, err
	}
	return rpc.RepositoryInfoResult{ID: cfg.ID, Snapshots: len(snaps), Missing: missingFrom(snaps, p.Expect)}, nil
}

// repoConfig is `restic cat config`, decoded.
func (o *Ops) repoConfig(ctx context.Context, repo host.Repo) (host.RepoConfig, error) {
	out, err := o.run(ctx, repo, metaTimeout, "cat", "config")
	if err != nil {
		return host.RepoConfig{}, err
	}
	return host.DecodeRepoConfig(out)
}

// allSnapshots is every snapshot of the repository, decoded strictly.
func (o *Ops) allSnapshots(ctx context.Context, repo host.Repo) ([]host.Snapshot, error) {
	out, err := o.run(ctx, repo, metaTimeout, "snapshots", "--json")
	if err != nil {
		return nil, err
	}
	snaps, err := host.DecodeSnapshots(out)
	if err != nil {
		return nil, fmt.Errorf("read the repository's snapshot list: %w", err)
	}
	return snaps, nil
}

// missingFrom is every id of expect that no snapshot of snaps names, by its
// ID or its original (SPEC §5), in order. Both sides are indexed by their
// first eight digits, the shortest ID either may hold.
func missingFrom(snaps []host.Snapshot, expect []string) []string {
	byPrefix := map[string][]host.Snapshot{}
	for _, s := range snaps {
		for _, id := range []string{s.ID, s.Original} {
			if len(id) >= 8 {
				k := strings.ToLower(id[:8])
				byPrefix[k] = append(byPrefix[k], s)
			}
		}
	}
	missing := []string{}
	for _, id := range expect {
		found := false
		for _, s := range byPrefix[strings.ToLower(id[:8])] {
			if names(s, id) {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, id)
		}
	}
	return missing
}

// CopyRepository copies every snapshot of From into To, making To when it
// does not exist (SPEC §13).
func (o *Ops) CopyRepository(ctx context.Context, p rpc.CopyRepositoryParams) (rpc.CopyRepositoryResult, error) {
	if p.From.Repository == p.To.Repository {
		return rpc.CopyRepositoryResult{}, agentrpc.Invalid("the two repositories are the same")
	}
	from, err := o.repoOf(ctx, p.From.Repository, p.From.Password, p.From.Env)
	if err != nil {
		return rpc.CopyRepositoryResult{}, err
	}
	to, err := o.repoOf(ctx, p.To.Repository, p.To.Password, p.To.Env)
	if err != nil {
		return rpc.CopyRepositoryResult{}, err
	}
	if err := copyable(from, to); err != nil {
		return rpc.CopyRepositoryResult{}, err
	}
	if err := o.readable(ctx, from); err != nil {
		return rpc.CopyRepositoryResult{}, err
	}
	to.From = &from
	if err := o.ensureCopyTarget(ctx, to); err != nil {
		return rpc.CopyRepositoryResult{}, err
	}

	unlock, err := o.lockBoth(ctx, from.URL, to.URL)
	if err != nil {
		return rpc.CopyRepositoryResult{}, err
	}
	defer unlock()
	if _, err := o.run(ctx, to, copyTimeout, "copy"); err != nil {
		return rpc.CopyRepositoryResult{}, fmt.Errorf("copy the snapshots: %w", err)
	}
	to.From = nil
	cfg, err := o.repoConfig(ctx, to)
	if err != nil {
		return rpc.CopyRepositoryResult{}, err
	}
	snaps, err := o.allSnapshots(ctx, to)
	if err != nil {
		return rpc.CopyRepositoryResult{}, err
	}
	return rpc.CopyRepositoryResult{ID: cfg.ID, Snapshots: len(snaps)}, nil
}

// copyable refuses two repositories restic cannot copy between in one run:
// one environment and one set of -o options serve both (SPEC §13).
func copyable(from, to host.Repo) error {
	for k, v := range from.Env {
		if w, ok := to.Env[k]; ok && host.ResticEnvKeys[k] && v != "" && w != "" && v != w {
			return agentrpc.Invalid("the two repositories need different %s values, and restic copies with one set: copy the snapshots yourself, then move with the files already copied", k)
		}
	}
	if isSFTP(from.URL) && isSFTP(to.URL) {
		fu, fh, fp, ferr := host.SFTPTarget(from.URL)
		tu, th, tp, terr := host.SFTPTarget(to.URL)
		if ferr != nil || terr != nil || fu != tu || fh != th || fp != tp {
			return agentrpc.Invalid("restic cannot copy between two SFTP servers in one run: copy the snapshots yourself, then move with the files already copied")
		}
	}
	return nil
}

func isSFTP(url string) bool { return strings.HasPrefix(url, "sftp:") }

// readable checks the source opens, under its shared lock.
func (o *Ops) readable(ctx context.Context, repo host.Repo) error {
	unlock, err := o.lock(ctx, repo.URL, false)
	if err != nil {
		return err
	}
	defer unlock()
	_, err = o.repoConfig(ctx, repo)
	return err
}

// ensureCopyTarget makes the copy's destination when it does not exist,
// with the source's chunker parameters (so the copied data deduplicates as
// it did), as ensureRepo does for a backup (SPEC §3, §13).
func (o *Ops) ensureCopyTarget(ctx context.Context, to host.Repo) error {
	unlock, err := o.lock(ctx, to.URL, true)
	if err != nil {
		return err
	}
	defer unlock()
	plain := to
	plain.From = nil
	_, readErr := o.run(ctx, plain, metaTimeout, "cat", "config")
	if readErr == nil {
		return nil
	}
	if errors.Is(readErr, host.ErrWrongPassword) || ctx.Err() != nil {
		return readErr
	}
	if _, err := o.run(ctx, to, metaTimeout, "init", "--copy-chunker-params"); err != nil {
		if errors.Is(readErr, host.ErrRepoNotFound) {
			return fmt.Errorf("initialise the new repository: %w", err)
		}
		return errors.Join(readErr, fmt.Errorf("initialise the new repository: %w", err))
	}
	return nil
}

// lockBoth takes the shared lock of two repositories, in the order of
// their URLs: two copies the other way round cannot wait on each other.
func (o *Ops) lockBoth(ctx context.Context, a, b string) (func(), error) {
	if b < a {
		a, b = b, a
	}
	ua, err := o.lock(ctx, a, false)
	if err != nil {
		return nil, err
	}
	ub, err := o.lock(ctx, b, false)
	if err != nil {
		ua()
		return nil, err
	}
	return func() { ub(); ua() }, nil
}
