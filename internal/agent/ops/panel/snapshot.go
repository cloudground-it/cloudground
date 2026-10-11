package panel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/ops/rootfile"
	"golang.org/x/sys/unix"
)

// The pre-update snapshot (SPEC §2.5): the panel's state database and the
// agent's records, copied while neither service runs, before the new
// release first starts. Migrations only go forward, so this copy is how a
// rollback goes back.

// DefaultKeepSnapshots is how many snapshots are kept when the plan does
// not say.
const DefaultKeepSnapshots = 3

// snapshotManifest names what a snapshot holds.
const snapshotManifest = "snapshot.json"

// snapshotDirRe is the name of a snapshot directory: its UTC time first, so
// the names sort by age.
var snapshotDirRe = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[A-Za-z0-9.+_-]+$`)

// SnapshotFile is one file of a snapshot, or a path that did not exist when
// it was taken (Absent: removed again on a restore).
type SnapshotFile struct {
	Path   string      `json:"path"`
	Name   string      `json:"name,omitempty"`
	Absent bool        `json:"absent,omitempty"`
	Mode   os.FileMode `json:"mode,omitempty"`
	UID    int         `json:"uid"`
	GID    int         `json:"gid"`
	SHA256 string      `json:"sha256,omitempty"`
	Size   int64       `json:"size,omitempty"`
}

// Snapshot is the manifest of one snapshot directory.
type Snapshot struct {
	Dir     string         `json:"-"`
	Version string         `json:"version"`
	From    string         `json:"from"`
	Taken   string         `json:"taken"`
	Files   []SnapshotFile `json:"files"`
}

// snapshotPaths are the files a snapshot covers: the state database with
// its write-ahead log and shared-memory index (the index is never copied,
// only removed on a restore, since SQLite rebuilds it), and the records.
func (p Plan) snapshotPaths() (copyable, dropOnly []string) {
	if p.StateDB != "" {
		copyable = append(copyable, p.StateDB, p.StateDB+"-wal")
		dropOnly = append(dropOnly, p.StateDB+"-shm")
	}
	copyable = append(copyable, p.Records...)
	return copyable, dropOnly
}

// takeSnapshot copies the plan's state into a new directory under
// SnapshotRoot. Nothing may be writing it: the guard has stopped both
// services. A failure removes the partial directory.
func (p Plan) takeSnapshot(now time.Time) (Snapshot, error) {
	files, _ := p.snapshotPaths()
	if p.SnapshotRoot == "" || len(files) == 0 {
		return Snapshot{}, nil
	}
	if err := rootfile.MkdirMode(p.SnapshotRoot, 0o700); err != nil {
		return Snapshot{}, err
	}
	name := now.UTC().Format("20060102T150405Z") + "-" + safeLabel(p.Previous) + "-to-" + safeLabel(p.Version)
	dir := filepath.Join(p.SnapshotRoot, name)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return Snapshot{}, fmt.Errorf("snapshot directory: %w", err)
	}
	snap := Snapshot{Dir: dir, Version: p.Version, From: p.Previous, Taken: now.UTC().Format(time.RFC3339)}
	fail := func(err error) (Snapshot, error) {
		os.RemoveAll(dir)
		return Snapshot{}, fmt.Errorf("snapshot the panel's state: %w", err)
	}
	used := map[string]bool{}
	for _, path := range files {
		f := SnapshotFile{Path: path}
		src, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if errors.Is(err, os.ErrNotExist) {
			f.Absent = true
			snap.Files = append(snap.Files, f)
			continue
		}
		if err != nil {
			return fail(err)
		}
		fi, err := src.Stat()
		if err != nil || !fi.Mode().IsRegular() {
			src.Close()
			if err == nil {
				err = fmt.Errorf("%s is not a regular file", path)
			}
			return fail(err)
		}
		f.Name = filepath.Base(path)
		for i := 2; used[f.Name]; i++ {
			f.Name = fmt.Sprintf("%s.%d", filepath.Base(path), i)
		}
		used[f.Name] = true
		f.Mode = fi.Mode().Perm()
		f.UID, f.GID = owner(fi)
		f.SHA256, f.Size, err = copyNew(src, filepath.Join(dir, f.Name), 0o600)
		src.Close()
		if err != nil {
			return fail(err)
		}
		snap.Files = append(snap.Files, f)
	}
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fail(err)
	}
	if err := rootfile.Replace(filepath.Join(dir, snapshotManifest), append(b, '\n'), 0o600); err != nil {
		return fail(err)
	}
	return snap, nil
}

// restoreSnapshot puts every file of snap back as it was, owner and mode
// included, removes the paths that did not exist then, and the database's
// shared-memory index. Each file is checked against its recorded hash
// before it replaces the live one. Every file is tried even when one fails.
func (p Plan) restoreSnapshot(snap Snapshot) error {
	_, drop := p.snapshotPaths()
	var errs []error
	for _, f := range snap.Files {
		if f.Absent {
			if err := os.Remove(f.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
			continue
		}
		if err := restoreOne(filepath.Join(snap.Dir, f.Name), f); err != nil {
			errs = append(errs, fmt.Errorf("restore %s: %w", f.Path, err))
		}
	}
	for _, path := range drop {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ReadSnapshot reads the manifest of the snapshot in dir.
func ReadSnapshot(dir string) (Snapshot, error) {
	b, err := rootfile.ReadNoFollow(filepath.Join(dir, snapshotManifest))
	if err != nil {
		return Snapshot{}, err
	}
	var s Snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return Snapshot{}, fmt.Errorf("%s: %w", dir, err)
	}
	for _, f := range s.Files {
		if !filepath.IsAbs(f.Path) || (!f.Absent && (f.Name == "" || strings.ContainsAny(f.Name, "/\x00") || f.Name == snapshotManifest)) {
			return Snapshot{}, fmt.Errorf("%s: bad entry %q", dir, f.Path)
		}
	}
	s.Dir = dir
	return s, nil
}

// pruneSnapshots keeps the newest keep snapshots under root and removes the
// others. Only directories named like a snapshot are touched.
func pruneSnapshots(root string, keep int) error {
	if root == "" {
		return nil
	}
	if keep <= 0 {
		keep = DefaultKeepSnapshots
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && snapshotDirRe.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var errs []error
	for len(names) > keep {
		if err := os.RemoveAll(filepath.Join(root, names[0])); err != nil {
			errs = append(errs, err)
		}
		names = names[1:]
	}
	return errors.Join(errs...)
}

// Snapshots lists the snapshot directories under root, oldest first.
func Snapshots(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && snapshotDirRe.MatchString(e.Name()) {
			out = append(out, filepath.Join(root, e.Name()))
		}
	}
	sort.Strings(out)
	return out
}

func restoreOne(from string, f SnapshotFile) error {
	src, err := os.OpenFile(from, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer src.Close()
	dir := filepath.Dir(f.Path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(f.Path)+".cg-restore-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(name)
		}
	}()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), src); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != f.SHA256 {
		return fmt.Errorf("the snapshot's copy does not match its recorded hash")
	}
	if err := tmp.Chmod(f.Mode.Perm()); err != nil {
		return err
	}
	if err := tmp.Chown(f.UID, f.GID); err != nil && os.Geteuid() == 0 {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, f.Path); err != nil {
		return err
	}
	ok = true
	return nil
}

// copyNew streams src into a new file at path with mode and answers its
// sha256 and size.
func copyNew(src io.Reader, path string, mode os.FileMode) (string, int64, error) {
	dst, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, mode)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(dst, h), src)
	if err == nil {
		err = dst.Sync()
	}
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// safeLabel keeps a version usable in a directory name.
func safeLabel(v string) string {
	v = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_', r == '+':
			return r
		}
		return '_'
	}, v)
	if v == "" {
		return "unknown"
	}
	return v
}
