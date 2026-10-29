package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// VerifyBackup is the restore test (SPEC §6): the snapshot is restored into a
// scratch directory and put through the restore's own checks, its database
// is imported into a throwaway database, and the time it took is the
// recovery estimate. A backup that passes can be restored.
func (o *Ops) VerifyBackup(ctx context.Context, p rpc.RestoreParams) (rpc.VerifyResult, error) {
	if !ValidSnapshotID(p.SnapshotID) {
		return rpc.VerifyResult{}, agentrpc.Invalid("invalid snapshot id")
	}
	repo, err := o.repoOf(ctx, p.Repository, p.Password, p.Env)
	if err != nil {
		return rpc.VerifyResult{}, err
	}
	work, err := o.workDir("verify-")
	if err != nil {
		return rpc.VerifyResult{}, err
	}
	defer os.RemoveAll(work)

	start := time.Now()
	detail, err := o.restoreTest(ctx, repo, p.Site, p.SnapshotID, work)
	elapsed := time.Since(start).Milliseconds()
	if err == nil {
		err = o.check(ctx, repo)
	}
	if err != nil {
		// A request that went away is not a verdict on the backup.
		if ctx.Err() != nil {
			return rpc.VerifyResult{}, ctx.Err()
		}
		o.log().Warn("restore test failed", "site", p.Site.Domain, "snapshot", p.SnapshotID, "err", agentrpc.RedactCredentials(err.Error()))
		return rpc.VerifyResult{Passed: false, RestoreMillis: elapsed, Detail: failDetail(err)}, nil
	}
	// The detail reaches the API, its journal and the backup's record:
	// restic's words can carry the repository URL.
	return rpc.VerifyResult{Passed: true, RestoreMillis: elapsed, Detail: agentrpc.RedactCredentials(detail)}, nil
}

// restoreTest restores and checks everything a restore would put live.
func (o *Ops) restoreTest(ctx context.Context, repo host.Repo, s state.Site, id, work string) (string, error) {
	unlock, err := o.lock(ctx, repo.URL, false)
	if err != nil {
		return "", err
	}
	defer unlock()
	// What a restore would refuse is not a passing test (SR-T6).
	snaps := o.siteSnaps(repo, s)
	snap, err := o.siteSnapshot(ctx, snaps, id)
	if err != nil {
		return "", err
	}
	if err := o.restoreFiles(ctx, repo, snap.ID, work); err != nil {
		return "", err
	}
	// A site with databases is restorable only with them: every database
	// the backup holds is restore-tested.
	databases, err := o.siteDatabases(ctx, s)
	if err != nil {
		return "", err
	}
	parts, err := o.databaseParts(ctx, snaps, snap)
	if err != nil {
		return "", fmt.Errorf("database snapshot: %w", err)
	}
	if len(databases) > 0 && len(parts) == 0 {
		return "", agentrpc.Invalid("the backup has no database snapshot")
	}
	dumps, err := o.fetchDumps(ctx, repo, parts, work)
	if err != nil {
		return "", err
	}

	tree, err := sitefs.Open(filepath.Join(work, filesDir), sitefs.Owner{})
	if err != nil {
		return "", err
	}
	defer tree.Close()
	rel, err := checkRestored(tree, s.RootPath)
	if err != nil {
		return "", err
	}
	files := 0
	if err := tree.Walk(rel, func(_ string, fi fs.FileInfo) error {
		if fi.Mode().IsRegular() {
			files++
		}
		return nil
	}); err != nil {
		return "", fmt.Errorf("read the restored files: %w", err)
	}
	if files == 0 {
		return "", agentrpc.Invalid("the restored tree is empty")
	}
	if len(dumps) == 0 {
		return fmt.Sprintf("%d files restored and verified", files), nil
	}

	wordpress := s.Type == state.SiteWordPress || s.Type == state.SiteWooCommerce
	tested := make([]string, 0, len(dumps))
	for _, d := range dumps {
		if err := looksLikeSQL(d.Path); err != nil {
			return "", fmt.Errorf("%s: %w", d.Name, err)
		}
		// WordPress's own tables are in the primary database only.
		primary := s.Config.Database.Enabled && !s.Config.Database.External && d.Name == s.Config.Database.Name
		tables, err := o.DB.VerifyDumpOf(ctx, d.Name, d.Path, wordpress && primary, d.Tables)
		if err != nil {
			return "", fmt.Errorf("database restore test failed (%s): %w", d.Name, err)
		}
		tested = append(tested, fmt.Sprintf("%s: %d tables", d.Name, tables))
	}
	if len(dumps) == 1 {
		return fmt.Sprintf("%d files restored, database imported (%s) and verified", files, tested[0]), nil
	}
	return fmt.Sprintf("%d files restored, %d databases imported (%s) and verified", files, len(dumps), strings.Join(tested, "; ")), nil
}

