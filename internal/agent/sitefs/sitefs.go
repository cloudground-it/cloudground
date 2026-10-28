// Package sitefs gives root confined access to one site's directory tree.
//
// A Root is a descriptor on a directory. Every path it is asked for is
// resolved by openat2(RESOLVE_BENEATH|RESOLVE_NO_MAGICLINKS) relative to that
// descriptor, so the kernel refuses, at the moment of each open, anything that
// would leave the tree: an absolute path, a ".." too many, an absolute
// symlink, a relative one that climbs out, a /proc magic link. The site layout
// uses relative symlinks only, which resolve beneath and are followed.
//
// SPEC.md in this directory is the contract.
package sitefs

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

var (
	// ErrEscape: the path, or a symlink on it, leaves the root.
	ErrEscape = errors.New("path escapes the site")
	// ErrSymlink: a symlink where the operation refuses to follow one.
	ErrSymlink = errors.New("path is a symlink; refusing to follow it")
	// ErrUnsupported: the kernel has no openat2, so no confined open exists.
	ErrUnsupported = errors.New("the kernel refused openat2; refusing an unconfined open as root")
	// ErrTooLarge: a bounded read found more than it may return.
	ErrTooLarge = errors.New("file too large")
	// ErrNotRegular: the entry is not a regular file.
	ErrNotRegular = errors.New("not a regular file")
	// ErrForeignLink: a hard link to someone else's file (SPEC.md).
	ErrForeignLink = errors.New("a hard link to a file the site does not own")
)

// foreign reports whether st is a foreign link of r's: a non-directory with
// more than one link that belongs to someone other than r's owner, which
// may be a file outside the tree.
func (r *Root) foreign(st *unix.Stat_t) bool {
	return st.Mode&unix.S_IFMT != unix.S_IFDIR && st.Nlink > 1 && int(st.Uid) != r.owner.UID
}

// Rename flags.
const (
	NoReplace = unix.RENAME_NOREPLACE
	Exchange  = unix.RENAME_EXCHANGE
)

const beneath = unix.RESOLVE_BENEATH | unix.RESOLVE_NO_MAGICLINKS

// Owner is who the entries a Root creates belong to.
type Owner struct{ UID, GID int }

// LookupOwner resolves a Unix user to its uid and primary gid. An unknown user
// is an error: a site tree must never be left with root-owned files because a
// lookup failed quietly.
func LookupOwner(name string) (Owner, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return Owner{}, err
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return Owner{}, err
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return Owner{}, err
	}
	return Owner{UID: uid, GID: gid}, nil
}

type handle struct {
	fd   int
	path string
}

// Root is a confined handle on a directory tree.
type Root struct {
	h     *handle
	owner Owner
	prot  *protection // nil: not a protected view (Protect)
	// prefix is where this Root is in the tree it was opened from by
	// SubStrict ("" otherwise): a symlink target is checked against that
	// tree.
	prefix string
}

// Open pins the directory at the absolute path dir. Neither dir nor any of
// its components may be a symlink.
func Open(dir string, o Owner) (*Root, error) {
	if !path.IsAbs(dir) {
		return nil, &fs.PathError{Op: "open root", Path: dir, Err: errors.New("not an absolute path")}
	}
	fd, err := openat2(unix.AT_FDCWD, dir, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0,
		unix.RESOLVE_NO_SYMLINKS|unix.RESOLVE_NO_MAGICLINKS)
	if err != nil {
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EPERM) {
			return nil, ErrUnsupported
		}
		return nil, &fs.PathError{Op: "open root", Path: dir, Err: mapErr(err)}
	}
	return &Root{h: &handle{fd: fd, path: path.Clean(dir)}, owner: o}, nil
}

// Close releases the descriptor. Views made by WithOwner share it.
func (r *Root) Close() error {
	if r == nil || r.h.fd < 0 {
		return nil
	}
	err := unix.Close(r.h.fd)
	r.h.fd = -1
	return err
}

// Path is the directory the Root was opened on. It is for messages only.
func (r *Root) Path() string { return r.h.path }

// Owner is who the entries this Root creates belong to.
func (r *Root) Owner() Owner { return r.owner }

// WithOwner is the same Root creating entries for o instead.
func (r *Root) WithOwner(o Owner) *Root {
	return &Root{h: r.h, owner: o, prot: r.prot, prefix: r.prefix}
}

