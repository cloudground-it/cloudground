package sitefs

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path"

	"golang.org/x/sys/unix"
)

// WalkFunc is called for each entry Walk visits, with its root-relative path
// and its own (not followed) description.
type WalkFunc func(rel string, info fs.FileInfo) error

// top locates the start of a tree operation as a directory descriptor and a
// name in it. For the root itself that is the root's descriptor and ".".
// done releases what top opened.
func (r *Root) top(rel string) (dirfd int, name, c string, done func(), err error) {
	c, err = clean(rel)
	if err != nil {
		return -1, "", "", nil, err
	}
	if c == "." {
		return r.h.fd, ".", c, func() {}, nil
	}
	pfd, base, c, err := r.parent(c, unix.O_PATH)
	if err != nil {
		return -1, "", c, nil, err
	}
	return pfd, base, c, func() { unix.Close(pfd) }, nil
}

func (r *Root) topStat(rel string) (int, string, string, func(), *unix.Stat_t, error) {
	dirfd, name, c, done, err := r.top(rel)
	if err != nil {
		return -1, "", c, nil, nil, err
	}
	var st unix.Stat_t
	if err := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		done()
		return -1, "", c, nil, nil, pathErr("stat", c, err)
	}
	if st.Mode&unix.S_IFMT == unix.S_IFLNK {
		done()
		return -1, "", c, nil, nil, pathErr("walk", c, ErrSymlink)
	}
	return dirfd, name, c, done, &st, nil
}

