// Package backup takes a site's backups into a restic repository, restores
// them, restore-tests them and applies their retention. SPEC.md in this
// directory is the contract.
package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/db"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/backuprepo"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Bounds of each restic call (SPEC §2).
const (
	metaTimeout   = 2 * time.Minute
	dataTimeout   = 6 * time.Hour
	planTimeout   = 10 * time.Minute
	pruneTimeout  = 3 * time.Hour
	forgetTimeout = 2 * time.Minute
)

// DumpName is the database dump's file name inside its snapshot.
const DumpName = "database.sql"

// Tags.
const (
	tagSite     = "site:"
	tagKind     = "kind:"
	tagDB       = "db:"
	tagDatabase = "part:database"
)

var snapshotIDRe = regexp.MustCompile(`^[a-fA-F0-9]{8,64}$`)

// ValidSnapshotID reports whether id is a restic snapshot ID, full or short.
func ValidSnapshotID(id string) bool { return snapshotIDRe.MatchString(id) }

// Ops holds what the handlers use.
type Ops struct {
	Restic host.Restic
	// DB dumps, replaces and restore-tests the sites' databases.
	DB *db.Ops
	// Owner resolves a site user to the owner its files get.
	Owner func(systemUser string) (sitefs.Owner, error)
	// Relink rewrites a restored tree's absolute layout links (from before
	// R-2.3) as relative ones; siteRoot is the path they were made against.
	// Nil rewrites nothing.
	Relink func(r *sitefs.Root, siteRoot string) error
	// RestartPHP restarts a site's PHP-FPM master; RefreshPHP makes a change
	// to its database visible (flushes its object cache). Nil does nothing.
	RestartPHP func(context.Context, state.Site) error
	RefreshPHP func(context.Context, state.Site) error
	// ReloadNginx reloads the web server. Nil does nothing.
	ReloadNginx func() error
	// PurgeCache empties a site's page cache, keeping its root. Nil does
	// nothing.
	PurgeCache func(state.Site) error
	// RepoRules are the options every repository is checked with.
	RepoRules backuprepo.Options
	// CacheDir is restic's cache (RESTIC_CACHE_DIR), bound into the
	// sandboxed files backup.
	CacheDir string
	// WorkDir is root's own scratch directory.
	WorkDir string
	// ArchiveDir holds the downloadable archives of backups (SPEC §12); ""
	// is DefaultArchiveDir.
	ArchiveDir string
	// FreeSpace reports the bytes free for root in dir; nil reads statfs
	// (tests replace it).
	FreeSpace func(dir string) (int64, error)
	// Hostname scopes retention to this server; nil is os.Hostname.
	Hostname func() (string, error)
	Log      *slog.Logger

	locks sync.Map // repository URL -> *repoLock
}

// Register wires this package's methods into the agent's RPC server.
func (o *Ops) Register(s *agentrpc.Server) {
	agentrpc.Handle(s, agentrpc.RunBackup, o.RunBackup)
	agentrpc.Handle(s, agentrpc.RestoreSnapshot, o.RestoreSnapshot)
	agentrpc.Handle(s, agentrpc.VerifyBackup, o.VerifyBackup)
	agentrpc.Handle(s, agentrpc.TestBackup, o.TestBackup)
	agentrpc.Handle(s, agentrpc.BackupRetention, o.BackupRetention)
	agentrpc.Handle(s, agentrpc.ForgetSnapshot, o.ForgetSnapshot)
	agentrpc.Handle(s, agentrpc.PruneRepository, o.PruneRepository)
	agentrpc.Handle(s, agentrpc.ArchiveSnapshot, o.ArchiveSnapshot)
	agentrpc.Handle(s, agentrpc.ReadArchiveChunk, o.ReadArchiveChunk)
	agentrpc.Handle(s, agentrpc.DeleteArchive, o.DeleteArchive)
	agentrpc.Handle(s, agentrpc.RepositoryInfo, o.RepositoryInfo)
	agentrpc.Handle(s, agentrpc.CopyRepository, o.CopyRepository)
}

func (o *Ops) log() *slog.Logger {
	if o.Log != nil {
		return o.Log
	}
	return slog.Default()
}

// repoOf checks a request's repository (internal/backuprepo, SPEC §2)
// before restic ever sees it.
func (o *Ops) repoOf(ctx context.Context, url, password string, env map[string]string) (host.Repo, error) {
	if url == "" || password == "" {
		return host.Repo{}, agentrpc.Invalid("repository and password required")
	}
	if err := backuprepo.Validate(ctx, url, o.RepoRules); err != nil {
		var re *backuprepo.Error
		if errors.As(err, &re) {
			return host.Repo{}, agentrpc.Invalid("%s", re.Msg)
		}
		return host.Repo{}, err
	}
	// The panel's SFTP key travels with the credentials but is no variable
	// of restic's: it becomes the repository's key (SPEC §2).
	key := env[EnvSFTPKey]
	if key != "" {
		rest := make(map[string]string, len(env))
		for k, v := range env {
			if k != EnvSFTPKey {
				rest[k] = v
			}
		}
		env = rest
	}
	return host.Repo{URL: url, Password: password, Env: env, SSHKey: key}, nil
}

