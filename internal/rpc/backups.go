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
	// Accounts are the site's database accounts on this server, with their
	// passwords, for the backup's description (#193, ops/backup SPEC §4).
	Accounts []ManifestAccount `json:"accounts,omitempty"`
}

// Sites lists the site backed up.
func (p *BackupParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// Validate requires a known kind and accounts a server can take.
func (p *BackupParams) Validate() error {
	switch p.Kind {
	case "", "full", "files", "database":
		return validAccounts(p.Accounts)
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
	// Foreign, set only by the panel's restore from another server (#193),
	// lets the snapshot be another server's (ForeignRestore).
	Foreign *ForeignRestore `json:"foreign,omitempty"`
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
	if p.Foreign != nil {
		return p.Foreign.validate(mode, p)
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

// RepositoryInfoParams reads a repository without changing it (#192,
// ops/backup SPEC §13): its ID and which of Expect, snapshot IDs full or
// short, it does not hold.
type RepositoryInfoParams struct {
	Repository string            `json:"repository"`
	Password   string            `json:"password"`
	Env        map[string]string `json:"env,omitempty"`
	Expect     []string          `json:"expect,omitempty"`
}

// MaxExpect is how many snapshot IDs one RepositoryInfo may look for.
const MaxExpect = 100000

// Validate requires each expected ID to be a snapshot ID, and at most
// MaxExpect of them.
func (p *RepositoryInfoParams) Validate() error {
	if len(p.Expect) > MaxExpect {
		return errors.New("too many snapshot ids")
	}
	for _, id := range p.Expect {
		if !snapshotID(id) {
			return errors.New("invalid snapshot id")
		}
	}
	return nil
}

// RepositoryInfoResult is the repository's ID (restic's config id), how
// many snapshots it holds, and the expected IDs none of them is (by its ID
// or the one it had before a copy).
type RepositoryInfoResult struct {
	ID        string   `json:"id"`
	Snapshots int      `json:"snapshots"`
	Missing   []string `json:"missing"`
}

// CopyRepositoryParams copies every snapshot of From into To, which is made
// when it does not exist (#192, ops/backup SPEC §13).
type CopyRepositoryParams struct {
	From TestBackupParams `json:"from"`
	To   TestBackupParams `json:"to"`
}

// Validate requires two repositories, each with its password.
func (p *CopyRepositoryParams) Validate() error {
	if p.From.Repository == "" || p.From.Password == "" || p.To.Repository == "" || p.To.Password == "" {
		return errors.New("repository and password required")
	}
	if p.From.Repository == p.To.Repository {
		return errors.New("the two repositories are the same")
	}
	return nil
}

// CopyRepositoryResult is the destination's ID and how many snapshots it
// holds after the copy.
type CopyRepositoryResult struct {
	ID        string `json:"id"`
	Snapshots int    `json:"snapshots"`
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
// kept. Originals are the IDs the forgotten snapshots had before `restic
// copy` gave them new ones (ops/backup SPEC §5): a record may hold those.
type RetentionResult struct {
	Forgotten []string `json:"forgotten"`
	Kept      []string `json:"kept"`
	Originals []string `json:"originals,omitempty"`
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

// ValidSnapshotID reports whether id is a restic snapshot ID, full or
// short: 8 to 64 hex digits.
func ValidSnapshotID(id string) bool { return snapshotID(id) }

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
	// Originals are the IDs the forgotten snapshots had before a copy
	// (RetentionResult).
	Originals []string `json:"originals,omitempty"`
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
