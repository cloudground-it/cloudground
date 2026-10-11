package wp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/cloudground-it/cloudground/connector"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// The drop-in's place in the site layout. The link is relative, so it
// resolves beneath the site root, and in a staging copy to the copy's own
// shared/.
const (
	liveLink     = "current"
	DropinLink   = "wp-content/object-cache.php"
	DropinTarget = "../../../shared/object-cache.php"
	sharedDropin = "shared/object-cache.php"
	// sharedEpoch is the drop-in's key namespace; the drop-in reads it from
	// its own (resolved) directory.
	sharedEpoch = "shared/.cloudground-cache-epoch"
)

// ObjectCache turns the APCu object cache on or off (SPEC §6). The RPC
// server holds the site's lock.
func (o *Ops) ObjectCache(ctx context.Context, p rpc.WPParams) (map[string]string, error) {
	if p.What != "" && p.What != "apcu" {
		return nil, agentrpc.Invalid("unsupported object-cache backend %q", p.What)
	}
	if !p.Enable {
		if err := o.setDropin(p.Site, false); err != nil {
			return nil, err
		}
		if err := o.PHP.Reload(ctx, p.Site); err != nil {
			return nil, fmt.Errorf("reload PHP: %w", err)
		}
		return map[string]string{"object_cache": "disabled"}, nil
	}
	if err := o.InstallObjectCache(ctx, p.Site); err != nil {
		return nil, err
	}
	return map[string]string{"object_cache": "enabled", "backend": "apcu"}, nil
}

// InstallObjectCache writes the APCu drop-in to shared/, starts a fresh key
// namespace, links the drop-in into the live release and reloads PHP. Site
// creation calls it directly.
func (o *Ops) InstallObjectCache(ctx context.Context, s state.Site) error {
	if err := o.setDropin(s, true); err != nil {
		return err
	}
	// The reload makes OPcache see the new file; a stale cached copy of an
	// old drop-in could otherwise fatal the site.
	if err := o.PHP.Reload(ctx, s); err != nil {
		return fmt.Errorf("reload PHP: %w", err)
	}
	return nil
}

func (o *Ops) setDropin(s state.Site, on bool) error {
	r, err := o.OpenSite(s)
	if err != nil {
		return err
	}
	defer r.Close()
	rel, err := liveRelease(r)
	if err != nil {
		return err
	}
	if on {
		if err := r.Replace(sharedDropin, 0o640, func(f *os.File) error {
			_, err := io.WriteString(f, connector.ObjectCacheAPCu)
			return err
		}); err != nil {
			return fmt.Errorf("write the drop-in: %w", err)
		}
		// Coming back from off, APCu may still hold entries written before
		// the database changed under it: start a namespace they are not in.
		// Already on, the warm cache is kept.
		if _, err := r.Lstat(rel + "/" + DropinLink); errors.Is(err, fs.ErrNotExist) {
			if err := newEpoch(r); err != nil {
				return fmt.Errorf("reset the object-cache namespace: %w", err)
			}
		}
	}
	if err := LinkDropin(r, rel, on); err != nil {
		return fmt.Errorf("link the drop-in: %w", err)
	}
	return nil
}

// liveRelease is the site-relative directory the live link points at,
// "releases/<id>". The link is the site's own layout (root-owned); a target
// outside releases/ is refused.
func liveRelease(r *sitefs.Root) (string, error) {
	t, err := r.Readlink(liveLink)
	if err != nil {
		return "", err
	}
	if path.IsAbs(t) {
		rel, ok := strings.CutPrefix(path.Clean(t), path.Clean(r.Path())+"/")
		if !ok {
			return "", errors.New("the live release points outside the site")
		}
		t = rel
	}
	id, ok := strings.CutPrefix(path.Clean(t), "releases/")
	if !ok || id == "" || id == "." || id == ".." || strings.Contains(id, "/") {
		return "", errors.New("the live release points outside the site's releases")
	}
	return "releases/" + id, nil
}

// newEpoch gives the drop-in a fresh random key namespace. Entries APCu
// still holds from before (the cache was off while the database changed)
// are never read again.
func newEpoch(r *sitefs.Root) error {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	return r.Replace(sharedEpoch, 0o644, func(f *os.File) error {
		_, err := io.WriteString(f, hex.EncodeToString(b))
		return err
	})
}

// LinkDropin makes a release (a directory in r) agree with
// the object-cache setting: linked to the shared drop-in when on, with no
// drop-in at all when off. Off is enforced, not skipped: a release copied
// from a tree that had one would otherwise switch it back on. A release
// without wp-content is left alone.
//
// wp-content is opened once, with no symlink allowed on the way (sitefs
// SubStrict), and the link is removed and made through that descriptor:
// there is no window between the check that it is a real directory and the
// write (SPEC §12).
func LinkDropin(r *sitefs.Root, release string, on bool) error {
	wc, err := r.SubStrict(release + "/wp-content")
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("wp-content is not a real directory; refusing to follow it: %w", err)
	}
	defer wc.Close()
	link := path.Base(DropinLink)
	if err := wc.Remove(link); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if !on {
		return nil
	}
	if _, err := r.Lstat(sharedDropin); err != nil {
		return nil // nothing to link to yet; InstallObjectCache writes it
	}
	return wc.Symlink(DropinTarget, link)
}

// dropinLinked reports whether the live release has the drop-in.
func (o *Ops) dropinLinked(s state.Site) (bool, error) {
	r, err := o.OpenSite(s)
	if err != nil {
		return false, err
	}
	defer r.Close()
	_, err = r.Lstat(liveLink + "/" + DropinLink)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}
