package system

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/cloudground-it/cloudground/internal/site"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	installfiles "github.com/cloudground-it/cloudground/internal/install/managed"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

// CheckDrift hashes the managed files the panel recorded and lists those
// that changed outside it (SPEC.md §6). A path outside the managed places is
// handed back unopened: the agent is root, and this must not be a hash oracle.
func (o *Ops) CheckDrift(ctx context.Context, p rpc.DriftParams) (rpc.DriftResult, error) {
	var res rpc.DriftResult
	for path, want := range p.Expected {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if !o.managed(path) {
			res.NotManaged = append(res.NotManaged, path)
			continue
		}
		got, err := o.hashManaged(path)
		if err != nil {
			return res, err
		}
		if got != want {
			res.Drifted = append(res.Drifted, rpc.DriftedFile{Path: path, ExpectedHash: want, ActualHash: got})
		}
	}
	return res, nil
}

// managed reports whether path is one of the places the panel writes.
func (o *Ops) managed(path string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	// The installer's configuration files: one list, shared with the
	// installer and `cgctl doctor` (internal/install/SPEC.md §7).
	if installfiles.Has(path) {
		return true
	}
	below := func(dir string) bool {
		return dir != "" && strings.HasPrefix(path, filepath.Clean(dir)+"/")
	}
	for _, dir := range []string{o.Paths.NginxSites, o.Paths.NginxConfDir, o.Paths.MariaDBConf, o.Paths.PHPMasterDir, o.Paths.NginxAuthDir} {
		if below(dir) {
			return true
		}
	}
	name := filepath.Base(path)
	if o.Paths.SystemdDir != "" && filepath.Dir(path) == filepath.Clean(o.Paths.SystemdDir) &&
		(strings.HasPrefix(name, "cloudground-php-") || strings.HasPrefix(name, "cloudground-app-")) && strings.HasSuffix(name, ".service") {
		return true
	}
	// A site's slice unit, with its limits.
	if o.Paths.SystemdDir != "" && filepath.Dir(path) == filepath.Clean(o.Paths.SystemdDir) && site.ValidSystemUser(strings.TrimSuffix(name, ".slice")) &&
		strings.HasSuffix(name, ".slice") {
		return true
	}
	// A site master's drop-ins (its slice, R-3.7).
	if dir := filepath.Dir(path); o.Paths.SystemdDir != "" && filepath.Dir(dir) == filepath.Clean(o.Paths.SystemdDir) &&
		strings.HasPrefix(filepath.Base(dir), "cloudground-php-") && strings.HasSuffix(dir, ".service.d") && strings.HasSuffix(name, ".conf") {
		return true
	}
	return below(o.Paths.SitesRoot) && strings.Contains(path, "/wp-content/mu-plugins/")
}

// hashManaged hashes a managed file. One inside a site tree (the connector)
// is opened through sitefs, beneath the site's root: the directories on the
// way belong to the site user, and a symlinked mu-plugins/ would otherwise
// have root hash a file of the site's choosing elsewhere on the host.
func (o *Ops) hashManaged(path string) (string, error) {
	sites := filepath.Clean(o.Paths.SitesRoot) + "/"
	if o.Paths.SitesRoot == "" || !strings.HasPrefix(path, sites) {
		return sha256File(path)
	}
	domain, rel, ok := strings.Cut(strings.TrimPrefix(path, sites), "/")
	if !ok || domain == "" {
		return "", nil
	}
	root, err := sitefs.Open(sites+domain, sitefs.Owner{})
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	defer root.Close()
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		// Gone, a link at its name, a FIFO, or a path that leaves the site:
		// not the file the panel wrote, reported as drift with no hash.
		return "", nil
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return "", nil
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// sha256File hashes a file without following a symlink at its name. A file
// that is gone, or that is no longer a regular file, hashes as "": the
// connector's directory belongs to the site, whose user can put a FIFO there,
// and a blocking open of one hung the check for good.
func sha256File(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ELOOP) {
		// Gone, or a symlink now stands at its name: either way not the
		// file the panel wrote, and not a reason to fail every site's check.
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", nil
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