// Sub opens the directory rel beneath r as a Root of its own (following an
// in-tree link at rel), with the same owner. Close it separately.
func (r *Root) Sub(rel string) (*Root, error) {
	c, err := clean(rel)
	if err != nil {
		return nil, err
	}
	fd, err := r.open(c, unix.O_PATH|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, pathErr("open", c, err)
	}
	return &Root{h: &handle{fd: fd, path: path.Join(r.h.path, c)}, owner: r.owner, prot: r.prot}, nil
}

// ------------------------------------------------------------ resolution --

func openat2(dirfd int, p string, flags int, mode uint32, resolve uint64) (int, error) {
	how := unix.OpenHow{Flags: uint64(flags) | unix.O_CLOEXEC, Resolve: resolve}
	if flags&(unix.O_CREAT|unix.O_TMPFILE) != 0 {
		how.Mode = uint64(mode)
	}
	for i := 0; ; i++ {
		fd, err := unix.Openat2(dirfd, p, &how)
		// EAGAIN: a rename raced a ".." lookup and the kernel could not prove
		// it stayed beneath. Retrying is what the man page prescribes.
		if (errors.Is(err, unix.EAGAIN) && i < 64) || errors.Is(err, unix.EINTR) {
			continue
		}
		return fd, err
	}
}

// open resolves the cleaned rel beneath the root.
func (r *Root) open(c string, flags int, mode uint32) (int, error) {
	if r.h.fd < 0 {
		return -1, unix.EBADF
	}
	fd, err := openat2(r.h.fd, c, flags, mode, beneath)
	if errors.Is(err, unix.ENOSYS) {
		return -1, ErrUnsupported
	}
	return fd, err
}

// clean normalises a root-relative path and refuses one that names anything
// outside the root.
func clean(rel string) (string, error) {
	if strings.IndexByte(rel, 0) >= 0 {
		return "", pathErr("resolve", rel, unix.EINVAL)
	}
	if rel == "" {
		return ".", nil
	}
	if path.IsAbs(rel) {
		return "", pathErr("resolve", rel, ErrEscape)
	}
	c := path.Clean(rel)
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", pathErr("resolve", rel, ErrEscape)
	}
	return c, nil
}

// parent opens the directory holding rel's final component and returns it
// with that component's name. The root itself has no parent here.
func (r *Root) parent(rel string, flags int) (int, string, string, error) {
	c, err := clean(rel)
	if err != nil {
		return -1, "", "", err
	}
	if c == "." {
		return -1, "", c, pathErr("resolve", c, unix.EINVAL)
	}
	fd, err := r.open(path.Dir(c), flags|unix.O_DIRECTORY, 0)
	if err != nil {
		return -1, "", c, pathErr("open", path.Dir(c), err)
	}
	return fd, path.Base(c), c, nil
}

func mapErr(err error) error {
	switch {
	case errors.Is(err, unix.EXDEV):
		return ErrEscape
	case errors.Is(err, unix.ELOOP):
		return ErrSymlink
	}
	return err
}

func pathErr(op, rel string, err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return err
	}
	if errors.Is(err, ErrUnsupported) {
		return err
	}
	return &fs.PathError{Op: op, Path: rel, Err: mapErr(err)}
}

// -------------------------------------------------------------- reading --

