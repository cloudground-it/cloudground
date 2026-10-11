package release

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudground-it/cloudground/internal/state"
)

// R-4.5: a release built from a repository the site user checked out in
// its shell runs nothing of the repository as root: no git (whose hooks
// and core.fsmonitor run code from the repository), no composer, no hook,
// and a link in the checkout is copied as a link, never followed.
func TestDeployFromACheckoutRunsNothingOfIt(t *testing.T) {
	f := newSite(t, state.SiteStatic)
	marker := filepath.Join(t.TempDir(), "ran")
	script := "#!/bin/sh\ntouch " + marker + "\n"
	// git and composer on the PATH, as on the box: any call would run them.
	bin := t.TempDir()
	for _, tool := range []string{"git", "composer"} {
		if err := os.WriteFile(filepath.Join(bin, tool), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("root's"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := f.path("deploy/build")
	mkTree(t, src, map[string]string{
		".git/config":            "[core]\n\tfsmonitor = " + filepath.Join(bin, "git") + "\n",
		".git/HEAD":              "ref: refs/heads/main\n",
		"composer.json":          `{"scripts":{"post-install-cmd":"touch ` + marker + `"}}`,
		".git/hooks/post-commit": script,
	})
	if err := os.Chmod(filepath.Join(src, ".git/hooks/post-commit"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(src, "leak")); err != nil {
		t.Fatal(err)
	}
	id := ids(1)[0]
	f.mustDeploy(t, id)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("building the release ran something of the checkout")
	}
	rel := f.path("releases/" + id)
	if l, err := os.Readlink(filepath.Join(rel, "leak")); err != nil || l != secret {
		t.Fatalf("the checkout's link was not copied as a link: %q %v", l, err)
	}
	if b, err := os.ReadFile(filepath.Join(rel, ".git/config")); err != nil || len(b) == 0 {
		t.Fatalf("the repository was not copied as files: %v", err)
	}
}