// EnvSFTPKey is the credential the panel sends an SFTP repository's private
// key under.
const EnvSFTPKey = "CG_SFTP_KEY"

// run is one bounded restic call.
func (o *Ops) run(ctx context.Context, repo host.Repo, d time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	out, err := o.Restic.Run(ctx, repo, args...)
	return out, repoError(err)
}

// runSandboxed is one bounded restic call that walks the tree root (a files
// backup), in a namespace that shows only root and restic's own paths.
func (o *Ops) runSandboxed(ctx context.Context, repo host.Repo, root string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, dataTimeout)
	defer cancel()
	sb := host.TreeSandbox{Tree: root, Writable: o.SandboxWritable(repo.URL)}
	out, err := o.Restic.RunSandboxed(ctx, repo, sb, args...)
	return out, repoError(err)
}

// SandboxWritable is what a sandboxed restic run writes: its cache and, for
// a local repository, the repository.
func (o *Ops) SandboxWritable(url string) []string {
	var w []string
	if o.CacheDir != "" && os.MkdirAll(o.CacheDir, 0o700) == nil {
		w = append(w, o.CacheDir)
	}
	if strings.HasPrefix(url, "/") {
		w = append(w, url)
	}
	return w
}

// runIO is one bounded restic call with its standard streams.
func (o *Ops) runIO(ctx context.Context, repo host.Repo, d time.Duration, in io.Reader, out io.Writer, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return repoError(o.Restic.RunIO(ctx, repo, in, out, args...))
}

// repoError gives restic's typed failures a message a user may read; the
// rest keep the method's own (SPEC §2).
func repoError(err error) error {
	var code agentrpc.Code
	var msg string
	switch {
	case err == nil:
		return nil
	case errors.Is(err, host.ErrWrongPassword):
		code, msg = agentrpc.CodeInvalid, "the backup repository password is wrong"
	case errors.Is(err, host.ErrRepoLocked):
		code, msg = agentrpc.CodeConflict, "the backup repository is locked by another restic process"
	case errors.Is(err, host.ErrRepoNotFound):
		code, msg = agentrpc.CodeNotFound, "the backup repository does not exist"
	default:
		return err
	}
	return &resticError{safe: agentrpc.Wrap(code, msg, err), cause: err}
}

// resticError is a typed restic failure: its text is the user-safe message,
// and errors.Is still finds restic's kind.
type resticError struct {
	safe  error // an *agentrpc.Error
	cause error
}

func (e *resticError) Error() string   { return e.safe.Error() }
func (e *resticError) Unwrap() []error { return []error{e.safe, e.cause} }

// forget removes a snapshot nothing must use, on its own short deadline: it
// runs because the request failed, possibly by being cancelled.
func (o *Ops) forget(ctx context.Context, repo host.Repo, id string) {
	if !ValidSnapshotID(id) {
		return
	}
	ctx = context.WithoutCancel(ctx)
	if _, err := o.run(ctx, repo, forgetTimeout, "forget", id); err != nil {
		o.log().Warn("forget an unusable snapshot", "snapshot", id, "err", agentrpc.RedactCredentials(err.Error()))
	}
}

// ensureRepo initialises the repository on first use (SPEC §3).
func (o *Ops) ensureRepo(ctx context.Context, repo host.Repo) error {
	unlock, err := o.lock(ctx, repo.URL, true)
	if err != nil {
		return err
	}
	defer unlock()
	_, readErr := o.run(ctx, repo, metaTimeout, "cat", "config")
	if readErr == nil {
		return nil
	}
	if errors.Is(readErr, host.ErrWrongPassword) || ctx.Err() != nil {
		return readErr
	}
	if _, err := o.run(ctx, repo, metaTimeout, "init"); err != nil {
		if errors.Is(readErr, host.ErrRepoNotFound) {
			return fmt.Errorf("initialise the repository: %w", err)
		}
		return errors.Join(readErr, fmt.Errorf("initialise the repository: %w", err))
	}
	return nil
}

// snapshot reads one snapshot's metadata. One restic does not know by that
// ID is a *missingSnapshot.
func (o *Ops) snapshot(ctx context.Context, repo host.Repo, id string) (host.Snapshot, error) {
	out, err := o.run(ctx, repo, metaTimeout, "snapshots", "--json", id)
	if err != nil {
		return host.Snapshot{}, err
	}
	snaps, err := host.DecodeSnapshots(out)
	if err != nil {
		return host.Snapshot{}, err
	}
	if len(snaps) != 1 {
		return host.Snapshot{}, &missingSnapshot{agentrpc.NotFound("snapshot %s not found", id)}
	}
	return snaps[0], nil
}

// missingSnapshot is a snapshot ID restic lists nothing for (not a missing
// repository, which is not_found too).
type missingSnapshot struct{ err error }

func (e *missingSnapshot) Error() string { return e.err.Error() }
func (e *missingSnapshot) Unwrap() error { return e.err }