// OpenFile opens rel beneath the root. A write, create or truncate never
// follows a symlink at the final name. Only a regular file or a directory is
// returned: the open is non-blocking and checked, so a FIFO cannot hang root.
func (r *Root) OpenFile(rel string, flags int, perm os.FileMode) (*os.File, error) {
	c, err := clean(rel)
	if err != nil {
		return nil, err
	}
	write := flags&(unix.O_WRONLY|unix.O_RDWR|unix.O_CREAT|unix.O_TRUNC) != 0
	if write {
		flags |= unix.O_NOFOLLOW
	}
	var fd int
	if write && r.prot != nil && c != "." {
		// Opened through the parent the guard checked.
		pfd, base, _, perr := r.parentW(c, unix.O_PATH)
		if perr != nil {
			return nil, perr
		}
		fd, err = unix.Openat(pfd, base, flags|unix.O_NONBLOCK|unix.O_CLOEXEC, uint32(perm.Perm()))
		unix.Close(pfd)
	} else {
		fd, err = r.open(c, flags|unix.O_NONBLOCK, uint32(perm.Perm()))
	}
	if err != nil {
		return nil, pathErr("open", c, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return nil, pathErr("open", c, err)
	}
	if t := st.Mode & unix.S_IFMT; t != unix.S_IFREG && t != unix.S_IFDIR {
		unix.Close(fd)
		return nil, pathErr("open", c, ErrNotRegular)
	}
	if err := unix.SetNonblock(fd, false); err != nil {
		unix.Close(fd)
		return nil, pathErr("open", c, err)
	}
	return os.NewFile(uintptr(fd), c), nil
}

// Open opens rel for reading, following an in-tree link at the final name.
func (r *Root) Open(rel string) (*os.File, error) { return r.OpenFile(rel, unix.O_RDONLY, 0) }

// ReadFile reads the regular file rel, not following a symlink at the final
// name, and returns at most limit bytes (ErrTooLarge past that).
func (r *Root) ReadFile(rel string, limit int64) ([]byte, error) {
	f, err := r.OpenFile(rel, unix.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, pathErr("read", f.Name(), ErrNotRegular)
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, pathErr("read", f.Name(), err)
	}
	if int64(len(b)) > limit {
		return nil, pathErr("read", f.Name(), ErrTooLarge)
	}
	return b, nil
}

// Lstat describes rel itself, a symlink as a symlink.
func (r *Root) Lstat(rel string) (fs.FileInfo, error) {
	c, err := clean(rel)
	if err != nil {
		return nil, err
	}
	var st unix.Stat_t
	if c == "." {
		if err := unix.Fstat(r.h.fd, &st); err != nil {
			return nil, pathErr("lstat", c, err)
		}
		return newInfo(".", &st), nil
	}
	pfd, base, c, err := r.parent(c, unix.O_PATH)
	if err != nil {
		return nil, err
	}
	defer unix.Close(pfd)
	if err := unix.Fstatat(pfd, base, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, pathErr("lstat", c, err)
	}
	return newInfo(base, &st), nil
}

// Stat describes what rel resolves to beneath the root.
func (r *Root) Stat(rel string) (fs.FileInfo, error) {
	c, err := clean(rel)
	if err != nil {
		return nil, err
	}
	fd, err := r.open(c, unix.O_PATH, 0)
	if err != nil {
		return nil, pathErr("stat", c, err)
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, pathErr("stat", c, err)
	}
	return newInfo(path.Base(c), &st), nil
}

// ReadDir lists the directory rel (following an in-tree link at rel), each
// entry described without following it, sorted by name.
func (r *Root) ReadDir(rel string) ([]fs.FileInfo, error) {
	c, err := clean(rel)
	if err != nil {
		return nil, err
	}
	fd, err := r.open(c, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, pathErr("readdir", c, err)
	}
	d := os.NewFile(uintptr(fd), c)
	defer d.Close()
	names, err := readNames(d)
	if err != nil {
		return nil, pathErr("readdir", c, err)
	}
	out := make([]fs.FileInfo, 0, len(names))
	for _, n := range names {
		var st unix.Stat_t
		if err := unix.Fstatat(fd, n, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			return nil, pathErr("readdir", path.Join(c, n), err)
		}
		out = append(out, newInfo(n, &st))
	}
	return out, nil
}

// readNames lists a directory through its own descriptor, sorted.
func readNames(d *os.File) ([]string, error) {
	var names []string
	for {
		batch, err := d.Readdirnames(1024)
		names = append(names, batch...)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			break
		}
	}
	sort.Strings(names)
	return names, nil
}

// Readlink returns the target text of the symlink rel.
func (r *Root) Readlink(rel string) (string, error) {
	pfd, base, c, err := r.parent(rel, unix.O_PATH)
	if err != nil {
		return "", err
	}
	defer unix.Close(pfd)
	buf := make([]byte, 4096)
	n, err := unix.Readlinkat(pfd, base, buf)
	if err != nil {
		return "", pathErr("readlink", c, err)
	}
	return string(buf[:n]), nil
}

