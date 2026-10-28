package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Downloads of a backup (SPEC §12, #23): a gzip'd tar written into the
// agent's own folder, read back in chunks by the API.

// DefaultArchiveDir is where archives are written when ArchiveDir is unset.
const DefaultArchiveDir = "/var/lib/cloudground-agent/archives"

// archiveTTL is how long an archive nobody fetched is kept.
const archiveTTL = 24 * time.Hour

func (o *Ops) archiveRoot() string {
	if o.ArchiveDir != "" {
		return o.ArchiveDir
	}
	return DefaultArchiveDir
}

// siteArchiveDir is a site's folder of archives, made root-only.
func (o *Ops) siteArchiveDir(s state.Site) (string, error) {
	if s.ID <= 0 {
		return "", agentrpc.Invalid("site id required")
	}
	dir := filepath.Join(o.archiveRoot(), strconv.FormatInt(s.ID, 10))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, os.Chmod(o.archiveRoot(), 0o700)
}

// sweepArchives removes every archive older than archiveTTL.
func (o *Ops) sweepArchives(now time.Time) {
	_ = filepath.WalkDir(o.archiveRoot(), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil && now.Sub(info.ModTime()) > archiveTTL {
			os.Remove(p)
		}
		return nil
	})
}

// ArchiveSnapshot writes a backup's archive (SPEC §12).
func (o *Ops) ArchiveSnapshot(ctx context.Context, p rpc.ArchiveParams) (rpc.ArchiveResult, error) {
	part, err := rpc.ArchivePart(p.Part)
	if err != nil {
		return rpc.ArchiveResult{}, agentrpc.Invalid("%s", err.Error())
	}
	if !ValidSnapshotID(p.SnapshotID) {
		return rpc.ArchiveResult{}, agentrpc.Invalid("invalid snapshot id")
	}
	repo, err := o.repoOf(ctx, p.Repository, p.Password, p.Env)
	if err != nil {
		return rpc.ArchiveResult{}, err
	}
	s := p.Site
	dir, err := o.siteArchiveDir(s)
	if err != nil {
		return rpc.ArchiveResult{}, err
	}
	o.sweepArchives(time.Now())
	work, err := o.workDir("archive-")
	if err != nil {
		return rpc.ArchiveResult{}, err
	}
	defer os.RemoveAll(work)
	unlock, err := o.lock(ctx, repo.URL, false)
	if err != nil {
		return rpc.ArchiveResult{}, err
	}
	defer unlock()

	snaps := o.siteSnaps(repo, s)
	files, err := o.siteSnapshot(ctx, snaps, p.SnapshotID)
	if err != nil {
		return rpc.ArchiveResult{}, err
	}
	var parts []dbPart
	if part != "files" {
		if parts, err = o.databaseParts(ctx, snaps, files); err != nil {
			return rpc.ArchiveResult{}, err
		}
		if part == "database" && len(parts) == 0 {
			return rpc.ArchiveResult{}, agentrpc.Invalid("snapshot %s holds no database", p.SnapshotID)
		}
	}
	// Room first (SPEC §12): the selection's restore size, the dumps twice
	// (the work copy, then the archive) and a margin.
	if err := o.checkArchiveRoom(ctx, repo, dir, part, files.ID, parts); err != nil {
		return rpc.ArchiveResult{}, err
	}
	var dumps []dumpFile
	if len(parts) > 0 {
		if dumps, err = o.fetchDumps(ctx, repo, parts, work); err != nil {
			return rpc.ArchiveResult{}, err
		}
	}

	name := rpc.ArchiveName(p.SnapshotID, part)
	tmp, err := os.CreateTemp(dir, ".partial-")
	if err != nil {
		return rpc.ArchiveResult{}, err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	gz := gzip.NewWriter(tmp)
	tw := tar.NewWriter(gz)
	if part != "database" {
		if err := o.archiveFiles(ctx, repo, s, files.ID, tw); err != nil {
			tmp.Close()
			return rpc.ArchiveResult{}, err
		}
	}
	for _, d := range dumps {
		if err := addFile(tw, d.Path, "databases/"+d.Name+".sql"); err != nil {
			tmp.Close()
			return rpc.ArchiveResult{}, fmt.Errorf("add the dump of %s: %w", d.Name, err)
		}
	}
	err = errors.Join(tw.Close(), gz.Close(), tmp.Sync())
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return rpc.ArchiveResult{}, err
	}
	final := filepath.Join(dir, name)
	if err := os.Rename(tmp.Name(), final); err != nil {
		return rpc.ArchiveResult{}, err
	}
	info, err := os.Stat(final)
	if err != nil {
		return rpc.ArchiveResult{}, err
	}
	return rpc.ArchiveResult{Name: name, Size: info.Size()}, nil
}

