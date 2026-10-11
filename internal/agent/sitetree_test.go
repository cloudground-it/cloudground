package agent

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agent/ops/wp"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// wpLayout turns a provisioned static site's live release into the layout a
// WordPress site has: wp-config.php and uploads linked into shared/, either
// relative (the layout since R-2.3) or absolute (the one before it).
func wpLayout(t *testing.T, a *Agent, site state.Site, absolute bool) string {
	t.Helper()
	rel := liveRelease(t, a, site)
	cfg := filepath.Join(site.RootPath, "shared", "wp-config.php")
	if err := os.WriteFile(cfg, []byte("<?php define('DB_NAME', 'production');\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rel, "wp-content"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgTarget, upTarget := wp.ConfigTarget, wp.UploadsTarget
	if absolute {
		cfgTarget, upTarget = cfg, filepath.Join(site.RootPath, "shared", "uploads")
		os.Remove(filepath.Join(site.RootPath, "current"))
		if err := os.Symlink(rel, filepath.Join(site.RootPath, "current")); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(cfgTarget, filepath.Join(rel, "wp-config.php")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(upTarget, filepath.Join(rel, "wp-content", "uploads")); err != nil {
		t.Fatal(err)
	}
	return rel
}

func stagingOf(a *Agent, prod state.Site) state.Site {
	stg := prod
	stg.ID, stg.Domain, stg.SystemUser, stg.StagingOf = 4, "staging.docs.example.com", "cg-site-4", prod.ID
	stg.RootPath = filepath.Join(a.Paths.SitesRoot, stg.Domain)
	return stg
}

// A staging clone's wp-config.php resolves inside the staging site, to its
// own shared/ copy, with no relinking: the release's links are relative.
// Production's config is never touched.
func TestStagingCloneConfigResolvesInsideStaging(t *testing.T) {
	a, _ := testAgent(t)
	prod := staticSite(a)
	if _, err := a.sitesHandlers().Create(context.Background(), rpc.CreateSiteParams{Site: prod}); err != nil {
		t.Fatal(err)
	}
	prodRel := wpLayout(t, a, prod, false)
	prodCfg := filepath.Join(prod.RootPath, "shared", "wp-config.php")
	before, _ := os.ReadFile(prodCfg)

	// A file production's user owns outside its tree, hard-linked into it.
	outside := filepath.Join(t.TempDir(), "mine")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(prodRel, "hardlink.txt")); err != nil {
		t.Fatal(err)
	}

	stg := stagingOf(a, prod)
	if _, err := a.stagingHandlers().Create(context.Background(), rpc.StagingParams{Production: prod, Staging: stg}); err != nil {
		t.Fatalf("create staging: %v", err)
	}
	stgCfg := filepath.Join(stg.RootPath, "current", "wp-config.php")
	real, err := filepath.EvalSymlinks(stgCfg)
	if err != nil || real != filepath.Join(stg.RootPath, "shared", "wp-config.php") {
		t.Fatalf("staging wp-config resolves to %q (%v), want staging's shared/", real, err)
	}
	if target, _ := os.Readlink(filepath.Join(stg.RootPath, "releases", "initial", "wp-config.php")); target != wp.ConfigTarget {
		t.Errorf("staging wp-config link = %q, want %q", target, wp.ConfigTarget)
	}
	if real, err := filepath.EvalSymlinks(filepath.Join(stg.RootPath, "current", "wp-content", "uploads")); err != nil || real != filepath.Join(stg.RootPath, "shared", "uploads") {
		t.Errorf("staging uploads resolve to %q (%v)", real, err)
	}
	if target, _ := os.Readlink(filepath.Join(stg.RootPath, "current")); target != "releases/initial" {
		t.Errorf("staging current = %q, want releases/initial", target)
	}
	// Edit staging's config the way wp-cli does, through the link.
	if err := os.WriteFile(stgCfg, []byte("<?php define('DB_NAME', 'staging');\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(prodCfg); string(after) != string(before) {
		t.Fatalf("production's wp-config changed: %q", after)
	}
	// The hard link became an independent copy.
	var st, ost syscall.Stat_t
	if err := syscall.Lstat(filepath.Join(stg.RootPath, "current", "hardlink.txt"), &st); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Lstat(outside, &ost); err != nil {
		t.Fatal(err)
	}
	if st.Ino == ost.Ino || st.Nlink != 1 || ost.Nlink != 2 {
		t.Errorf("the clone linked the outside file (ino %d/%d, nlink %d/%d)", st.Ino, ost.Ino, st.Nlink, ost.Nlink)
	}
}

// A site built with the absolute links of the old layout is migrated: a
// staging clone of it points at staging's shared/, not production's, and
// applying the site's configuration rewrites its own links as relative.
func TestAbsoluteLayoutIsRelinked(t *testing.T) {
	a, _ := testAgent(t)
	prod := staticSite(a)
	if _, err := a.sitesHandlers().Create(context.Background(), rpc.CreateSiteParams{Site: prod}); err != nil {
		t.Fatal(err)
	}
	prodRel := wpLayout(t, a, prod, true)

	stg := stagingOf(a, prod)
	if _, err := a.stagingHandlers().Create(context.Background(), rpc.StagingParams{Production: prod, Staging: stg}); err != nil {
		t.Fatalf("create staging: %v", err)
	}
	real, err := filepath.EvalSymlinks(filepath.Join(stg.RootPath, "current", "wp-config.php"))
	if err != nil || real != filepath.Join(stg.RootPath, "shared", "wp-config.php") {
		t.Fatalf("a clone of an old-layout site resolves wp-config to %q (%v)", real, err)
	}

	prod.Status = state.SiteActive
	if _, err := a.sitesHandlers().Apply(context.Background(), rpc.SiteRef{Site: prod}); err != nil {
		t.Fatal(err)
	}
	for link, want := range map[string]string{
		filepath.Join(prod.RootPath, "current"):         "releases/initial",
		filepath.Join(prodRel, "wp-config.php"):         wp.ConfigTarget,
		filepath.Join(prodRel, "wp-content", "uploads"): wp.UploadsTarget,
	} {
		if got, _ := os.Readlink(link); got != want {
			t.Errorf("%s -> %q, want %q", link, got, want)
		}
	}
}

// The file manager writes a new file rather than into the existing one: a
// hard link a site user made to a file elsewhere is not written through.
func TestFileManagerNeverWritesThroughAHardLink(t *testing.T) {
	a, _ := testAgent(t)
	site := staticSite(a)
	if _, err := a.sitesHandlers().Create(context.Background(), rpc.CreateSiteParams{Site: site}); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "theirs")
	if err := os.WriteFile(outside, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(liveRelease(t, a, site), "linked.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := rpcCall(a, agentrpc.WriteFile, rpc.WriteFileParams{Site: site, RelPath: "current/linked.txt", Content: "edited\n"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(outside); string(b) != "original\n" {
		t.Fatalf("the editor wrote through a hard link: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(site.RootPath, "current", "linked.txt")); string(b) != "edited\n" {
		t.Fatalf("the edit was not saved: %q", b)
	}
}