// siteSnaps resolves the IDs of one call against a repository for site
// (SPEC §5): by restic's own ID first, then, for a snapshot copied by
// `restic copy` (which gives it a new ID), by its original ID among the
// site's snapshots, listed once.
type siteSnaps struct {
	o    *Ops
	repo host.Repo
	site state.Site
	list []host.Snapshot
	read bool
}

func (o *Ops) siteSnaps(repo host.Repo, s state.Site) *siteSnaps {
	return &siteSnaps{o: o, repo: repo, site: s}
}

// find is the snapshot id names: its own, or the copy of it.
func (l *siteSnaps) find(ctx context.Context, id string) (host.Snapshot, error) {
	snap, err := l.o.snapshot(ctx, l.repo, id)
	var missing *missingSnapshot
	if err == nil || !errors.As(err, &missing) {
		return snap, err
	}
	if !l.read {
		out, lerr := l.o.run(ctx, l.repo, metaTimeout, "snapshots", "--json", "--tag", tagSite+l.site.Domain)
		if lerr != nil {
			return host.Snapshot{}, lerr
		}
		if l.list, lerr = host.DecodeSnapshots(out); lerr != nil {
			return host.Snapshot{}, lerr
		}
		l.read = true
	}
	found, ferr := matchOriginal(l.list, id)
	if ferr != nil {
		return host.Snapshot{}, ferr
	}
	if found == nil {
		return host.Snapshot{}, err
	}
	return *found, nil
}

// matchOriginal is the snapshot of snaps whose original ID is id (either
// may be a prefix of the other); nil for none, invalid for two.
func matchOriginal(snaps []host.Snapshot, id string) (*host.Snapshot, error) {
	var found *host.Snapshot
	for i := range snaps {
		if snaps[i].Original == "" || !ValidSnapshotID(snaps[i].ID) || !protected(snaps[i].Original, []string{id}) {
			continue
		}
		if found != nil {
			return nil, agentrpc.Invalid("snapshot id %s is ambiguous", id)
		}
		found = &snaps[i]
	}
	return found, nil
}

// names reports whether snap is the snapshot id names: by its ID or by the
// ID it had before a copy (SPEC §5).
func names(snap host.Snapshot, id string) bool {
	return snap.ID != "" && protected(snap.ID, []string{id}) || snap.Original != "" && protected(snap.Original, []string{id})
}

// hostname is this server's name, as restic records it in a snapshot.
func (o *Ops) hostname() (string, error) {
	hostname := o.Hostname
	if hostname == nil {
		hostname = os.Hostname
	}
	hn, err := hostname()
	if err == nil && hn == "" {
		err = errors.New("empty hostname")
	}
	return hn, err
}

// ours reports why snap is not a backup of site s made on this server, or
// "" when it is: tagged site:<domain> and recorded with this host's name
// (SR-T6). A repository may hold other sites' snapshots, and other
// servers' (of a site with the same domain).
func (o *Ops) ours(snap host.Snapshot, s state.Site) string {
	if !slices.Contains(snap.Tags, tagSite+s.Domain) {
		return "it is not a backup of " + s.Domain
	}
	hn, err := o.hostname()
	if err != nil {
		return "this server's hostname cannot be read: " + err.Error()
	}
	if snap.Hostname != hn {
		return fmt.Sprintf("it was made on %q, not on this server (%q)", snap.Hostname, hn)
	}
	return ""
}

// siteSnapshot reads the file snapshot id (resolved by l, SPEC §5) and
// requires it to be one of the site's own, made on this server, and not a
// database part (SR-T6). Before, a files restore swapped in whatever tree
// the ID named. The snapshot returned carries restic's own ID, which every
// later call names.
func (o *Ops) siteSnapshot(ctx context.Context, l *siteSnaps, id string) (host.Snapshot, error) {
	snap, err := l.find(ctx, id)
	if err != nil {
		return host.Snapshot{}, err
	}
	if why := o.ours(snap, l.site); why != "" {
		return host.Snapshot{}, agentrpc.Invalid("snapshot %s cannot be restored here: %s", id, why)
	}
	if slices.Contains(snap.Tags, tagDatabase) {
		return host.Snapshot{}, agentrpc.Invalid("snapshot %s is a database part, not a site backup", id)
	}
	if !ValidSnapshotID(snap.ID) {
		return host.Snapshot{}, fmt.Errorf("the repository lists an invalid snapshot id")
	}
	return snap, nil
}

// fetchDump writes a database snapshot's dump into a new root-only file at
// path and returns it.
func (o *Ops) fetchDump(ctx context.Context, repo host.Repo, dbID, path string) (string, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err := o.runIO(ctx, repo, dataTimeout, nil, f, "dump", dbID, "/"+DumpName); err != nil {
		return "", fmt.Errorf("read the database snapshot: %w", err)
	}
	return path, f.Sync()
}

// workDir makes a fresh root-only directory under WorkDir.
func (o *Ops) workDir(prefix string) (string, error) {
	if err := os.MkdirAll(o.WorkDir, 0o700); err != nil {
		return "", err
	}
	return os.MkdirTemp(o.WorkDir, prefix)
}
