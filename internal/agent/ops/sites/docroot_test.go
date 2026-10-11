package sites

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// R-4.3 review: a public root that is, or passes through, a link the site
// planted is refused; a real folder, or one not made yet, passes.
func TestCheckDocRootRefusesLinks(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "a.example.com")
	other := filepath.Join(base, "b.example.com", "shared", "exports")
	for _, d := range []string{filepath.Join(root, "releases", "r1", "web", "inner"), other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("releases/r1", filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(root, "releases", "r1", "x")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("web", filepath.Join(root, "releases", "r1", "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "releases", "r1", "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	o := &Ops{Open: func(s state.Site) (*sitefs.Root, error) {
		return sitefs.Open(s.RootPath, sitefs.Owner{UID: os.Getuid(), GID: os.Getgid()})
	}}
	site := func(pr string) state.Site {
		return state.Site{RootPath: root, Config: state.SiteConfig{PublicRoot: pr}}
	}
	for _, ok := range []string{"", "web", "web/inner", "missing", "web/missing/deeper"} {
		if err := o.checkDocRoot(site(ok)); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"x", "x/sub", "alias", "alias/inner", "file"} {
		err := o.checkDocRoot(site(bad))
		if agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
			t.Errorf("%q = %v, want an invalid error", bad, err)
		}
	}
}
