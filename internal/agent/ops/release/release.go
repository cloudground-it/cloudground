// Package release builds a site's releases (immutable copies of its code
// under releases/<id>), points the site's current link at one of them, rolls
// back, and prunes the old ones. SPEC.md in this directory is the contract.
package release

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"runtime"
	"sort"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// The site layout this package owns (SPEC §1).
const (
	// CurrentLink is the relative link naming the live release.
	CurrentLink = "current"
	// Dir holds the releases, one directory each.
	Dir = "releases"
	// buildPrefix names the private directory a release is built in.
	buildPrefix = ".build-"
	// DefaultKeep is how many releases older than the live one are kept.
	DefaultKeep = 5
)

// Ops holds what the handlers use.
type Ops struct {
	// Open opens a site's tree; what it creates belongs to the site user.
	Open func(state.Site) (*sitefs.Root, error)
	// ReloadPHP reloads the site's PHP-FPM master, so OPcache drops the
	// code of the release that was live. Nil reloads nothing.
	ReloadPHP func(context.Context, state.Site) error
	// Keep is how many releases older than the live one survive a promote
	// (0 means DefaultKeep).
	Keep int
	// Workers bounds the parallel hashing and compression of a new release
	// (0 means one per CPU).
	Workers int
	Log     *slog.Logger
}

// Register wires this package's methods into the agent's RPC server.
func (o *Ops) Register(s *agentrpc.Server) {
	agentrpc.Handle(s, agentrpc.DeployRelease, o.Deploy)
	agentrpc.Handle(s, agentrpc.PromoteRelease, o.Promote)
	agentrpc.Handle(s, agentrpc.RollbackRelease, o.Rollback)
	agentrpc.Handle(s, agentrpc.CurrentRelease, o.CurrentRelease)
	agentrpc.Handle(s, agentrpc.RemoveReleases, o.RemoveReleases)
}

func (o *Ops) keep() int {
	if o.Keep > 0 {
		return o.Keep
	}
	return DefaultKeep
}

func (o *Ops) workers() int {
	if o.Workers > 0 {
		return o.Workers
	}
	return runtime.GOMAXPROCS(0)
}

func (o *Ops) log() *slog.Logger {
	if o.Log != nil {
		return o.Log
	}
	return slog.Default()
}

// rootOwned creates entries for root with the site's group: the site root,
// releases/ and the current link.
func rootOwned(r *sitefs.Root) *sitefs.Root {
	return r.WithOwner(sitefs.Owner{UID: 0, GID: r.Owner().GID})
}

// asRoot creates entries for root alone: the private build directory.
func asRoot(r *sitefs.Root) *sitefs.Root { return r.WithOwner(sitefs.Owner{}) }

// validName is a release a promote or rollback may name.
func validName(id string) bool { return id == site.InitialRelease || site.ValidReleaseID(id) }

// ParseLink reads the target of a current link: "releases/<id>", or the
// absolute form under root the layout used before R-2.3.
func ParseLink(target, root string) (string, bool) {
	t := target
	if path.IsAbs(t) {
		rel, ok := strings.CutPrefix(path.Clean(t), path.Clean(root)+"/")
		if !ok {
			return "", false
		}
		t = rel
	}
	id, ok := strings.CutPrefix(path.Clean(t), Dir+"/")
	if !ok || id == "" || id == "." || id == ".." || strings.Contains(id, "/") {
		return "", false
	}
	return id, true
}

// Live returns the site-relative path of the release current points at
// ("releases/<id>"), which must be a real directory.
func Live(r *sitefs.Root) (string, error) {
	t, err := r.Readlink(CurrentLink)
	if err != nil {
		return "", err
	}
	id, ok := ParseLink(t, r.Path())
	if !ok {
		return "", fmt.Errorf("the current release points outside the site's releases")
	}
	rel := Dir + "/" + id
	fi, err := r.Lstat(rel)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("the current release is not a directory")
	}
	return rel, nil
}

// LockSkeleton makes the parts of a site tree that must be root's root's,
// with the site's group (SPEC §1.1): the root itself (OpenSSH's chroot
// requires it), releases/ (a site user owning it could replace a release
// with a symlink and have current, and nginx's document root, point
// anywhere), and .ssh with everything in it. releases/ is created if
// missing.
func LockSkeleton(r *sitefs.Root) error {
	gid := r.Owner().GID
	fi, err := r.Lstat(Dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := rootOwned(r).Mkdir(Dir, 0o750); err != nil {
			return err
		}
	case err != nil:
		return err
	case !fi.IsDir():
		return fmt.Errorf("%s is not a real directory", Dir)
	}
	for _, rel := range []string{".", Dir} {
		if err := r.Chown(rel, 0, gid); err != nil {
			return err
		}
		if err := r.Chmod(rel, 0o750); err != nil {
			return err
		}
	}
	return LockSSH(r)
}

// SSHDir is the site's .ssh, the folder of sshd's key files.
const SSHDir = ".ssh"

// The modes of .ssh and of the key files in it (the sftp package writes the
// same).
const (
	SSHDirMode  = 0o750
	SSHFileMode = 0o640
)

// LockSSH makes .ssh, when it is a real folder, root:<site group> 0750, and
// every entry in it root:<site group> 0640 when it is a regular file with
// one link (SPEC §1.1). Anything else in it (a symlink, a folder, a FIFO, a
// hard link to a file elsewhere) is removed: sshd reads only regular files
// there, and only the agent writes them. A .ssh that is not a folder is
// left to the sftp package, which refuses it with a message.
func LockSSH(r *sitefs.Root) error {
	gid := r.Owner().GID
	fi, err := r.Lstat(SSHDir)
	if err != nil || !fi.IsDir() {
		return nil
	}
	if err := r.Chown(SSHDir, 0, gid); err != nil {
		return err
	}
	if err := r.Chmod(SSHDir, SSHDirMode); err != nil {
		return err
	}
	ents, err := r.ReadDir(SSHDir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		rel := SSHDir + "/" + e.Name()
		if !e.Mode().IsRegular() || nlink(e) != 1 {
			if err := r.RemoveAll(rel); err != nil {
				return err
			}
			continue
		}
		if err := r.Chown(rel, 0, gid); err != nil {
			return err
		}
		if err := r.Chmod(rel, SSHFileMode); err != nil {
			return err
		}
	}
	return nil
}

