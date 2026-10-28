package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/db"
	"github.com/cloudground-it/cloudground/internal/agent/ops/release"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// filesDir is where a snapshot's files are restored inside a work directory.
const filesDir = "files"

// RestoreSnapshot restores a snapshot over the live site: its files, its
// database or both (SPEC §5). The server holds the site's lock.
func (o *Ops) RestoreSnapshot(ctx context.Context, p rpc.RestoreParams) (map[string]string, error) {
	// A database on an external server is imported as the site's own account
	// with the password the panel sent; a fallback collects a warning.
	ctx = db.WithImportAccounts(ctx, p.Accounts)
	ctx, imported := db.WithImportWarnings(ctx)
	mode := strings.ToLower(strings.TrimSpace(p.Mode))
	if mode == "" {
		mode = "full"
	}
	if mode != "full" && mode != "files" && mode != "database" {
		return nil, agentrpc.Invalid("restore mode must be full, files, or database")
	}
	if !ValidSnapshotID(p.SnapshotID) {
		return nil, agentrpc.Invalid("invalid snapshot id")
	}
	repo, err := o.repoOf(ctx, p.Repository, p.Password, p.Env)
	if err != nil {
		return nil, err
	}
	s := p.Site
	if mode == "files" && len(p.Databases) > 0 {
		return nil, agentrpc.Invalid("a files restore puts back no database")
	}
	if len(p.Paths) > 0 && mode != "files" {
		return nil, agentrpc.Invalid("only a files restore takes paths")
	}
	if len(p.Paths) > rpc.MaxRestorePaths {
		return nil, agentrpc.Invalid("at most %d paths", rpc.MaxRestorePaths)
	}
	databases, err := o.siteDatabases(ctx, s)
	if err != nil {
		return nil, err
	}
	withDB := mode == "database" || mode == "full" && len(databases) > 0
	if mode == "database" && len(databases) == 0 {
		return nil, agentrpc.Invalid("site has no managed database to restore")
	}

	work, err := o.workDir("restore-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	unlock, err := o.lock(ctx, repo.URL, false)
	if err != nil {
		return nil, err
	}
	defer unlock()

	files, err := o.siteSnapshot(ctx, repo, s, p.SnapshotID)
	if err != nil {
		return nil, err
	}
	var dumps []dumpFile
	var skipped []string
	if withDB {
		parts, err := o.databaseParts(ctx, repo, s, files)
		if err != nil {
			return nil, err
		}
		if len(parts) == 0 {
			return nil, agentrpc.Invalid("snapshot %s has no database backup of its own (it predates them, and the dump inside the site's files is not trusted); restore it in files mode", p.SnapshotID)
		}
		var keep []dbPart
		if keep, skipped, err = o.restorePlan(ctx, s, parts, p.Databases); err != nil {
			return nil, err
		}
		if mode == "database" && len(keep) == 0 {
			return nil, agentrpc.Invalid("none of the databases in snapshot %s is still a database of %s", p.SnapshotID, s.Domain)
		}
		if dumps, err = o.fetchDumps(ctx, repo, keep, work); err != nil {
			return nil, err
		}
	}
	done := map[string]string{"restored": p.SnapshotID, "target": s.RootPath, "mode": mode}
	finish := func() map[string]string {
		if msgs := imported.Messages(); len(msgs) > 0 {
			done["warning"] = strings.Join(msgs, "; ")
		}
		return done
	}
	if len(dumps) > 0 {
		names := make([]string, len(dumps))
		for i, d := range dumps {
			names[i] = d.Name
		}
		done["databases"] = strings.Join(names, ",")
	}
	if len(skipped) > 0 {
		done["skipped"] = strings.Join(skipped, ",")
	}

	if mode == "database" {
		txs, err := o.replaceAll(ctx, dumps)
		if err != nil {
			return nil, err
		}
		txs.commit()
		// The database changed under the running site: what its PHP holds
		// (the object cache in APCu, above all) is from before.
		if o.RefreshPHP != nil {
			if err := o.RefreshPHP(ctx, s); err != nil {
				o.log().Warn("refresh PHP after a database restore", "site", s.Domain, "err", err)
			}
		}
		o.purge(s)
		done["target"] = done["databases"]
		return finish(), nil
	}

	if len(p.Paths) > 0 {
		if err := o.restorePaths(ctx, repo, s, p.SnapshotID, work, p.Paths); err != nil {
			return nil, err
		}
		done["paths"] = strings.Join(p.Paths, ",")
		return done, nil
	}
	if err := o.restoreFiles(ctx, repo, p.SnapshotID, work); err != nil {
		return nil, err
	}
	if err := o.swapIn(ctx, s, work, dumps); err != nil {
		return nil, err
	}
	return finish(), nil
}

// restoreFiles restores a snapshot's files into work/files.
func (o *Ops) restoreFiles(ctx context.Context, repo host.Repo, id, work string) error {
	if _, err := o.run(ctx, repo, dataTimeout, "restore", id, "--target", filepath.Join(work, filesDir)); err != nil {
		return fmt.Errorf("restore the snapshot's files: %w", err)
	}
	return nil
}

// checkRestored finds the site's root in a restored tree and checks it can
// go live: a directory whose current link names one of its releases. It
// returns the root's path relative to the tree. The restore test runs the
// same check (SPEC §6).
func checkRestored(tree *sitefs.Root, siteRoot string) (string, error) {
	rel := strings.TrimPrefix(path.Clean(siteRoot), "/")
	if fi, err := tree.Lstat(rel); err != nil || !fi.IsDir() {
		return "", agentrpc.Invalid("the snapshot does not contain the site's files")
	}
	t, err := tree.Readlink(rel + "/" + release.CurrentLink)
	if err != nil {
		return "", agentrpc.Invalid("the snapshot has no valid current release")
	}
	id, ok := release.ParseLink(t, siteRoot)
	if !ok {
		return "", agentrpc.Invalid("the snapshot has no valid current release")
	}
	if fi, err := tree.Lstat(rel + "/" + release.Dir + "/" + id); err != nil || !fi.IsDir() {
		return "", agentrpc.Invalid("the snapshot's current release %s is missing", id)
	}
	return rel, nil
}

// swapIn puts the restored tree in work/files live in place of the site's
// root, importing each dump into its database on the way, and undoes all
// of it if the site does not come up.
func (o *Ops) swapIn(ctx context.Context, s state.Site, work string, dumps []dumpFile) (err error) {
	tree, err := sitefs.Open(filepath.Join(work, filesDir), sitefs.Owner{})
	if err != nil {
		return err
	}
	defer tree.Close()
	rel, err := checkRestored(tree, s.RootPath)
	if err != nil {
		return err
	}
	owner, err := o.Owner(s.SystemUser)
	if err != nil {
		return err
	}

	// Staged beside the live root, on its filesystem, so the swap is one
	// rename. A dot-name: no domain starts with one.
	parentPath, base := filepath.Dir(s.RootPath), filepath.Base(s.RootPath)
	stage, err := os.MkdirTemp(parentPath, ".cg-restore-")
	if err != nil {
		return err
	}
	keepStage := false
	defer func() {
		if !keepStage {
			os.RemoveAll(stage)
		}
	}()
	staged := filepath.Base(stage) + "/new"
	keys, err := liveKeys(s, owner)
	if err != nil {
		return fmt.Errorf("read the site's SSH keys: %w", err)
	}
	if err := o.prepare(tree, rel, stage, owner, s.RootPath, keys); err != nil {
		return fmt.Errorf("prepare the restored files: %w", err)
	}

	txs, err := o.replaceAll(ctx, dumps)
	if err != nil {
		return err
	}
	rollbackDB := txs.rollback

	parent, err := sitefs.Open(parentPath, sitefs.Owner{})
	if err != nil {
		return rollbackDB(err)
	}
	defer parent.Close()
	if err := parent.Rename(staged, base, sitefs.Exchange); err != nil {
		return rollbackDB(fmt.Errorf("swap the restored files in: %w", err))
	}

	// From here the restored tree is live; the old one is at stage/new.
	bctx := context.WithoutCancel(ctx)
	undo := func(cause error) error {
		if err := parent.Rename(staged, base, sitefs.Exchange); err != nil {
			keepStage = true
			cause = fmt.Errorf("%w; putting the original files back ALSO failed (%v): they are at %s", cause, err, filepath.Join(stage, "new"))
		}
		cause = rollbackDB(cause)
		// The PHP master must bind the directory now at the root's path.
		if o.RestartPHP != nil {
			if err := o.RestartPHP(bctx, s); err != nil {
				o.log().Warn("restart PHP after undoing a restore", "site", s.Domain, "err", err)
			}
		}
		if o.ReloadNginx != nil {
			_ = o.ReloadNginx()
		}
		return cause
	}
	// A restart, not a reload: the master's mount namespace binds the
	// directory that was at the root's path when it started, the old tree;
	// a restart also empties OPcache and APCu.
	if o.RestartPHP != nil {
		if err := o.RestartPHP(ctx, s); err != nil {
			return undo(fmt.Errorf("restart PHP after the restore: %w", err))
		}
	}
	if o.ReloadNginx != nil {
		if err := o.ReloadNginx(); err != nil {
			return undo(fmt.Errorf("reload nginx after the restore: %w", err))
		}
	}
	if err := liveRelease(s, owner); err != nil {
		return undo(fmt.Errorf("the restored site has no live release: %w", err))
	}
	txs.commit()
	o.purge(s)
	return nil
}

// keyFiles are the key files of a site's .ssh that a restore carries over
// from the live site (SPEC §5, step 3): the panel's key list is what the live
// files hold, never what a snapshot held.
var keyFiles = []string{".ssh/authorized_keys", ".ssh/shell_keys"}

// maxKeyFile bounds a key file read (the sftp package's limit).
const maxKeyFile = 1 << 20

// sshKeys is the live site's .ssh: whether it is a folder and the key files
// it holds (a missing one is absent from files).
type sshKeys struct {
	dir   bool
	files map[string][]byte
}

// liveKeys reads the live site's key files, never following a link.
func liveKeys(s state.Site, owner sitefs.Owner) (sshKeys, error) {
	k := sshKeys{files: map[string][]byte{}}
	r, err := sitefs.Open(s.RootPath, owner)
	if err != nil {
		return k, err
	}
	defer r.Close()
	fi, err := r.Lstat(release.SSHDir)
	if err != nil || !fi.IsDir() {
		return k, nil
	}
	k.dir = true
	for _, f := range keyFiles {
		fi, err := r.Lstat(f)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		b, err := r.ReadFile(f, maxKeyFile)
		if err != nil {
			return k, err
		}
		k.files[f] = b
	}
	return k, nil
}

// putKeys replaces the restored tree's .ssh with the live site's keys: the
// snapshot's .ssh, which may hold keys revoked since, is removed whole.
func putKeys(nr *sitefs.Root, k sshKeys) error {
	if err := nr.RemoveAll(release.SSHDir); err != nil {
		return err
	}
	if !k.dir {
		return nil
	}
	rr := nr.WithOwner(sitefs.Owner{UID: 0, GID: nr.Owner().GID})
	if err := rr.Mkdir(release.SSHDir, release.SSHDirMode); err != nil {
		return err
	}
	for _, f := range keyFiles {
		b, ok := k.files[f]
		if !ok {
			continue
		}
		if err := rr.Replace(f, release.SSHFileMode, func(w *os.File) error {
			_, err := w.Write(b)
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}

// prepare copies the restored root (rel in tree) to stage/new, the site
// user's, with the layout's links made relative, the live site's SSH keys in
// place of the snapshot's and the skeleton root's.
func (o *Ops) prepare(tree *sitefs.Root, rel, stage string, owner sitefs.Owner, siteRoot string, keys sshKeys) error {
	st, err := sitefs.Open(stage, owner)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := sitefs.CopyTree(tree, rel, st, "new"); err != nil {
		return err
	}
	nr, err := st.Sub("new")
	if err != nil {
		return err
	}
	defer nr.Close()
	if o.Relink != nil {
		if err := o.Relink(nr, siteRoot); err != nil {
			return err
		}
	}
	// Backups leave tmp/ out (TransientDirs): PHP's sessions directory is
	// made again, the site user's, as a new site has it.
	if err := nr.MkdirAll(sessionsDir, 0o750); err != nil {
		return err
	}
	if err := putKeys(nr, keys); err != nil {
		return fmt.Errorf("put the site's SSH keys back: %w", err)
	}
	return release.LockSkeleton(nr)
}

// liveRelease checks the site's current link names one of its releases.
func liveRelease(s state.Site, owner sitefs.Owner) error {
	r, err := sitefs.Open(s.RootPath, owner)
	if err != nil {
		return err
	}
	defer r.Close()
	_, err = release.Live(r)
	return err
}

// purge empties the site's page cache, keeping its root (SPEC §5.8).
func (o *Ops) purge(s state.Site) {
	if o.PurgeCache == nil {
		return
	}
	if err := o.PurgeCache(s); err != nil && !errors.Is(err, os.ErrNotExist) {
		o.log().Warn("purge the page cache after a restore", "site", s.Domain, "err", err)
	}
}
