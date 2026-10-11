package sitefs

import (
	"errors"
	"path"

	"golang.org/x/sys/unix"
)

// SubStrict opens the directory rel beneath r as a Root of its own, like
// Sub, but no component of rel may be a symlink (RESOLVE_NO_SYMLINKS): what
// is checked to be a real directory is the directory then written in, with
// no window for a link to be swapped in between. A link anywhere on rel is
// ErrSymlink. Symlinks the new Root creates are checked against r's tree
// (their target may climb out of rel as long as it stays beneath r).
func (r *Root) SubStrict(rel string) (*Root, error) {
	c, err := clean(rel)
	if err != nil {
		return nil, err
	}
	if r.h.fd < 0 {
		return nil, unix.EBADF
	}
	fd, err := openat2(r.h.fd, c, unix.O_PATH|unix.O_DIRECTORY, 0, beneath|unix.RESOLVE_NO_SYMLINKS)
	if errors.Is(err, unix.ENOSYS) {
		return nil, ErrUnsupported
	}
	if err != nil {
		return nil, pathErr("open", c, err)
	}
	return &Root{h: &handle{fd: fd, path: path.Join(r.h.path, c)}, owner: r.owner, prot: r.prot,
		prefix: path.Join(r.prefix, c)}, nil
}
