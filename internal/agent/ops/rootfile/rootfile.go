// Package rootfile writes and removes the root-owned files the agent manages
// outside site trees: nginx's configuration, the panel's certificates, the
// certificates operators bring, certbot's credentials. A file is replaced by
// rename (a reader sees the old content or the new, never a partial one), its
// mode and owner are set on the descriptor before it gets its name, and
// nothing here follows a symlink at the final component.
//
// Site trees are not handled here: their owner can change them under us, and
// they go through sitefs, as does removing a tree (sitefs.Root.RemoveAll).
package rootfile

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Keep leaves the owner of a replaced file as the agent creates it (root).
const Keep = -1

// Replace writes data to path with exactly mode, owned by root.
func Replace(path string, data []byte, mode os.FileMode) error {
	return ReplaceOwned(path, data, mode, Keep, Keep)
}

// ReplaceOwned writes data to path with exactly mode, owned by uid:gid (Keep
// leaves either as created). The temporary file is created next to path,
// synced, then renamed over it; the directory is synced after the rename.
func ReplaceOwned(path string, data []byte, mode os.FileMode, uid, gid int) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".cg-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	done := false
	defer func() {
		if !done {
			tmp.Close()
			os.Remove(name)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	// The umask applies at creation; Chmod on the descriptor does not.
	if err := tmp.Chmod(mode.Perm()); err != nil {
		return err
	}
	if uid != Keep || gid != Keep {
		if err := tmp.Chown(uid, gid); err != nil {
			return err
		}
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	done = true
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !errors.Is(err, unix.EINVAL) {
		return err
	}
	return nil
}

// MkdirMode creates dir (and its parents) and sets exactly mode on dir, which
// must be a real directory, not a symlink to one. The parents it creates get
// mode too (MkdirAllMode); the ones that exist are left as they are.
func MkdirMode(dir string, mode os.FileMode) error {
	if err := MkdirAllMode(dir, mode); err != nil {
		return err
	}
	return chmodDir(dir, mode)
}

// MkdirAllMode creates dir and every missing parent, each with exactly mode
// whatever the process's umask: the agent runs with UMask=0027, under which
// os.MkdirAll(p, 0o755) makes 0750 and leaves a directory that nginx's
// workers, a site's user or MariaDB must traverse closed to them. A
// directory that already exists, dir included, is not changed.
func MkdirAllMode(dir string, mode os.FileMode) error {
	dir = filepath.Clean(dir)
	var missing []string
	for p := dir; ; p = filepath.Dir(p) {
		fi, err := os.Stat(p)
		if err == nil {
			if !fi.IsDir() {
				return &fs.PathError{Op: "mkdir", Path: p, Err: unix.ENOTDIR}
			}
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		missing = append(missing, p)
		if filepath.Dir(p) == p {
			break
		}
	}
	for i := len(missing) - 1; i >= 0; i-- {
		p := missing[i]
		if err := os.Mkdir(p, mode.Perm()); err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue // made meanwhile by someone else: left as it is
			}
			return err
		}
		if err := chmodDir(p, mode); err != nil {
			return err
		}
	}
	return nil
}

// chmodDir sets mode on the real directory dir through a descriptor opened
// without following a symlink at its name.
func chmodDir(dir string, mode os.FileMode) error {
	f, err := os.OpenFile(dir, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_DIRECTORY|unix.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			return fmt.Errorf("%s is not a directory", dir)
		}
		return err
	}
	defer f.Close()
	return f.Chmod(mode.Perm())
}

// ReadNoFollow reads a whole file, refusing a symlink at its name. A missing
// file is (nil, os.ErrNotExist).
func ReadNoFollow(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	return io.ReadAll(f)
}
