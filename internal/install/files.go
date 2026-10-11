package install

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// ident resolves a user and a group name to ids: "root" is 0, "" is -1
// (leave as it is).
func (e *Env) ident(user, group string) (uid, gid int, err error) {
	uid, gid = -1, -1
	switch user {
	case "":
	case "root":
		uid = 0
	default:
		u, _, ok := e.Acct.User(user)
		if !ok {
			return 0, 0, fmt.Errorf("no user %s", user)
		}
		uid = u
	}
	switch group {
	case "":
	case "root":
		gid = 0
	default:
		g, ok := e.Acct.Group(group)
		if !ok {
			return 0, 0, fmt.Errorf("no group %s", group)
		}
		gid = g
	}
	return uid, gid, nil
}

// want is what a file or directory must look like: a nil content is not
// compared, an empty user or group is left as it is ("root" is root).
type want struct {
	content     []byte
	mode        fs.FileMode
	user, group string
}

// fileDiff says how the file at p differs from w ("" = it does not).
func (e *Env) fileDiff(p string, w want) string {
	fi, err := e.stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return p + ": missing"
	}
	if err != nil {
		return p + ": " + err.Error()
	}
	if !fi.Mode().IsRegular() {
		return p + ": not a regular file"
	}
	if w.content != nil {
		got, err := e.readFile(p)
		if err != nil {
			return p + ": " + err.Error()
		}
		if !bytes.Equal(got, w.content) {
			return p + ": content differs"
		}
	}
	if fi.Mode().Perm() != w.mode {
		return fmt.Sprintf("%s: mode %04o, want %04o", p, fi.Mode().Perm(), w.mode)
	}
	return e.ownerDiff(p, w)
}

func (e *Env) ownerDiff(p string, w want) string {
	if w.user == "" && w.group == "" {
		return ""
	}
	uid, gid, err := e.ident(w.user, w.group)
	if err != nil {
		return p + ": " + err.Error()
	}
	gu, gg, err := e.FS.Owner(e.path(p))
	if err != nil {
		return p + ": " + err.Error()
	}
	if (uid >= 0 && gu != uid) || (gid >= 0 && gg != gid) {
		return fmt.Sprintf("%s: owner %d:%d, want %s:%s", p, gu, gg, orAny(w.user), orAny(w.group))
	}
	return ""
}

func orAny(s string) string {
	if s == "" {
		return "*"
	}
	return s
}

// writeFile puts content at p with its mode and owner, through a temporary
// file in the same directory and a rename: a reader sees the old file or the
// new one, never half of it, and a running binary is never written in place.
func (e *Env) writeFile(p string, content []byte, w want) error {
	full := e.path(p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	tmp, err := createTemp(full)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	_, werr := f.Write(content)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = e.own(tmp, w)
	}
	if werr == nil {
		werr = os.Chmod(tmp, w.mode)
	}
	if werr == nil {
		werr = e.FS.Rename(tmp, full)
	}
	if werr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write %s: %w", p, werr)
	}
	e.logf("wrote %s (%04o)", p, w.mode)
	return nil
}

// own sets the owner of a host path (already inside Root) where w names one.
func (e *Env) own(full string, w want) error {
	if w.user == "" && w.group == "" {
		return nil
	}
	uid, gid, err := e.ident(w.user, w.group)
	if err != nil {
		return err
	}
	return e.FS.Chown(full, uid, gid)
}

// dirDiff says how directory p differs from w ("" = it does not).
func (e *Env) dirDiff(p string, w want) string {
	fi, err := e.stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return p + ": missing"
	}
	if err != nil {
		return p + ": " + err.Error()
	}
	if !fi.IsDir() {
		return p + ": not a directory"
	}
	if fi.Mode().Perm() != w.mode {
		return fmt.Sprintf("%s: mode %04o, want %04o", p, fi.Mode().Perm(), w.mode)
	}
	return e.ownerDiff(p, w)
}

// ensureDir creates p (and its parents, 0755) or fixes its mode and owner.
// The directory is opened without following a symlink at its name and its
// mode set through that descriptor: a link planted there is refused, never
// chmod-ed through (the owner is set with lchown).
func (e *Env) ensureDir(p string, w want) error {
	full := e.path(p)
	if fi, err := os.Lstat(full); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink: refusing to use it as a directory", p)
	}
	if err := os.MkdirAll(full, 0o755); err != nil {
		return err
	}
	d, err := os.OpenFile(full, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0)
	if err != nil {
		return fmt.Errorf("%s: %w", p, err)
	}
	defer d.Close()
	if err := e.own(full, w); err != nil {
		return fmt.Errorf("%s: %w", p, err)
	}
	return d.Chmod(w.mode)
}

// createTemp makes an empty 0600 file with a random name beside full
// (O_EXCL: never an existing file or a planted link) and returns its path.
func createTemp(full string) (string, error) {
	for i := 0; i < 10; i++ {
		var rnd [8]byte
		if _, err := rand.Read(rnd[:]); err != nil {
			return "", err
		}
		tmp := filepath.Join(filepath.Dir(full), "."+filepath.Base(full)+".cg-"+hex.EncodeToString(rnd[:]))
		f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		return tmp, f.Close()
	}
	return "", fmt.Errorf("%s: cannot create a temporary file", full)
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// fileSHA256 hashes the file at p ("" when it cannot be read).
func (e *Env) fileSHA256(p string) string {
	b, err := e.readFile(p)
	if err != nil {
		return ""
	}
	return sha256Hex(b)
}
