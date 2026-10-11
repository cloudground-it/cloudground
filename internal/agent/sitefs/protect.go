package sitefs

import (
	"errors"

	"golang.org/x/sys/unix"
)

// ErrProtected: a protected Root refused to change an entry of the site's
// skeleton (SPEC "Protected views").
var ErrProtected = errors.New("the entry is part of the site's skeleton and cannot be changed here")

// protection is what a protected Root refuses: an entry named in names
// directly in the directory the protection was made on (dev, ino), and any
// entry of a directory root owns other than that one.
type protection struct {
	dev, ino uint64
	names    map[string]bool
	// rootOwner: the Root Protect was called on created for root itself
	// (a view made later with WithOwner does not change this).
	rootOwner bool
}

// Protect returns a view of r (same descriptor, same owner) that refuses to
// create, replace, remove, rename or re-own the entries names directly in
// r's directory, and, when r's owner is not root, any entry directly in a
// directory owned by root other than r's own (a site's .ssh, releases/, a
// build directory). The check is
// made on the parent directory's descriptor that the operation then uses,
// so a symlink swapped in on the way cannot move the operation past it.
// Chmod and the tree operations that change ownership or modes are refused
// outright on a protected view.
func (r *Root) Protect(names ...string) (*Root, error) {
	var st unix.Stat_t
	if err := unix.Fstat(r.h.fd, &st); err != nil {
		return nil, err
	}
	p := &protection{dev: uint64(st.Dev), ino: uint64(st.Ino), names: map[string]bool{}, rootOwner: r.owner.UID == 0}
	for _, n := range names {
		p.names[n] = true
	}
	return &Root{h: r.h, owner: r.owner, prot: p, prefix: r.prefix}, nil
}

// guard checks a change of the entry name in the directory pfd.
func (r *Root) guard(pfd int, name, c string) error {
	if r.prot == nil {
		return nil
	}
	var st unix.Stat_t
	if err := unix.Fstat(pfd, &st); err != nil {
		return pathErr("stat", c, err)
	}
	if uint64(st.Dev) == r.prot.dev && uint64(st.Ino) == r.prot.ino {
		if r.prot.names[name] {
			return pathErr("change", c, ErrProtected)
		}
		return nil
	}
	// A folder root owns in a tree that is someone else's is the skeleton's
	// (a Root that creates for root itself has only the name rule).
	if st.Uid == 0 && !r.prot.rootOwner {
		return pathErr("change", c, ErrProtected)
	}
	return nil
}

// parentW is parent for an operation that changes the final name: on a
// protected Root the parent's descriptor is checked first.
func (r *Root) parentW(rel string, flags int) (int, string, string, error) {
	pfd, base, c, err := r.parent(rel, flags)
	if err != nil {
		return pfd, base, c, err
	}
	if err := r.guard(pfd, base, c); err != nil {
		unix.Close(pfd)
		return -1, "", c, err
	}
	return pfd, base, c, nil
}

// refuseProtected refuses an operation a protected Root never does.
func (r *Root) refuseProtected(op, c string) error {
	if r.prot != nil {
		return pathErr(op, c, ErrProtected)
	}
	return nil
}
