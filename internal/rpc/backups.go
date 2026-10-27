package rpc

import (
	"errors"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/cloudground-it/cloudground/internal/state"
)

// A restic repository is named by its URL (a local path, s3:…, b2:…) and
// opened with its password. Env carries the object store's credentials,
// which reach restic through its environment and never its command line.

// BackupParams takes a snapshot of a site: Kind "files", "database", or
// "full" (both; also the meaning of an empty Kind).
type BackupParams struct {
	Site       state.Site        `json:"site"`
	Repository string            `json:"repository"`
	Password   string            `json:"password"`
	Kind       string            `json:"kind"`
	Env        map[string]string `json:"env,omitempty"`
}

// Sites lists the site backed up.
func (p *BackupParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// Validate requires a known kind.
func (p *BackupParams) Validate() error {
	switch p.Kind {
	case "", "full", "files", "database":
		return nil
	}
	return errors.New("backup kind must be full, files, or database")
}

// BackupResult is the snapshot taken.
type BackupResult struct {
	SnapshotID string `json:"snapshot_id"`
	SizeBytes  int64  `json:"size_bytes"`
	// Databases are the names of the databases the snapshot holds, which
	// a restore may choose among (RestoreParams.Databases).
	Databases []string `json:"databases"`
}

// RestoreParams names a snapshot of a site. RestoreSnapshot puts it back
// over the live site (Mode "files", "database", or "full", the default);
// VerifyBackup restores it aside and checks it.
type RestoreParams struct {
	Site       state.Site        `json:"site"`
	Repository string            `json:"repository"`
	Password   string            `json:"password"`
	SnapshotID string            `json:"snapshot_id"`
	Mode       string            `json:"mode,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	// Databases limits a database or full restore to these of the
	// snapshot's databases; empty restores every one the site still has.
	Databases []string `json:"databases,omitempty"`
	// Paths limits a files restore to these files or folders, relative to
	// the site's root (#23, ops/backup SPEC §5); empty restores the tree.
	Paths []string `json:"paths,omitempty"`
}

// Sites lists the site restored.
func (p *RestoreParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// Validate requires a known mode, in any case and spacing, and paths only
// on a files restore, each one a site may restore.
func (p *RestoreParams) Validate() error {
	mode := strings.ToLower(strings.TrimSpace(p.Mode))
	switch mode {
	case "", "full", "files", "database":
	default:
		return errors.New("restore mode must be full, files, or database")
	}
	if len(p.Paths) == 0 {
		return nil
	}
	if mode != "files" {
		return errors.New("only a files restore takes paths")
	}
	if len(p.Paths) > MaxRestorePaths {
		return errors.New("at most 20 paths")
	}
	for _, rel := range p.Paths {
		if _, err := RestorePath(rel); err != nil {
			return err
		}
	}
	return nil
}

// MaxRestorePaths is how many paths one restore may name.
const MaxRestorePaths = 20

// RestorePath cleans a path a files restore names (internal/api SPEC
// §5.6): relative to the site's root, inside it, not the root itself, not
// .ssh (the keys are the live site's), tmp, current, nor releases itself.
func RestorePath(raw string) (string, error) {
	bad := errors.New("invalid path " + strconvQuote(raw) + ": give a file or folder inside the site, such as shared/wp-config.php")
	p := strings.TrimSpace(raw)
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\x00\n\r") {
		return "", bad
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return "", bad
		}
	}
	p = path.Clean(p)
	top, _, _ := strings.Cut(p, "/")
	switch {
	case p == "." || p == "":
		return "", bad
	case top == ".ssh" || top == "tmp" || top == "current":
		return "", bad
	case p == "releases":
		return "", bad
	}
	return p, nil
}

func strconvQuote(s string) string { return strconv.Quote(s) }

// VerifyResult is a restore test's verdict and how long the restore took.
type VerifyResult struct {
	Passed        bool   `json:"passed"`
	RestoreMillis int64  `json:"restore_millis"`
	Detail        string `json:"detail,omitempty"`
}

// TestBackupParams names a repository to reach, and to initialise when it
// is empty.
type TestBackupParams struct {
	Repository string            `json:"repository"`
	Password   string            `json:"password"`
	Env        map[string]string `json:"env,omitempty"`
}

// RetentionParams forgets a site's snapshots older than KeepWithinDays.
// Protect lists snapshot IDs (full or short) kept whatever their age: the
// panel names the site's newest verified snapshot, so failed verifications
// can never leave a site without a restore point known to work.
type RetentionParams struct {
	Site           state.Site        `json:"site"`
	Repository     string            `json:"repository"`
	Password       string            `json:"password"`
	KeepWithinDays int               `json:"keep_within_days"`
	Protect        []string          `json:"protect,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
}

// Sites lists the site whose snapshots are pruned.
func (p *RetentionParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// RetentionResult lists, by full ID, the snapshots forgotten and the ones
// kept.
type RetentionResult struct {
	Forgotten []string `json:"forgotten"`
	Kept      []string `json:"kept"`
}

// ForgetSnapshotParams deletes one backup of a site from its repository:
// the files snapshot SnapshotID (full or short) and the database snapshot
// its db: tag names, then prunes their data (R-4.3).
type ForgetSnapshotParams struct {
	Site       state.Site        `json:"site"`
	Repository string            `json:"repository"`
	Password   string            `json:"password"`
	SnapshotID string            `json:"snapshot_id"`
	Env        map[string]string `json:"env,omitempty"`
}

// Sites lists the site whose backup is deleted.
func (p *ForgetSnapshotParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// Validate requires a snapshot ID: 8 to 64 hex digits.
func (p *ForgetSnapshotParams) Validate() error {
	if !snapshotID(p.SnapshotID) {
		return errors.New("invalid snapshot id")
	}
	return nil
}

// snapshotID reports whether id is a restic snapshot ID, full or short.
func snapshotID(id string) bool {
	if len(id) < 8 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
			return false
		}
	}
	return true
}

// ForgetSnapshotResult lists, by full ID, the snapshots forgotten. Missing
// is true when the repository has no such snapshot of the site (nothing
// was forgotten: it is already gone).
type ForgetSnapshotResult struct {
	Forgotten []string `json:"forgotten"`
	Missing   bool     `json:"missing,omitempty"`
}

// ArchiveParams makes a downloadable archive of a backup (#23, ops/backup
// SPEC §12): Part "full" (the default), "files" or "database".
type ArchiveParams struct {
	Site       state.Site        `json:"site"`
	Repository string            `json:"repository"`
	Password   string            `json:"password"`
	SnapshotID string            `json:"snapshot_id"`
	Part       string            `json:"part,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
}

// Sites lists the site whose backup is archived.
func (p *ArchiveParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// Validate requires a snapshot ID and a known part.
func (p *ArchiveParams) Validate() error {
	if !snapshotID(p.SnapshotID) {
		return errors.New("invalid snapshot id")
	}
	if _, err := ArchivePart(p.Part); err != nil {
		return err
	}
	return nil
}

// ArchivePart is a part's normal name: "" reads as full.
func ArchivePart(part string) (string, error) {
	switch part {
	case "", "full":
		return "full", nil
	case "files", "database":
		return part, nil
	}
	return "", errors.New("part must be full, files or database")
}

// ArchiveName is the file name of a backup's archive.
func ArchiveName(snapshot, part string) string {
	return "backup-" + strings.ToLower(snapshot) + "-" + part + ".tar.gz"
}

var archiveNameRe = regexp.MustCompile(`^backup-[0-9a-f]{8,64}-(full|files|database)\.tar\.gz$`)

// ValidArchiveName reports whether name is one ArchiveName makes.
func ValidArchiveName(name string) bool { return archiveNameRe.MatchString(name) }

// ArchiveResult is the archive made.
type ArchiveResult struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// ArchiveChunkParams reads (or deletes) a site's archive.
type ArchiveChunkParams struct {
	Site   state.Site `json:"site"`
	Name   string     `json:"name"`
	Offset int64      `json:"offset,omitempty"`
	Length int        `json:"length,omitempty"`
}

// Sites lists the site whose archive is read.
func (p *ArchiveChunkParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// Validate requires an archive's name and a sane chunk.
func (p *ArchiveChunkParams) Validate() error {
	if !ValidArchiveName(p.Name) {
		return errors.New("invalid archive name")
	}
	if p.Offset < 0 || p.Length < 0 || p.Length > MaxArchiveChunk {
		return errors.New("invalid chunk")
	}
	return nil
}

// MaxArchiveChunk is the most one ReadArchiveChunk returns.
const MaxArchiveChunk = 16 << 20

// ArchiveChunkResult is the bytes read and the archive's size.
type ArchiveChunkResult struct {
	Data []byte `json:"data"`
	Size int64  `json:"size"`
}