// check is restic's own integrity check of a tenth of the data. It takes
// restic's exclusive lock, so ours too.
func (o *Ops) check(ctx context.Context, repo host.Repo) error {
	unlock, err := o.lock(ctx, repo.URL, true)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := o.run(ctx, repo, dataTimeout, "check", "--read-data-subset=10%"); err != nil {
		return fmt.Errorf("repository check failed: %w", err)
	}
	return nil
}

// failDetail is a failure's text for the panel, with restic's stderr cut
// off: it can name the repository's location, and it is in the agent's log.
func failDetail(err error) string {
	msg := err.Error()
	var ee *host.ExitError
	if errors.As(err, &ee) && ee.Name == "restic" {
		msg = strings.TrimSuffix(strings.TrimSuffix(msg, ee.Error()), ": ")
		msg += fmt.Sprintf(": restic exited with status %d (the agent's log has its output)", ee.Code)
	}
	// Any other error may still quote the repository URL.
	return agentrpc.RedactCredentials(msg)
}

// pgDumpHeader and pgDumpEnd are the first and the last comment of a plain
// pg_dump: the header (as the file's first two lines, `--` then this one) and
// the closing line it writes only when it has dumped everything. An empty
// PostgreSQL database has no CREATE TABLE to recognise it by, so these are
// what it is recognised by: both, never one.
const (
	pgDumpHeader = "--\n-- PostgreSQL database dump\n"
	pgDumpEnd    = "\n-- PostgreSQL database dump complete\n"
)

// pgDumpIntact reports whether f, whose first bytes are head, is a whole
// pg_dump: it starts with the header and its last 4 KiB hold the closing
// line, so a dump that stopped half way is refused. Only the two ends are
// read.
func pgDumpIntact(f *os.File, head string) bool {
	if !strings.HasPrefix(head, pgDumpHeader) {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	tail := make([]byte, 4096)
	n, err := f.ReadAt(tail, max(fi.Size()-int64(len(tail)), 0))
	if err != nil && !errors.Is(err, io.EOF) {
		return false
	}
	return strings.Contains("\n"+string(tail[:n]), pgDumpEnd)
}

// looksLikeSQL reads the dump's head only (and, for a PostgreSQL dump, its
// tail): a multi-gigabyte database is not loaded into memory to look at 4 KiB.
func looksLikeSQL(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	head := make([]byte, 4096)
	n, err := io.ReadFull(f, head)
	switch {
	case errors.Is(err, io.EOF):
		return agentrpc.Invalid("the database dump is empty")
	case err != nil && !errors.Is(err, io.ErrUnexpectedEOF):
		return err
	}
	if s := string(head[:n]); !strings.Contains(s, "CREATE TABLE") && !strings.Contains(s, "MariaDB dump") && !pgDumpIntact(f, s) {
		return agentrpc.Invalid("the database dump does not look like SQL")
	}
	return nil
}
