package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strconv"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

// TransientDirs are the directories of a site's root that its backups leave
// out (SPEC §4): tmp/ is PHP's upload_tmp_dir and holds its sessions
// (tmp/sessions), files PHP creates and deletes at any moment, which a site
// needs only while they last. One vanishing while restic copied the tree
// failed the whole backup (restic's exit 3). A restore makes them again,
// empty (sessionsDir).
var TransientDirs = []string{"tmp"}

// sessionsDir is the directory PHP keeps the site's sessions in
// (session.save_path, internal/phpfpm): the restore recreates it.
const sessionsDir = "tmp/sessions"

// RunBackup snapshots a site's database, then its files (SPEC §4).
func (o *Ops) RunBackup(ctx context.Context, p rpc.BackupParams) (rpc.BackupResult, error) {
	repo, err := o.repoOf(ctx, p.Repository, p.Password, p.Env)
	if err != nil {
		return rpc.BackupResult{}, err
	}
	kind := p.Kind
	if kind == "" {
		kind = "full"
	}
	if kind != "full" && kind != "files" && kind != "database" {
		return rpc.BackupResult{}, agentrpc.Invalid("backup kind must be full, files, or database")
	}
	var databases []string
	if kind != "files" {
		if databases, err = o.siteDatabases(ctx, p.Site); err != nil {
			return rpc.BackupResult{}, err
		}
	}
	if err := o.ensureRepo(ctx, repo); err != nil {
		return rpc.BackupResult{}, err
	}
	unlock, err := o.lock(ctx, repo.URL, false)
	if err != nil {
		return rpc.BackupResult{}, err
	}
	defer unlock()

	tags := []string{"--tag", tagSite + p.Site.Domain, "--tag", tagKind + kind}
	// The site's description first (SPEC §4): what another server needs to
	// create the site again before restoring it.
	// Its databases are the ones this backup holds.
	manifest, err := o.manifestOf(ctx, p.Site, databases)
	if err != nil {
		return rpc.BackupResult{}, err
	}
	msum, err := o.backupManifest(ctx, repo, manifest, tags)
	if err != nil {
		return rpc.BackupResult{}, err
	}
	// Every database of the site, each in a snapshot of its own; one that
	// fails takes the ones made before it along.
	var dbSums []host.BackupSummary
	forgetDBs := func() {
		for _, s := range dbSums {
			o.forget(ctx, repo, s.SnapshotID)
		}
		o.forget(ctx, repo, msum.SnapshotID)
	}
	var dbBytes int64
	for _, name := range databases {
		sum, err := o.backupDatabase(ctx, repo, name, tags)
		if err != nil {
			forgetDBs()
			return rpc.BackupResult{}, fmt.Errorf("%s: %w", name, err)
		}
		dbSums = append(dbSums, sum)
		dbBytes += sum.TotalBytesProcessed
	}
	args := append([]string{"backup", "--json", "--quiet"}, tags...)
	for _, s := range dbSums {
		args = append(args, "--tag", tagDB+s.SnapshotID)
	}
	args = append(args, "--tag", tagManifest+msum.SnapshotID)
	for _, d := range TransientDirs {
		args = append(args, "--exclude", path.Join(p.Site.RootPath, d))
	}
	// The walk of the tenant's tree runs where only that tree is visible
	// (SPEC §4).
	out, err := o.runSandboxed(ctx, repo, p.Site.RootPath, append(args, p.Site.RootPath)...)
	sum, sumErr := host.DecodeBackupSummary(out)
	if err == nil && sumErr != nil {
		err = sumErr
	}
	if err != nil {
		// Nothing half-made stays: the database snapshot nothing names, and
		// a files snapshot restic made of a tree it could not read whole.
		if sumErr == nil {
			o.forget(ctx, repo, sum.SnapshotID)
		}
		forgetDBs()
		return rpc.BackupResult{}, fmt.Errorf("back up the files: %w", err)
	}
	if !ValidSnapshotID(sum.SnapshotID) {
		return rpc.BackupResult{}, fmt.Errorf("restic reported an invalid snapshot id")
	}
	if databases == nil {
		databases = []string{}
	}
	return rpc.BackupResult{SnapshotID: sum.SnapshotID, SizeBytes: sum.TotalBytesProcessed + dbBytes, Databases: databases}, nil
}

// backupDatabase streams the database's dump into a snapshot of its own,
// through a pipe: it never touches a disk, least of all the site's tree.
func (o *Ops) backupDatabase(ctx context.Context, repo host.Repo, database string, tags []string) (host.BackupSummary, error) {
	// How many tables the database has now: the restore test requires the
	// dump to bring back as many (an empty database is a valid backup of an
	// empty database, and of nothing else).
	tables, err := o.DB.TableNames(ctx, database)
	if err != nil {
		return host.BackupSummary{}, fmt.Errorf("count the tables of %s: %w", database, err)
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return host.BackupSummary{}, err
	}
	dumped := make(chan error, 1)
	go func() {
		dumped <- o.DB.Dump(ctx, database, pw)
		pw.Close()
	}()
	args := append(append([]string{"backup", "--stdin", "--stdin-filename", DumpName}, tags...),
		"--tag", tagDatabase, "--tag", tagDBName+database, "--tag", tagTables+strconv.Itoa(len(tables)), "--json", "--quiet")
	out := host.NewResticOutput(args)
	resticErr := o.runIO(ctx, repo, dataTimeout, pr, out, args...)
	if resticErr == nil {
		resticErr = out.Err()
	}
	// A dump restic stopped reading gets EPIPE instead of blocking forever.
	pr.Close()
	dumpErr := <-dumped
	sum, sumErr := host.DecodeBackupSummary(out.Bytes())
	if dumpErr != nil {
		// restic stored what arrived before the dump died: not a database,
		// and nothing must ever restore it as one.
		if sumErr == nil {
			o.forget(ctx, repo, sum.SnapshotID)
		}
		return host.BackupSummary{}, fmt.Errorf("database dump: %w", errors.Join(dumpErr, resticErr))
	}
	if resticErr != nil {
		if sumErr == nil {
			o.forget(ctx, repo, sum.SnapshotID)
		}
		return host.BackupSummary{}, fmt.Errorf("back up the database: %w", resticErr)
	}
	if sumErr != nil {
		return host.BackupSummary{}, fmt.Errorf("back up the database: %w", sumErr)
	}
	if !ValidSnapshotID(sum.SnapshotID) {
		return host.BackupSummary{}, fmt.Errorf("restic reported an invalid snapshot id")
	}
	return sum, nil
}