// archiveFiles streams `restic dump --archive tar` of the site's root into
// tw, every entry under files/: nothing is unpacked on disk.
func (o *Ops) archiveFiles(ctx context.Context, repo host.Repo, s state.Site, id string, tw *tar.Writer) error {
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := o.runIO(ctx, repo, dataTimeout, nil, pw, "dump", "--archive", "tar", id, s.RootPath)
		pw.CloseWithError(err)
		done <- err
	}()
	err := retar(tar.NewReader(pr), tw, s.RootPath)
	if err != nil {
		pr.CloseWithError(err)
	}
	if rerr := <-done; err == nil && rerr != nil {
		err = fmt.Errorf("read the snapshot's files: %w", rerr)
	}
	return err
}

// archiveMargin is the room kept beyond an archive's estimated size: a
// tenth of it, and at least this much.
const archiveMargin = 512 << 20

// restoreSize is `restic stats --mode restore-size` of snapshots, in bytes.
func (o *Ops) restoreSize(ctx context.Context, repo host.Repo, ids ...string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	out, err := o.run(ctx, repo, planTimeout, append([]string{"stats", "--mode", "restore-size", "--json"}, ids...)...)
	if err != nil {
		return 0, fmt.Errorf("measure the backup: %w", err)
	}
	var st struct {
		TotalSize int64 `json:"total_size"`
	}
	if err := json.Unmarshal(lastJSONLine(out), &st); err != nil {
		return 0, fmt.Errorf("measure the backup: %w", err)
	}
	return st.TotalSize, nil
}

// lastJSONLine is the last line of out that starts a JSON object.
func lastJSONLine(out []byte) []byte {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); strings.HasPrefix(l, "{") {
			return []byte(l)
		}
	}
	return out
}

// archiveNeed is the room an archive of the selection takes: the files'
// restore size (gzip only shrinks it), each dump twice, and the margin.
func archiveNeed(files, dumps int64) int64 {
	n := files + 2*dumps
	margin := n / 10
	if margin < archiveMargin {
		margin = archiveMargin
	}
	return n + margin
}

// checkArchiveRoom refuses an archive that would not fit in dir's
// filesystem (conflict: the server, not the request, lacks room).
func (o *Ops) checkArchiveRoom(ctx context.Context, repo host.Repo, dir, part, filesID string, parts []dbPart) error {
	var filesSize, dumpSize int64
	var err error
	if part != "database" {
		if filesSize, err = o.restoreSize(ctx, repo, filesID); err != nil {
			return err
		}
	}
	var ids []string
	if part != "files" {
		for _, p := range parts {
			ids = append(ids, p.ID)
		}
	}
	if dumpSize, err = o.restoreSize(ctx, repo, ids...); err != nil {
		return err
	}
	need := archiveNeed(filesSize, dumpSize)
	free, err := o.freeSpace(dir)
	if err != nil {
		return err
	}
	if need > free {
		return agentrpc.Conflict("not enough free space on the server to prepare this download: it needs about %s and %s is free", humanBytes(need), humanBytes(free))
	}
	return nil
}

// freeSpace is the room free for root in dir.
func (o *Ops) freeSpace(dir string) (int64, error) {
	if o.FreeSpace != nil {
		return o.FreeSpace(dir)
	}
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// humanBytes is n in binary units, for a message.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// retar copies a tar stream into tw with every entry under files/, the
// site's root stripped from the names (restic writes them with or without
// it, with or without a leading slash). Links keep their targets; no entry
// may climb out of files/.
func retar(tr *tar.Reader, tw *tar.Writer, root string) error {
	rootRel := strings.Trim(root, "/")
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := strings.TrimLeft(h.Name, "/")
		if name == rootRel || strings.HasPrefix(name, rootRel+"/") {
			name = strings.TrimPrefix(strings.TrimPrefix(name, rootRel), "/")
		}
		clean := path.Clean("files/" + name)
		if clean != "files" && !strings.HasPrefix(clean, "files/") {
			return fmt.Errorf("an entry of the snapshot leaves its folder: %q", h.Name)
		}
		if h.Typeflag == tar.TypeDir && !strings.HasSuffix(clean, "/") {
			clean += "/"
		}
		h.Name = clean
		if h.Typeflag == tar.TypeLink {
			// A hard link names another entry, which moved too.
			target := strings.TrimLeft(h.Linkname, "/")
			if target == rootRel || strings.HasPrefix(target, rootRel+"/") {
				target = strings.TrimPrefix(strings.TrimPrefix(target, rootRel), "/")
			}
			h.Linkname = path.Clean("files/" + target)
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := io.Copy(tw, tr); err != nil {
				return err
			}
		}
	}
}

// addFile appends a root-only work file to tw as name.
func addFile(tw *tar.Writer, src, name string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: info.Size(), ModTime: info.ModTime(), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}

// archivePath is a site's archive, by a name ArchiveName makes.
func (o *Ops) archivePath(s state.Site, name string) (string, error) {
	if !rpc.ValidArchiveName(name) {
		return "", agentrpc.Invalid("invalid archive name")
	}
	if s.ID <= 0 {
		return "", agentrpc.Invalid("site id required")
	}
	return filepath.Join(o.archiveRoot(), strconv.FormatInt(s.ID, 10), name), nil
}