// Resolve returns the root-relative path rel names once every symlink on it
// is followed, the way the kernel would but lexically, at most 40 links. An
// absolute target, or a ".." above the root, is ErrEscape. A missing tail is
// kept as it is (a file about to be created resolves to itself). The result
// is still opened beneath the root by whatever uses it, so a race that
// changes a link after this can only lead to another in-tree path.
func (r *Root) Resolve(rel string) (string, error) {
	c, err := clean(rel)
	if err != nil {
		return "", err
	}
	if c == "." {
		return c, nil
	}
	done := "."
	rest := strings.Split(c, "/")
	links := 0
	for len(rest) > 0 {
		name := rest[0]
		rest = rest[1:]
		switch name {
		case "", ".":
			continue
		case "..":
			if done == "." {
				return "", pathErr("resolve", rel, ErrEscape)
			}
			done = path.Dir(done)
			continue
		}
		next := join(done, name)
		fi, err := r.Lstat(next)
		if errors.Is(err, fs.ErrNotExist) {
			return path.Join(append([]string{next}, rest...)...), nil
		}
		if err != nil {
			return "", err
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			done = next
			continue
		}
		if links++; links > 40 {
			return "", pathErr("resolve", rel, ErrSymlink)
		}
		t, err := r.Readlink(next)
		if err != nil {
			return "", err
		}
		if path.IsAbs(t) {
			return "", pathErr("resolve", rel, ErrEscape)
		}
		rest = append(strings.Split(t, "/"), rest...)
	}
	return done, nil
}

// -------------------------------------------------------------- writing --

// Create makes a new file rel (never an existing name, never through a
// symlink), owned by the Root's owner with mode perm.
func (r *Root) Create(rel string, perm os.FileMode) (*os.File, error) {
	pfd, base, c, err := r.parentW(rel, unix.O_PATH)
	if err != nil {
		return nil, err
	}
	defer unix.Close(pfd)
	fd, err := unix.Openat(pfd, base, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, pathErr("create", c, err)
	}
	if err := r.own(fd, perm); err != nil {
		unix.Close(fd)
		unix.Unlinkat(pfd, base, 0)
		return nil, pathErr("create", c, err)
	}
	return os.NewFile(uintptr(fd), c), nil
}

// own gives a descriptor to the Root's owner, then sets its mode (in that
// order: a chown may clear mode bits).
func (r *Root) own(fd int, perm os.FileMode) error {
	if err := unix.Fchown(fd, r.owner.UID, r.owner.GID); err != nil {
		return err
	}
	return unix.Fchmod(fd, uint32(perm.Perm()))
}

func tempName() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return ".cg-tmp-" + hex.EncodeToString(b[:]), nil
}

// Replace atomically puts a new file at rel, filled by fill, owned by the
// Root's owner with mode perm. Whatever was at rel (a file, a hard link, a
// symlink) is replaced by the rename, never written through.
func (r *Root) Replace(rel string, perm os.FileMode, fill func(*os.File) error) error {
	return r.replace(rel, perm, fill, true)
}

// ReplaceUnsynced is Replace without the fsyncs, for derived files that are
// rebuilt when lost (precompressed assets): a deploy writes thousands.
func (r *Root) ReplaceUnsynced(rel string, perm os.FileMode, fill func(*os.File) error) error {
	return r.replace(rel, perm, fill, false)
}

func (r *Root) replace(rel string, perm os.FileMode, fill func(*os.File) error, sync bool) error {
	pfd, base, c, err := r.parentW(rel, unix.O_RDONLY)
	if err != nil {
		return err
	}
	defer unix.Close(pfd)
	tmp, err := tempName()
	if err != nil {
		return err
	}
	fd, err := unix.Openat(pfd, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return pathErr("replace", c, err)
	}
	f := os.NewFile(uintptr(fd), c)
	err = fill(f)
	if err == nil {
		err = r.own(fd, perm)
	}
	if err == nil && sync {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = unix.Renameat(pfd, tmp, pfd, base)
	}
	if err != nil {
		unix.Unlinkat(pfd, tmp, 0)
		return pathErr("replace", c, err)
	}
	if sync {
		_ = unix.Fsync(pfd)
	}
	return nil
}

