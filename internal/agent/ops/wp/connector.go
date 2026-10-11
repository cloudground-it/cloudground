package wp

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"

	"github.com/cloudground-it/cloudground/connector"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
)

// ConnectorPlugin is where the connector lives in a WordPress tree.
const ConnectorPlugin = "wp-content/mu-plugins/cloudground-connector.php"

// InstallConnector writes the cache-invalidation mu-plugin into the
// WordPress tree at release (a directory in r), next to whatever
// mu-plugins the tree already has.
//
// Site creation, deploys and migration all need it. A site without it looks
// healthy and serves cached pages, but nothing ever purges them: an edit
// stays invisible until fastcgi_cache_valid expires, which reads to the
// site owner as "my change did not save".
//
// wp-content and wp-content/mu-plugins must be real directories (SPEC
// §12): each is made when missing, then opened with no symlink allowed on
// the way (sitefs SubStrict), and the plugin is written through that
// descriptor, so a link the tenant swaps in cannot steer the write.
func InstallConnector(r *sitefs.Root, release string) error {
	mu, err := realDir(r, path.Join(release, "wp-content"), "mu-plugins")
	if err != nil {
		return err
	}
	defer mu.Close()
	return mu.Replace(path.Base(ConnectorPlugin), 0o644, func(f *os.File) error {
		_, err := io.WriteString(f, connector.PHP)
		return err
	})
}

// realDir opens each of dirs, in turn, beneath base in r as a real
// directory, making it (0750) when it is missing; a symlink at any of them
// is refused. The caller closes what it returns.
func realDir(r *sitefs.Root, base string, dirs ...string) (*sitefs.Root, error) {
	if _, err := r.Lstat(base); errors.Is(err, fs.ErrNotExist) {
		if err := r.Mkdir(base, 0o750); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
	}
	cur, err := r.SubStrict(base)
	if err != nil {
		return nil, fmt.Errorf("%s is not a real directory: %w", base, err)
	}
	for _, d := range dirs {
		if _, err := cur.Lstat(d); errors.Is(err, fs.ErrNotExist) {
			if err := cur.Mkdir(d, 0o750); err != nil && !errors.Is(err, fs.ErrExist) {
				cur.Close()
				return nil, err
			}
		}
		next, err := cur.SubStrict(d)
		cur.Close()
		if err != nil {
			return nil, fmt.Errorf("%s/%s is not a real directory: %w", base, d, err)
		}
		cur = next
	}
	return cur, nil
}