// nlink is an entry's link count (1 when unknown).
func nlink(fi fs.FileInfo) uint64 {
	if st, ok := fi.Sys().(*unix.Stat_t); ok {
		return uint64(st.Nlink)
	}
	return 1
}

// List returns the site's releases, oldest first, and the one current names
// ("" when there is no current link). Entries of releases/ that are not
// release names (a build in progress, anything else) are not releases.
func List(r *sitefs.Root) ([]string, string, error) {
	ents, err := r.ReadDir(Dir)
	if err != nil {
		return nil, "", err
	}
	var ids []string
	for _, e := range ents {
		if e.IsDir() && validName(e.Name()) {
			ids = append(ids, e.Name())
		}
	}
	sort.Slice(ids, func(i, j int) bool { return site.ReleaseBefore(ids[i], ids[j]) })
	cur := ""
	if t, err := r.Readlink(CurrentLink); err == nil {
		cur, _ = ParseLink(t, r.Path())
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, "", err
	}
	return ids, cur, nil
}

// previous is the newest release older than cur.
func previous(ids []string, cur string) (string, bool) {
	for i := len(ids) - 1; i >= 0; i-- {
		if site.ReleaseBefore(ids[i], cur) {
			return ids[i], true
		}
	}
	return "", false
}

// Prune removes build directories left by an interrupted deploy, and every
// release older than the live one but the newest keep of them. The live
// release and anything newer (built, not promoted yet, or rolled back
// from) are kept. It returns the releases removed.
func Prune(r *sitefs.Root, keep int) ([]string, error) {
	if err := clearBuilds(r); err != nil {
		return nil, err
	}
	ids, cur, err := List(r)
	if err != nil || cur == "" {
		return nil, err
	}
	var older []string
	for _, id := range ids {
		if site.ReleaseBefore(id, cur) {
			older = append(older, id)
		}
	}
	if len(older) <= keep {
		return nil, nil
	}
	var removed []string
	for _, id := range older[:len(older)-keep] {
		if err := asRoot(r).RemoveAll(Dir + "/" + id); err != nil {
			return removed, fmt.Errorf("remove release %s: %w", id, err)
		}
		removed = append(removed, id)
	}
	return removed, nil
}

// clearBuilds removes the private build directories under releases/. The
// caller holds the site's lock, so none of them is in use.
func clearBuilds(r *sitefs.Root) error {
	ents, err := r.ReadDir(Dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), buildPrefix) {
			if err := asRoot(r).RemoveAll(Dir + "/" + e.Name()); err != nil {
				return fmt.Errorf("remove an interrupted build: %w", err)
			}
		}
	}
	return nil
}

// Promote points current at a release and reloads the site's PHP, then
// prunes the releases beyond Keep. The server holds the site's lock
// (LockLiveSite).
func (o *Ops) Promote(ctx context.Context, p rpc.ReleaseParams) (map[string]string, error) {
	if !validName(p.ReleaseID) {
		return nil, agentrpc.Invalid("invalid release id %q", p.ReleaseID)
	}
	r, err := o.Open(p.Site)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	if err := LockSkeleton(r); err != nil {
		return nil, err
	}
	return o.activate(ctx, r, p.Site, p.ReleaseID)
}

// Rollback points current at the newest release older than the live one,
// or at the one named. The server holds the site's lock (LockLiveSite).
func (o *Ops) Rollback(ctx context.Context, p rpc.ReleaseParams) (map[string]string, error) {
	if p.ReleaseID != "" {
		return o.Promote(ctx, p)
	}
	r, err := o.Open(p.Site)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	if err := LockSkeleton(r); err != nil {
		return nil, err
	}
	ids, cur, err := List(r)
	if err != nil {
		return nil, err
	}
	if cur == "" {
		return nil, agentrpc.Conflict("the site has no current release")
	}
	target, ok := previous(ids, cur)
	if !ok {
		return nil, agentrpc.Conflict("no release older than %s to roll back to", cur)
	}
	return o.activate(ctx, r, p.Site, target)
}

// activate swaps current to releases/<id> (a new link renamed over the old
// one, so every request sees one release or the other), reloads PHP and
// prunes.
func (o *Ops) activate(ctx context.Context, r *sitefs.Root, s state.Site, id string) (map[string]string, error) {
	// Lstat: a release must be a real directory. A symlink in its place
	// would make current, and nginx's document root, point wherever it
	// pointed.
	rel := Dir + "/" + id
	if fi, err := r.Lstat(rel); err != nil || !fi.IsDir() {
		return nil, agentrpc.NotFound("release %s not found", id)
	}
	if err := rootOwned(r).Symlink(rel, CurrentLink); err != nil {
		return nil, err
	}
	if s.PHPVersion != "" && o.ReloadPHP != nil {
		if err := o.ReloadPHP(ctx, s); err != nil {
			return nil, err
		}
	}
	removed, err := Prune(r, o.keep())
	if err != nil {
		o.log().Warn("pruning old releases failed", "site", s.Domain, "err", err)
	} else if len(removed) > 0 {
		o.log().Info("pruned old releases", "site", s.Domain, "removed", removed)
	}
	return map[string]string{"current": id}, nil
}