// ReadArchiveChunk reads part of a site's archive.
func (o *Ops) ReadArchiveChunk(_ context.Context, p rpc.ArchiveChunkParams) (rpc.ArchiveChunkResult, error) {
	file, err := o.archivePath(p.Site, p.Name)
	if err != nil {
		return rpc.ArchiveChunkResult{}, err
	}
	n := p.Length
	if n <= 0 || n > rpc.MaxArchiveChunk {
		n = rpc.MaxArchiveChunk
	}
	f, err := os.Open(file)
	if errors.Is(err, fs.ErrNotExist) {
		return rpc.ArchiveChunkResult{}, agentrpc.NotFound("no such archive")
	}
	if err != nil {
		return rpc.ArchiveChunkResult{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return rpc.ArchiveChunkResult{}, err
	}
	buf := make([]byte, n)
	got, err := f.ReadAt(buf, p.Offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return rpc.ArchiveChunkResult{}, err
	}
	return rpc.ArchiveChunkResult{Data: buf[:got], Size: info.Size()}, nil
}

// DeleteArchive removes a site's archive; a missing one is no error.
func (o *Ops) DeleteArchive(_ context.Context, p rpc.ArchiveChunkParams) (map[string]any, error) {
	file, err := o.archivePath(p.Site, p.Name)
	if err != nil {
		return nil, err
	}
	if err := os.Remove(file); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return map[string]any{"deleted": true}, nil
}

// restorePaths puts back only paths of snapshot id, each relative to the
// site's root (SPEC §5, Paths): restored into work, all checked, then each
// copied beside its live place and renamed in.
func (o *Ops) restorePaths(ctx context.Context, repo host.Repo, s state.Site, id, work string, paths []string) error {
	clean := make([]string, 0, len(paths))
	args := []string{"restore", id, "--target", filepath.Join(work, filesDir)}
	for _, raw := range paths {
		p, err := rpc.RestorePath(raw)
		if err != nil {
			return agentrpc.Invalid("%s", err.Error())
		}
		clean = append(clean, p)
		args = append(args, "--include", path.Join(s.RootPath, p))
	}
	if _, err := o.run(ctx, repo, dataTimeout, args...); err != nil {
		return fmt.Errorf("restore the snapshot's files: %w", err)
	}
	tree, err := sitefs.Open(filepath.Join(work, filesDir), sitefs.Owner{})
	if err != nil {
		return err
	}
	defer tree.Close()
	rootRel := strings.TrimPrefix(s.RootPath, "/")
	for _, p := range clean {
		fi, err := tree.Lstat(path.Join(rootRel, p))
		if err != nil || !(fi.Mode().IsRegular() || fi.IsDir()) {
			return agentrpc.Invalid("snapshot %s has no %s", id, p)
		}
	}
	owner, err := o.Owner(s.SystemUser)
	if err != nil {
		return err
	}
	live, err := sitefs.Open(s.RootPath, owner)
	if err != nil {
		return err
	}
	defer live.Close()
	for _, p := range clean {
		if err := putBack(tree, path.Join(rootRel, p), live, p); err != nil {
			return fmt.Errorf("put back %s: %w", p, err)
		}
	}
	if o.RefreshPHP != nil {
		if err := o.RefreshPHP(ctx, s); err != nil {
			o.log().Warn("refresh PHP after a path restore", "site", s.Domain, "err", err)
		}
	}
	o.purge(s)
	return nil
}

// putBack copies src of tree to a dot-named temporary beside rel in live,
// then renames it into place: a file in one rename, a folder after its live
// copy is renamed aside (and removed once the new one is in).
func putBack(tree *sitefs.Root, src string, live *sitefs.Root, rel string) error {
	parent := path.Dir(rel)
	if parent != "." {
		if err := live.MkdirAll(parent, 0o750); err != nil {
			return err
		}
	}
	tmpName, err := dotName(".cg-restore-")
	if err != nil {
		return err
	}
	tmp := path.Join(parent, tmpName)
	if err := sitefs.CopyTree(tree, src, live, tmp); err != nil {
		live.RemoveAll(tmp)
		return err
	}
	cur, err := live.Lstat(rel)
	if err == nil && cur.IsDir() {
		oldName, err := dotName(".cg-old-")
		if err != nil {
			live.RemoveAll(tmp)
			return err
		}
		old := path.Join(parent, oldName)
		if err := live.Rename(rel, old, 0); err != nil {
			live.RemoveAll(tmp)
			return err
		}
		if err := live.Rename(tmp, rel, 0); err != nil {
			_ = live.Rename(old, rel, 0)
			live.RemoveAll(tmp)
			return err
		}
		return live.RemoveAll(old)
	}
	if err := live.Rename(tmp, rel, 0); err != nil {
		live.RemoveAll(tmp)
		return err
	}
	return nil
}

// dotName is prefix and random hex: no domain or WordPress file starts
// with it.
func dotName(prefix string) (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b), nil
}