// WriteFile writes data to rel atomically, as the file manager does: an
// in-tree symlink at rel is followed (Resolve) and the file it names
// replaced. An existing regular file keeps its owner and mode; a new one gets
// the Root's owner and perm.
func (r *Root) WriteFile(rel string, data []byte, perm os.FileMode) error {
	target, err := r.Resolve(rel)
	if err != nil {
		return err
	}
	w := r
	if fi, err := r.Lstat(target); err == nil {
		if !fi.Mode().IsRegular() {
			return pathErr("write", target, ErrNotRegular)
		}
		st := fi.Sys().(*unix.Stat_t)
		w = r.WithOwner(Owner{UID: int(st.Uid), GID: int(st.Gid)})
		perm = fi.Mode().Perm()
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return w.Replace(target, perm, func(f *os.File) error {
		_, err := f.Write(data)
		return err
	})
}

// Mkdir creates the directory rel owned by the Root's owner with mode perm.
func (r *Root) Mkdir(rel string, perm os.FileMode) error {
	pfd, base, c, err := r.parentW(rel, unix.O_PATH)
	if err != nil {
		return err
	}
	defer unix.Close(pfd)
	if err := unix.Mkdirat(pfd, base, 0o700); err != nil {
		return pathErr("mkdir", c, err)
	}
	fd, err := unix.Openat(pfd, base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return pathErr("mkdir", c, err)
	}
	defer unix.Close(fd)
	if err := r.own(fd, perm); err != nil {
		return pathErr("mkdir", c, err)
	}
	return nil
}

// MkdirAll creates rel and any missing parents, each owned by the Root's
// owner with mode perm. Existing components are kept as they are.
func (r *Root) MkdirAll(rel string, perm os.FileMode) error {
	c, err := clean(rel)
	if err != nil {
		return err
	}
	if c == "." {
		return nil
	}
	parts := strings.Split(c, "/")
	for i := range parts {
		p := strings.Join(parts[:i+1], "/")
		fd, err := r.open(p, unix.O_PATH|unix.O_DIRECTORY, 0)
		if err == nil {
			unix.Close(fd)
			continue
		}
		if !errors.Is(err, unix.ENOENT) {
			return pathErr("mkdir", p, err)
		}
		if err := r.Mkdir(p, perm); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
	}
	return nil
}

// Symlink makes rel a symlink to target, replacing whatever is at rel
// atomically. target must be relative and stay beneath the root.
func (r *Root) Symlink(target, rel string) error {
	pfd, base, c, err := r.parentW(rel, unix.O_PATH)
	if err != nil {
		return err
	}
	defer unix.Close(pfd)
	if target == "" || path.IsAbs(target) {
		return pathErr("symlink", c, ErrEscape)
	}
	if _, err := clean(path.Join(r.prefix, path.Dir(c), target)); err != nil {
		return pathErr("symlink", c, ErrEscape)
	}
	tmp, err := tempName()
	if err != nil {
		return err
	}
	if err := unix.Symlinkat(target, pfd, tmp); err != nil {
		return pathErr("symlink", c, err)
	}
	err = unix.Fchownat(pfd, tmp, r.owner.UID, r.owner.GID, unix.AT_SYMLINK_NOFOLLOW)
	if err == nil {
		err = unix.Renameat(pfd, tmp, pfd, base)
	}
	if err != nil {
		unix.Unlinkat(pfd, tmp, 0)
		return pathErr("symlink", c, err)
	}
	return nil
}

// Remove deletes rel: a file, a symlink (as a link) or an empty directory.
func (r *Root) Remove(rel string) error {
	pfd, base, c, err := r.parentW(rel, unix.O_PATH)
	if err != nil {
		return err
	}
	defer unix.Close(pfd)
	err = unix.Unlinkat(pfd, base, 0)
	if errors.Is(err, unix.EISDIR) {
		err = unix.Unlinkat(pfd, base, unix.AT_REMOVEDIR)
	}
	if err != nil {
		return pathErr("remove", c, err)
	}
	return nil
}

// RemoveAll deletes rel and everything beneath it, never following a
// symlink. A missing rel is not an error.
func (r *Root) RemoveAll(rel string) error {
	pfd, base, c, err := r.parentW(rel, unix.O_PATH)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(pfd)
	if err := removeAllAt(pfd, base); err != nil && !errors.Is(err, unix.ENOENT) {
		return pathErr("remove", c, err)
	}
	return nil
}

func removeAllAt(dirfd int, name string) error {
	var st unix.Stat_t
	if err := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return unix.Unlinkat(dirfd, name, 0)
	}
	fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	d := os.NewFile(uintptr(fd), name)
	for {
		names, rerr := d.Readdirnames(256)
		for _, n := range names {
			if err := removeAllAt(fd, n); err != nil && !errors.Is(err, unix.ENOENT) {
				d.Close()
				return err
			}
		}
		if rerr != nil || len(names) == 0 {
			break
		}
	}
	d.Close()
	return unix.Unlinkat(dirfd, name, unix.AT_REMOVEDIR)
}

