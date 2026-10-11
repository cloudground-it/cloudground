package sites

import (
	"context"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agent/ops/staging"
	"github.com/cloudground-it/cloudground/internal/state"
)

// withAgentUmask runs fn under the agent's UMask=0027 (internal/units), the
// umask every directory and file the agent creates on a server is made
// under.
func withAgentUmask(t *testing.T, fn func()) {
	t.Helper()
	old := syscall.Umask(0o027)
	defer syscall.Umask(old)
	fn()
}

// wantMode fails when path is not there with exactly mode.
func wantMode(t *testing.T, what, path string, mode os.FileMode) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Errorf("%s (%s): %v", what, path, err)
		return
	}
	if fi.Mode().Perm() != mode {
		t.Errorf("%s (%s): mode %04o, want %04o", what, path, fi.Mode().Perm(), mode)
	}
}

// Every directory a site's creation makes, on a server where none of them
// exists yet, has the mode its readers need, not what UMask=0027 leaves: a
// fresh Ubuntu 26.04 made /etc/cloudground-php 0750, and every PHP site
// failed its FPM test as the site user ("failed to open configuration
// file ... Permission denied").
func TestCreateUnderTheAgentUmaskSetsEveryMode(t *testing.T) {
	e := newEnv(t)
	e.installPHP(t, "8.4")
	// A fresh server: nothing of the agent's yet, not even the roots.
	for _, d := range []string{e.Paths.PHPMasterDir, e.Paths.NginxAuthDir, e.Paths.LogRoot, e.Paths.CacheRoot,
		e.Paths.SitesRoot, filepath.Join(e.root, "nginx")} {
		if err := os.RemoveAll(d); err != nil {
			t.Fatal(err)
		}
	}
	s := e.site(2, "php.example.com", state.SitePHP)
	s.Config.CacheEnabled = true
	s.Config.StagingAuthHash = "$2a$10$" + strings.Repeat("d", 53)
	withAgentUmask(t, func() { e.create(t, s) })

	// Read by the site's PHP master and its configuration test, as the site
	// user: the directory open to all, the pool to the site's group.
	wantMode(t, "PHP masters' directory", e.Paths.PHPMasterDir, 0o755)
	wantMode(t, "pool", e.poolPath(s), 0o640)
	wantMode(t, "unit", e.unitPath(s), 0o644)
	wantMode(t, "unit drop-in directory", e.dropinDir(s), 0o755)
	wantMode(t, "unit drop-in", e.dropinPath(s), 0o644)
	// Read by nginx's master (root): created 0755 like the package's.
	wantMode(t, "nginx sites directory", e.Paths.NginxSites, 0o755)
	wantMode(t, "nginx conf.d", e.Paths.NginxConfDir, 0o755)
	wantMode(t, "vhost", e.vhostPath(s), 0o600) // holds the staging key
	wantMode(t, "cache zone", e.cacheZonePath(s), 0o644)
	// Read by nginx's workers (www-data): the password file 0640, its
	// directory root:www-data 0750 where the group exists.
	auth := staging.AuthFile(e.Paths.NginxAuthDir, s.Domain)
	wantMode(t, "staging password file", auth, 0o640)
	authDir := os.FileMode(0o755)
	if _, err := user.LookupGroup("www-data"); err == nil {
		authDir = 0o750
	}
	wantMode(t, "staging password directory", e.Paths.NginxAuthDir, authDir)
	// Traversed by nginx's workers and every site's user.
	wantMode(t, "sites' root", e.Paths.SitesRoot, 0o755)
	wantMode(t, "site root", s.RootPath, 0o750)
	for _, d := range []string{"releases", "shared", "shared/uploads", "logs", "tmp", "tmp/sessions"} {
		wantMode(t, "site "+d, filepath.Join(s.RootPath, d), 0o750)
	}
	wantMode(t, "logs' root", e.Paths.LogRoot, 0o755)
	wantMode(t, "site log directory", filepath.Join(e.Paths.LogRoot, s.Domain), 0o750)
	wantMode(t, "PHP error log", filepath.Join(e.Paths.LogRoot, s.Domain, "php-error.log"), 0o640)
	wantMode(t, "page caches' root", e.Paths.CacheRoot, 0o755)
	wantMode(t, "page cache", e.input(context.Background(), s).CacheDir, 0o700)
}

// A masters' directory left 0750 by a build before the fix is opened again
// by the next render of a PHP site.
func TestRenderReopensAClosedPHPMasterDir(t *testing.T) {
	e := newEnv(t)
	e.installPHP(t, "8.4")
	s := e.site(3, "fix.example.com", state.SitePHP)
	e.create(t, s)
	if err := os.Chmod(e.Paths.PHPMasterDir, 0o750); err != nil {
		t.Fatal(err)
	}
	withAgentUmask(t, func() {
		if _, err := e.Render(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	})
	wantMode(t, "PHP masters' directory", e.Paths.PHPMasterDir, 0o755)
}