func openDirAt(dirfd int, name string) (*os.File, error) {
	fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func join(dir, name string) string {
	if dir == "." {
		return name
	}
	return dir + "/" + name
}

// Walk visits rel and everything beneath it in pre-order, names sorted,
// never following a symlink. fn may return fs.SkipDir or fs.SkipAll.
func (r *Root) Walk(rel string, fn WalkFunc) error {
	dirfd, name, c, done, st, err := r.topStat(rel)
	if err != nil {
		return err
	}
	defer done()
	err = fn(c, newInfo(path.Base(c), st))
	if err != nil {
		if errors.Is(err, fs.SkipDir) || errors.Is(err, fs.SkipAll) {
			return nil
		}
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return nil
	}
	d, err := openDirAt(dirfd, name)
	if err != nil {
		return pathErr("walk", c, err)
	}
	defer d.Close()
	if err := walkDir(d, c, fn); err != nil && !errors.Is(err, fs.SkipAll) {
		return err
	}
	return nil
}

func walkDir(d *os.File, rel string, fn WalkFunc) error {
	names, err := readNames(d)
	if err != nil {
		return pathErr("walk", rel, err)
	}
	fd := int(d.Fd())
	for _, n := range names {
		var st unix.Stat_t
		if err := unix.Fstatat(fd, n, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			return pathErr("walk", join(rel, n), err)
		}
		child := join(rel, n)
		err := fn(child, newInfo(n, &st))
		isDir := st.Mode&unix.S_IFMT == unix.S_IFDIR
		if errors.Is(err, fs.SkipDir) {
			if isDir {
				continue
			}
			return nil
		}
		if err != nil {
			return err
		}
		if !isDir {
			continue
		}
		cd, err := openDirAt(fd, n)
		if err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			// ELOOP or ENOTDIR: swapped for something else since the stat.
			return pathErr("walk", child, err)
		}
		err = walkDir(cd, child, fn)
		cd.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// postOrder calls fn for every entry beneath rel and then for rel itself,
// each as (parent descriptor, name), descending only through descriptors
// opened O_NOFOLLOW.
func (r *Root) postOrder(rel string, fn func(dirfd int, name string, st *unix.Stat_t) error) error {
	dirfd, name, c, done, st, err := r.topStat(rel)
	if err != nil {
		return err
	}
	defer done()
	if err := postEntry(dirfd, name, st, fn); err != nil {
		return pathErr("walk", c, err)
	}
	return nil
}

func postEntry(dirfd int, name string, st *unix.Stat_t, fn func(int, string, *unix.Stat_t) error) error {
	if st.Mode&unix.S_IFMT == unix.S_IFDIR {
		d, err := openDirAt(dirfd, name)
		if err != nil {
			return err
		}
		names, err := readNames(d)
		if err != nil {
			d.Close()
			return err
		}
		fd := int(d.Fd())
		for _, n := range names {
			var cst unix.Stat_t
			if err := unix.Fstatat(fd, n, &cst, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				if errors.Is(err, unix.ENOENT) {
					continue
				}
				d.Close()
				return err
			}
			if err := postEntry(fd, n, &cst, fn); err != nil {
				d.Close()
				return err
			}
		}
		d.Close()
	}
	// The directory itself last, so a mode change cannot lock the walk out.
	return fn(dirfd, name, st)
}

// ChownTree gives rel and everything beneath it to the Root's owner, never
// following a symlink (a link is re-owned as a link). A foreign link (a
// hard link to someone else's file, maybe outside this tree) is left
// alone; the check is on the descriptor the change is made through.
func (r *Root) ChownTree(rel string) error {
	if err := r.refuseProtected("chown", rel); err != nil {
		return err
	}
	uid, gid := r.owner.UID, r.owner.GID
	return r.postOrder(rel, func(dirfd int, name string, _ *unix.Stat_t) error {
		fd, err := unix.Openat(dirfd, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		defer unix.Close(fd)
		var now unix.Stat_t
		if err := unix.Fstat(fd, &now); err != nil {
			return err
		}
		if r.foreign(&now) {
			return nil
		}
		return unix.Fchownat(fd, "", uid, gid, unix.AT_EMPTY_PATH)
	})
}

// ChmodTree clears the bits in clear on every directory and regular file at
// or beneath rel, through descriptors, never following a symlink.
func (r *Root) ChmodTree(rel string, clear os.FileMode) error {
	if err := r.refuseProtected("chmod", rel); err != nil {
		return err
	}
	mask := uint32(clear.Perm())
	return r.postOrder(rel, func(dirfd int, name string, st *unix.Stat_t) error {
		t := st.Mode & unix.S_IFMT
		if t != unix.S_IFDIR && t != unix.S_IFREG {
			return nil
		}
		fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		defer unix.Close(fd)
		var now unix.Stat_t
		if err := unix.Fstat(fd, &now); err != nil {
			return err
		}
		if now.Mode&unix.S_IFMT != t {
			return ErrNotRegular
		}
		if r.foreign(&now) {
			return nil
		}
		return unix.Fchmod(fd, (now.Mode&0o7777)&^mask)
	})
}

// CopyTree copies srcRel in src to dstRel in dst, which must not exist yet.
// Nothing is followed: srcRel must be a real directory or regular file,
// symlinks inside are copied as links, and every file is copied as content
// (a hard link is never created). Everything created belongs to dst's owner
// and keeps the source's permission bits and modification time.
func CopyTree(src *Root, srcRel string, dst *Root, dstRel string) error {
	sdirfd, sname, sc, sdone, st, err := src.topStat(srcRel)
	if err != nil {
		return err
	}
	defer sdone()
	dpfd, dbase, dc, err := dst.parentW(dstRel, unix.O_PATH)
	if err != nil {
		return err
	}
	defer unix.Close(dpfd)
	c := copier{o: dst.owner}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFREG:
		if err := c.file(sdirfd, sname, dpfd, dbase); err != nil {
			return pathErr("copy", sc, err)
		}
		return nil
	case unix.S_IFDIR:
	default:
		return pathErr("copy", sc, ErrNotRegular)
	}
	if err := c.dir(sdirfd, sname, st, dpfd, dbase); err != nil {
		return &fs.PathError{Op: "copy", Path: sc + " -> " + dc, Err: mapErr(err)}
	}
	return nil
}

type copier struct{ o Owner }

func times(st *unix.Stat_t) []unix.Timespec { return []unix.Timespec{st.Atim, st.Mtim} }

// dir copies the source directory (sdirfd, sname) to a new directory
// (ddirfd, dname): created 0700, filled, then given its owner, mode and time.
func (c copier) dir(sdirfd int, sname string, st *unix.Stat_t, ddirfd int, dname string) error {
	if err := unix.Mkdirat(ddirfd, dname, 0o700); err != nil {
		return err
	}
	s, err := openDirAt(sdirfd, sname)
	if err != nil {
		return err
	}
	defer s.Close()
	d, err := openDirAt(ddirfd, dname)
	if err != nil {
		return err
	}
	defer d.Close()
	names, err := readNames(s)
	if err != nil {
		return err
	}
	sfd, dfd := int(s.Fd()), int(d.Fd())
	for _, n := range names {
		var cst unix.Stat_t
		if err := unix.Fstatat(sfd, n, &cst, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			return err
		}
		switch cst.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			err = c.dir(sfd, n, &cst, dfd, n)
		case unix.S_IFREG:
			err = c.file(sfd, n, dfd, n)
		case unix.S_IFLNK:
			err = c.link(sfd, n, &cst, dfd, n)
		}
		if err != nil {
			return err
		}
	}
	if err := unix.Fchown(dfd, c.o.UID, c.o.GID); err != nil {
		return err
	}
	if err := unix.Fchmod(dfd, st.Mode&0o777); err != nil {
		return err
	}
	return unix.UtimesNanoAt(ddirfd, dname, times(st), unix.AT_SYMLINK_NOFOLLOW)
}

// file copies the content of a regular file into a new file. The source is
// opened without following a link and non-blocking, and checked again on
// the descriptor: an entry swapped since it was listed is skipped.
func (c copier) file(sdirfd int, sname string, ddirfd int, dname string) error {
	ifd, err := unix.Openat(sdirfd, sname, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENXIO) {
		return nil
	}
	if err != nil {
		return err
	}
	in := os.NewFile(uintptr(ifd), sname)
	defer in.Close()
	var st unix.Stat_t
	if err := unix.Fstat(ifd, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil
	}
	if err := unix.SetNonblock(ifd, false); err != nil {
		return err
	}
	ofd, err := unix.Openat(ddirfd, dname, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	out := os.NewFile(uintptr(ofd), dname)
	_, err = io.Copy(out, in)
	if err == nil {
		err = unix.Fchown(ofd, c.o.UID, c.o.GID)
	}
	if err == nil {
		err = unix.Fchmod(ofd, st.Mode&0o777)
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return unix.UtimesNanoAt(ddirfd, dname, times(&st), unix.AT_SYMLINK_NOFOLLOW)
}

// link copies a symlink's target text, unchanged.
func (c copier) link(sdirfd int, sname string, st *unix.Stat_t, ddirfd int, dname string) error {
	buf := make([]byte, 4096)
	n, err := unix.Readlinkat(sdirfd, sname, buf)
	if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.EINVAL) {
		return nil // gone, or no longer a link
	}
	if err != nil {
		return err
	}
	if err := unix.Symlinkat(string(buf[:n]), ddirfd, dname); err != nil {
		return err
	}
	if err := unix.Fchownat(ddirfd, dname, c.o.UID, c.o.GID, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	return unix.UtimesNanoAt(ddirfd, dname, times(st), unix.AT_SYMLINK_NOFOLLOW)
}