// Rename moves oldRel to newRel within the root; flags is 0, NoReplace or
// Exchange. Neither final name is followed.
func (r *Root) Rename(oldRel, newRel string, flags uint) error {
	ofd, obase, oc, err := r.parentW(oldRel, unix.O_PATH)
	if err != nil {
		return err
	}
	defer unix.Close(ofd)
	nfd, nbase, nc, err := r.parentW(newRel, unix.O_PATH)
	if err != nil {
		return err
	}
	defer unix.Close(nfd)
	if err := unix.Renameat2(ofd, obase, nfd, nbase, flags); err != nil {
		return &fs.PathError{Op: "rename", Path: oc + " -> " + nc, Err: mapErr(err)}
	}
	return nil
}

// Chown sets rel's owner without following a symlink at it.
func (r *Root) Chown(rel string, uid, gid int) error {
	c, err := clean(rel)
	if err != nil {
		return err
	}
	if c == "." {
		if err := r.refuseProtected("chown", c); err != nil {
			return err
		}
		if err := unix.Fchownat(r.h.fd, "", uid, gid, unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return pathErr("chown", c, err)
		}
		return nil
	}
	pfd, base, c, err := r.parentW(c, unix.O_PATH)
	if err != nil {
		return err
	}
	defer unix.Close(pfd)
	fd, err := unix.Openat(pfd, base, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return pathErr("chown", c, err)
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return pathErr("chown", c, err)
	}
	if r.foreign(&st) {
		return pathErr("chown", c, ErrForeignLink)
	}
	if err := unix.Fchownat(fd, "", uid, gid, unix.AT_EMPTY_PATH); err != nil {
		return pathErr("chown", c, err)
	}
	return nil
}

// Chmod sets the mode of the regular file or directory rel through a
// descriptor; a symlink at rel is refused.
func (r *Root) Chmod(rel string, mode os.FileMode) error {
	c, err := clean(rel)
	if err != nil {
		return err
	}
	if err := r.refuseProtected("chmod", c); err != nil {
		return err
	}
	fd, err := r.open(c, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return pathErr("chmod", c, err)
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return pathErr("chmod", c, err)
	}
	if t := st.Mode & unix.S_IFMT; t != unix.S_IFREG && t != unix.S_IFDIR {
		return pathErr("chmod", c, ErrNotRegular)
	}
	if r.foreign(&st) {
		return pathErr("chmod", c, ErrForeignLink)
	}
	if err := unix.Fchmod(fd, uint32(mode.Perm())); err != nil {
		return pathErr("chmod", c, err)
	}
	return nil
}

// ---------------------------------------------------------------- stat --

type fileInfo struct {
	name string
	st   unix.Stat_t
}

func newInfo(name string, st *unix.Stat_t) fs.FileInfo { return &fileInfo{name: name, st: *st} }

func (fi *fileInfo) Name() string { return fi.name }
func (fi *fileInfo) Size() int64  { return fi.st.Size }
func (fi *fileInfo) Mode() fs.FileMode {
	m := fs.FileMode(fi.st.Mode & 0o777)
	switch fi.st.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
		m |= fs.ModeDir
	case unix.S_IFLNK:
		m |= fs.ModeSymlink
	case unix.S_IFIFO:
		m |= fs.ModeNamedPipe
	case unix.S_IFSOCK:
		m |= fs.ModeSocket
	case unix.S_IFBLK:
		m |= fs.ModeDevice
	case unix.S_IFCHR:
		m |= fs.ModeDevice | fs.ModeCharDevice
	}
	if fi.st.Mode&unix.S_ISUID != 0 {
		m |= fs.ModeSetuid
	}
	if fi.st.Mode&unix.S_ISGID != 0 {
		m |= fs.ModeSetgid
	}
	if fi.st.Mode&unix.S_ISVTX != 0 {
		m |= fs.ModeSticky
	}
	return m
}
func (fi *fileInfo) ModTime() time.Time { return time.Unix(fi.st.Mtim.Sec, fi.st.Mtim.Nsec) }
func (fi *fileInfo) IsDir() bool        { return fi.st.Mode&unix.S_IFMT == unix.S_IFDIR }

// Sys is the *unix.Stat_t of the entry.
func (fi *fileInfo) Sys() any { return &fi.st }

func (fi *fileInfo) String() string { return fmt.Sprintf("%s %v %d", fi.name, fi.Mode(), fi.st.Size) }
