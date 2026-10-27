package release

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/connector"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

var ctx = context.Background()

// siteUID is the site user's uid in these tests when they run as root (the
// container suite): files must end up the site user's, not root's.
const siteUID = 4242

// fixture is a provisioned site: releases/initial live, shared/ with the
// configuration, the object-cache drop-in and uploads.
type fixture struct {
	o       *Ops
	site    state.Site
	reloads int
	// nginxReloads counts nginx reloads (open_file_cache, SPEC §4).
	nginxReloads int
	owner        sitefs.Owner
}

func newSite(t *testing.T, typ state.SiteType) *fixture {
	t.Helper()
	sites := t.TempDir()
	f := &fixture{owner: sitefs.Owner{UID: os.Getuid(), GID: os.Getgid()}}
	if os.Getuid() == 0 {
		f.owner = sitefs.Owner{UID: siteUID, GID: siteUID}
	}
	f.site = state.Site{ID: 3, Domain: "shop.example.com", Type: typ, SystemUser: "cg-site-3", PHPVersion: "8.3",
		RootPath: filepath.Join(sites, "shop.example.com")}
	f.o = &Ops{
		Open: func(s state.Site) (*sitefs.Root, error) { return sitefs.Open(s.RootPath, f.owner) },
		ReloadPHP: func(_ context.Context, s state.Site) error {
			f.reloads++
			return nil
		},
		ReloadNginx: func(context.Context) error {
			f.nginxReloads++
			return nil
		},
	}
	mkTree(t, f.site.RootPath, map[string]string{
		"releases/initial/index.php":         "<?php // initial",
		"shared/wp-config.php":               "<?php // config",
		"shared/object-cache.php":            connector.ObjectCacheAPCu,
		"shared/uploads/2026/10/a.jpg":       "jpg",
		"deploy/build/index.php":             "<?php // v2",
		"deploy/build/wp-content/themes/t/a": "theme",
	})
	if err := os.Symlink("releases/initial", filepath.Join(f.site.RootPath, CurrentLink)); err != nil {
		t.Fatal(err)
	}
	return f
}

func mkTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *fixture) path(p string) string { return filepath.Join(f.site.RootPath, p) }

func (f *fixture) deploy(id string) (rpc.DeployResult, error) {
	return f.o.Deploy(ctx, rpc.DeployParams{Site: f.site, SourceDir: f.path("deploy/build"), ReleaseID: id})
}

func (f *fixture) mustDeploy(t *testing.T, id string) rpc.DeployResult {
	t.Helper()
	res, err := f.deploy(id)
	if err != nil {
		t.Fatalf("deploy %s: %v", id, err)
	}
	return res
}

func (f *fixture) current(t *testing.T) string {
	t.Helper()
	l, err := os.Readlink(f.path(CurrentLink))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func wantCode(t *testing.T, what string, err error, code agentrpc.Code) {
	t.Helper()
	if got := agentrpc.CodeOf(err); err == nil || got != code {
		t.Fatalf("%s: err = %v (%s), want %s", what, err, got, code)
	}
}

func ids(n int) []string {
	var out []string
	t0 := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	for i := range n {
		out = append(out, site.NewReleaseID(t0.Add(time.Duration(i)*time.Minute)))
	}
	return out
}

func TestDeployPromoteRollback(t *testing.T) {
	f := newSite(t, state.SiteStatic)
	id := ids(1)[0]
	res := f.mustDeploy(t, id)
	if res.ReleaseID != id || res.Path != f.path("releases/"+id) || len(res.Checksum) != 64 {
		t.Fatalf("result %+v", res)
	}
	if b, _ := os.ReadFile(f.path("releases/" + id + "/index.php")); string(b) != "<?php // v2" {
		t.Fatalf("release content %q", b)
	}
	if f.current(t) != "releases/initial" {
		t.Fatal("a deploy changed the live release")
	}
	_, err := f.deploy(id)
	wantCode(t, "a second deploy under the same id", err, agentrpc.CodeConflict)

	if _, err := f.o.Promote(ctx, rpc.ReleaseParams{Site: f.site, ReleaseID: id}); err != nil {
		t.Fatal(err)
	}
	if f.current(t) != "releases/"+id || f.reloads != 1 || f.nginxReloads != 1 {
		t.Fatalf("current -> %s after %d PHP and %d nginx reloads", f.current(t), f.reloads, f.nginxReloads)
	}
	out, err := f.o.Rollback(ctx, rpc.ReleaseParams{Site: f.site})
	if err != nil || out["current"] != site.InitialRelease || f.current(t) != "releases/initial" {
		t.Fatalf("rollback: %v %v, current -> %s", out, err, f.current(t))
	}
	_, err = f.o.Rollback(ctx, rpc.ReleaseParams{Site: f.site})
	wantCode(t, "a rollback with nothing older", err, agentrpc.CodeConflict)
	// An explicit target goes forward too.
	if _, err := f.o.Rollback(ctx, rpc.ReleaseParams{Site: f.site, ReleaseID: id}); err != nil || f.current(t) != "releases/"+id {
		t.Fatalf("rollback to %s: %v", id, err)
	}
}

// Rollback goes to the release that was live before, never to a newer one.
// It used to take the newest release that was not current: a built but
// never promoted release went live untested, and a second rollback undid
// the first instead of going further back.
func TestRollbackGoesBackInTime(t *testing.T) {
	f := newSite(t, state.SiteStatic)
	r := ids(3)
	for _, id := range r[:2] {
		f.mustDeploy(t, id)
		if _, err := f.o.Promote(ctx, rpc.ReleaseParams{Site: f.site, ReleaseID: id}); err != nil {
			t.Fatal(err)
		}
	}
	f.mustDeploy(t, r[2]) // built, not promoted
	for _, want := range []string{r[0], site.InitialRelease} {
		if _, err := f.o.Rollback(ctx, rpc.ReleaseParams{Site: f.site}); err != nil {
			t.Fatal(err)
		}
		if got := f.current(t); got != "releases/"+want {
			t.Fatalf("rollback went to %s, want releases/%s", got, want)
		}
	}
}

// Two IDs of the old second-resolution form and the new one order by time.
func TestRollbackOrdersLegacyIDs(t *testing.T) {
	f := newSite(t, state.SiteStatic)
	newer := site.NewReleaseID(time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC))
	for _, id := range []string{"20261009-115959", newer} {
		f.mustDeploy(t, id)
	}
	if _, err := f.o.Promote(ctx, rpc.ReleaseParams{Site: f.site, ReleaseID: newer}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.o.Rollback(ctx, rpc.ReleaseParams{Site: f.site}); err != nil || f.current(t) != "releases/20261009-115959" {
		t.Fatalf("rollback: %v, current -> %s", err, f.current(t))
	}
}

// R-4.3: a site deployed before ULIDs keeps its releases, and rollback walks
// back through both forms by time (a ULID starts with 0–7 and sorted
// before every older name by text).
func TestRollbackAcrossULIDsAndOlderNames(t *testing.T) {
	f := newSite(t, state.SiteStatic)
	noon := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	first, last := ulidAt(noon), ulidAt(noon.Add(time.Second))
	order := []string{"20261009-115959", first, "20261009-120000.500-abcdef", last}
	for _, id := range order {
		f.mustDeploy(t, id)
	}
	if _, err := f.o.Promote(ctx, rpc.ReleaseParams{Site: f.site, ReleaseID: last}); err != nil {
		t.Fatal(err)
	}
	for i := len(order) - 2; i >= 0; i-- {
		if _, err := f.o.Rollback(ctx, rpc.ReleaseParams{Site: f.site}); err != nil {
			t.Fatal(err)
		}
		if got := f.current(t); got != "releases/"+order[i] {
			t.Fatalf("rollback went to %s, want releases/%s", got, order[i])
		}
	}
}

// R-4.3: CurrentRelease reads what current names; RemoveReleases removes
// named releases but never the live one, and ignores ones not there.
func TestCurrentReleaseAndRemoveReleases(t *testing.T) {
	f := newSite(t, state.SiteStatic)
	cur, err := f.o.CurrentRelease(ctx, rpc.CurrentReleaseParams{Site: f.site})
	if err != nil || cur.ReleaseID != site.InitialRelease {
		t.Fatalf("current = %+v %v, want initial", cur, err)
	}
	r := ids(2)
	for _, id := range r {
		f.mustDeploy(t, id)
	}
	if _, err := f.o.Promote(ctx, rpc.ReleaseParams{Site: f.site, ReleaseID: r[0]}); err != nil {
		t.Fatal(err)
	}
	if cur, _ := f.o.CurrentRelease(ctx, rpc.CurrentReleaseParams{Site: f.site}); cur.ReleaseID != r[0] {
		t.Fatalf("current = %q, want %s", cur.ReleaseID, r[0])
	}
	res, err := f.o.RemoveReleases(ctx, rpc.RemoveReleasesParams{Site: f.site, ReleaseIDs: []string{r[0], r[1], "20200101-000000"}})
	if err != nil || len(res.Removed) != 1 || res.Removed[0] != r[1] {
		t.Fatalf("removed = %+v %v, want only %s", res, err, r[1])
	}
	if _, err := os.Lstat(f.path("releases/" + r[0])); err != nil {
		t.Fatal("the live release was removed")
	}
	if _, err := os.Lstat(f.path("releases/" + r[1])); !os.IsNotExist(err) {
		t.Fatal("the named release is still there")
	}
	if _, err := f.o.RemoveReleases(ctx, rpc.RemoveReleasesParams{Site: f.site, ReleaseIDs: []string{"../x"}}); err == nil {
		t.Fatal("an invalid id was accepted")
	}
	if err := os.Remove(f.path("current")); err != nil {
		t.Fatal(err)
	}
	if cur, err := f.o.CurrentRelease(ctx, rpc.CurrentReleaseParams{Site: f.site}); err != nil || cur.ReleaseID != "" {
		t.Fatalf("no current link = %+v %v", cur, err)
	}
}

// ulidAt is a ULID of time t (NewReleaseID never goes back in time, so it
// cannot make one for the past).
func ulidAt(t time.Time) string {
	const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	ms := uint64(t.UnixMilli())
	var b [10]byte
	for i := 9; i >= 0; i-- {
		b[i] = crockford[ms&31]
		ms >>= 5
	}
	return string(b[:]) + "7Q0000000000000A"
}

func TestPromoteRefuses(t *testing.T) {
	f := newSite(t, state.SiteStatic)
	// A release must be a real directory: promoting a symlink would point
	// the document root anywhere.
	if err := os.Symlink(t.TempDir(), f.path("releases/20260711-130000")); err != nil {
		t.Fatal(err)
	}
	_, err := f.o.Promote(ctx, rpc.ReleaseParams{Site: f.site, ReleaseID: "20260711-130000"})
	wantCode(t, "a symlinked release", err, agentrpc.CodeNotFound)
	_, err = f.o.Promote(ctx, rpc.ReleaseParams{Site: f.site, ReleaseID: "20260711-130001"})
	wantCode(t, "a missing release", err, agentrpc.CodeNotFound)
	if err := os.Mkdir(f.path("releases/.build-x"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "../shared", ".build-x", "20260711-130000/..", "initial/"} {
		_, err := f.o.Promote(ctx, rpc.ReleaseParams{Site: f.site, ReleaseID: id})
		wantCode(t, fmt.Sprintf("release id %q", id), err, agentrpc.CodeInvalid)
	}
	if f.current(t) != "releases/initial" {
		t.Fatal("a refused promote moved current")
	}
}

// The release appears whole or not at all, and is the site user's, with
// nothing writable by the group or others; releases/ stays root's.
func TestDeployPublishesAFinishedTree(t *testing.T) {
	f := newSite(t, state.SiteWordPress)
	f.site.Config.ObjectCache = true
	mkTree(t, f.path("deploy/build"), map[string]string{"wp-content/plugins/p/open.php": "<?php"})
	os.Chmod(f.path("deploy/build/wp-content/plugins/p/open.php"), 0o666)
	os.Chmod(f.path("deploy/build/wp-content/plugins/p"), 0o777)
	id := ids(1)[0]

	// A failure after the copy (a selective push of a path staging lacks)
	// leaves neither the release nor its build directory.
	stg := f.site
	stg.ID, stg.Domain, stg.SystemUser, stg.StagingOf = 4, "staging.shop.example.com", "cg-site-4", f.site.ID
	stg.RootPath = filepath.Join(filepath.Dir(f.site.RootPath), stg.Domain)
	mkTree(t, stg.RootPath, map[string]string{"releases/initial/index.php": "<?php // staging"})
	os.Symlink("releases/initial", filepath.Join(stg.RootPath, CurrentLink))
	_, err := f.o.Deploy(ctx, rpc.DeployParams{Site: f.site, SourceDir: f.path("deploy/build"), ReleaseID: id,
		SourceSite: &stg, Overlay: []string{"wp-content/nothing"}})
	wantCode(t, "an overlay of a missing path", err, agentrpc.CodeInvalid)
	ents, _ := os.ReadDir(f.path("releases"))
	if len(ents) != 1 {
		t.Fatalf("a failed deploy left %v in releases/", ents)
	}

	// A build directory left by a crash is not a release, and the next
	// deploy clears it.
	if err := os.MkdirAll(f.path("releases/.build-"+id+"/"+id), 0o700); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := List(mustOpen(t, f)); len(got) != 1 {
		t.Fatalf("List = %v, want only initial", got)
	}
	f.mustDeploy(t, id)
	if _, err := os.Stat(f.path("releases/.build-" + id)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("the interrupted build directory is still there")
	}

	rel := f.path("releases/" + id)
	err = filepath.WalkDir(rel, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		st := fi.Sys().(*syscall.Stat_t)
		if int(st.Uid) != f.owner.UID || int(st.Gid) != f.owner.GID {
			t.Errorf("%s belongs to %d:%d, want the site user", p, st.Uid, st.Gid)
		}
		if fi.Mode()&fs.ModeSymlink == 0 && fi.Mode().Perm()&0o022 != 0 {
			t.Errorf("%s is %v: writable by group or others", p, fi.Mode())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(rel); fi.Mode().Perm() != 0o750 {
		t.Errorf("release mode %v, want 0750", fi.Mode().Perm())
	}
	if os.Getuid() == 0 {
		fi, _ := os.Stat(f.path("releases"))
		if st := fi.Sys().(*syscall.Stat_t); st.Uid != 0 || int(st.Gid) != f.owner.GID || fi.Mode().Perm() != 0o750 {
			t.Errorf("releases/ is %d:%d %v, want root:<site group> 0750", st.Uid, st.Gid, fi.Mode().Perm())
		}
	}
	for link, target := range map[string]string{
		"wp-config.php": configTarget, "wp-content/uploads": uploadsTarget, "wp-content/object-cache.php": "../../../shared/object-cache.php",
	} {
		if got, err := os.Readlink(filepath.Join(rel, link)); got != target {
			t.Errorf("%s -> %q (%v), want %q", link, got, err, target)
		}
	}
}

func mustOpen(t *testing.T, f *fixture) *sitefs.Root {
	t.Helper()
	r, err := f.o.Open(f.site)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// A deploy must leave the release agreeing with the site's object-cache
// setting: the drop-in is a link into shared/ that a fresh tree lacks, and a
// site once ran for two weeks without one while the panel said it was on.
// And it installs the connector, which a CI build artefact never has.
func TestDeployKeepsConnectorAndDropin(t *testing.T) {
	for _, tc := range []struct {
		name       string
		typ        state.SiteType
		cache      bool
		seed       bool // a stale drop-in in the source tree
		dropin, mu bool
	}{
		{"wordpress, cache on", state.SiteWordPress, true, false, true, true},
		{"woocommerce, cache on, source had one", state.SiteWooCommerce, true, true, true, true},
		{"wordpress, cache off, source had one", state.SiteWordPress, false, true, false, true},
		{"static", state.SiteStatic, true, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSite(t, tc.typ)
			f.site.Config.ObjectCache = tc.cache
			if tc.seed {
				mkTree(t, f.path("deploy/build"), map[string]string{"wp-content/object-cache.php": "<?php // stale"})
			}
			id := ids(1)[0]
			res := f.mustDeploy(t, id)
			rel := f.path("releases/" + id)
			_, err := os.Lstat(filepath.Join(rel, "wp-content/object-cache.php"))
			if (err == nil) != tc.dropin {
				t.Errorf("drop-in present = %v, want %v", err == nil, tc.dropin)
			}
			b, err := os.ReadFile(filepath.Join(rel, "wp-content/mu-plugins/cloudground-connector.php"))
			if (err == nil) != tc.mu || (tc.mu && string(b) != connector.PHP) {
				t.Errorf("connector present = %v, want %v", err == nil, tc.mu)
			}
			if tc.mu != (len(res.Managed) == 1) {
				t.Errorf("managed files %v", res.Managed)
			}
		})
	}
}

// A deploy copies as root, so its source is confined to the site's own tree
// or its own staging copy's: otherwise a site manager could read any
// directory on the host back out of the release.
func TestDeployRefusesSourcesOutsideTheSite(t *testing.T) {
	f := newSite(t, state.SiteStatic)
	secret := filepath.Join(t.TempDir(), "panel-secrets")
	mkTree(t, secret, map[string]string{"agent.token": "s3cret"})
	if err := os.Symlink(secret, f.path("shared/build")); err != nil {
		t.Fatal(err)
	}
	id := ids(1)[0]
	for name, src := range map[string]string{
		"absolute path elsewhere on the host":  secret,
		"symlink inside the site pointing out": f.path("shared/build"),
		"dot-dot escape from the site":         f.path("../../" + filepath.Base(filepath.Dir(secret)) + "/panel-secrets"),
		"relative path":                        "deploy/build",
		"the site root":                        f.site.RootPath,
		"releases/":                            f.path("releases"),
		"a build directory":                    f.path("releases/.build-" + id),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := f.o.Deploy(ctx, rpc.DeployParams{Site: f.site, SourceDir: src, ReleaseID: id})
			wantCode(t, "deploy", err, agentrpc.CodeInvalid)
			if _, err := os.Lstat(f.path("releases/" + id)); err == nil {
				t.Fatal("a release was made")
			}
		})
	}

	// Another site's tree only when it is this site's staging copy.
	other := f.site
	other.ID, other.Domain, other.SystemUser = 9, "other.example.com", "cg-site-9"
	other.RootPath = filepath.Join(filepath.Dir(f.site.RootPath), other.Domain)
	mkTree(t, other.RootPath, map[string]string{"build/index.html": "staging"})
	p := rpc.DeployParams{Site: f.site, SourceDir: filepath.Join(other.RootPath, "build"), ReleaseID: id, SourceSite: &other}
	_, err := f.o.Deploy(ctx, p)
	wantCode(t, "another site's tree", err, agentrpc.CodeInvalid)
	other.StagingOf = f.site.ID
	if _, err := f.o.Deploy(ctx, p); err != nil {
		t.Fatalf("deploy from the site's own staging copy: %v", err)
	}
	if b, _ := os.ReadFile(f.path("releases/" + id + "/index.html")); string(b) != "staging" {
		t.Fatalf("release content %q", b)
	}
}

// A selective push takes the chosen paths from staging's live release into
// a release that is otherwise production's, and refuses paths that would
// carry staging's configuration or uploads, or leave the release.
func TestSelectivePush(t *testing.T) {
	f := newSite(t, state.SiteStatic)
	mkTree(t, f.path("releases/initial"), map[string]string{"wp-content/themes/t/style.css": "old", "wp-content/plugins/p/p.php": "prod"})
	stg := f.site
	stg.ID, stg.Domain, stg.SystemUser, stg.StagingOf = 4, "staging.shop.example.com", "cg-site-4", f.site.ID
	stg.RootPath = filepath.Join(filepath.Dir(f.site.RootPath), stg.Domain)
	mkTree(t, stg.RootPath, map[string]string{
		"releases/initial/wp-content/themes/t/style.css": "new!", "releases/initial/wp-content/plugins/p/p.php": "staging",
	})
	os.Symlink("releases/initial", filepath.Join(stg.RootPath, CurrentLink))
	id := ids(2)
	push := func(id string, paths ...string) error {
		_, err := f.o.Deploy(ctx, rpc.DeployParams{Site: f.site, SourceDir: f.path(CurrentLink), ReleaseID: id, SourceSite: &stg, Overlay: paths})
		return err
	}
	if err := push(id[0], "wp-content/themes/t"); err != nil {
		t.Fatal(err)
	}
	read := func(p string) string { b, _ := os.ReadFile(f.path("releases/" + id[0] + "/" + p)); return string(b) }
	if read("wp-content/themes/t/style.css") != "new!" || read("wp-content/plugins/p/p.php") != "prod" || read("index.php") != "<?php // initial" {
		t.Fatal("the release is not production's with only the chosen path from staging")
	}
	for _, bad := range []string{"../../etc", "wp-config.php", "wp-content/uploads/x", "a//b", ""} {
		wantCode(t, fmt.Sprintf("overlay %q", bad), push(id[1], bad), agentrpc.CodeInvalid)
	}
	_, err := f.o.Deploy(ctx, rpc.DeployParams{Site: f.site, SourceDir: f.path(CurrentLink), ReleaseID: id[1], Overlay: []string{"x"}})
	wantCode(t, "an overlay without the staging site", err, agentrpc.CodeInvalid)
}

// A selective push never writes or deletes through a symlink that arrived
// in the release from production's tree.
func TestOverlayRefusesSymlinkedParents(t *testing.T) {
	f := newSite(t, state.SiteStatic)
	stg := f.site
	stg.ID, stg.Domain, stg.SystemUser, stg.StagingOf = 4, "staging.shop.example.com", "cg-site-4", f.site.ID
	stg.RootPath = filepath.Join(filepath.Dir(f.site.RootPath), stg.Domain)
	mkTree(t, stg.RootPath, map[string]string{"releases/initial/wp-content/plugins/p/x.php": "new"})
	os.Symlink("releases/initial", filepath.Join(stg.RootPath, CurrentLink))

	outside := t.TempDir()
	mkTree(t, outside, map[string]string{"plugins/p/keep": "k"})
	if err := os.MkdirAll(f.path("releases/.build-r/r"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.Symlink(outside, f.path("releases/.build-r/r/wp-content"))
	r := mustOpen(t, f)
	err := f.o.overlay(r, "releases/.build-r/r", stg, []string{"wp-content/plugins/p"})
	if err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("the overlay went through a symlinked parent: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "plugins/p/keep")); err != nil {
		t.Fatal("a file outside the release was deleted")
	}
}

// Promote keeps the newest Keep releases older than the live one, and
// everything at or after it.
func TestPromotePrunes(t *testing.T) {
	f := newSite(t, state.SiteStatic)
	f.o.Keep = 2
	r := ids(5)
	for _, id := range r {
		f.mustDeploy(t, id)
	}
	if err := os.MkdirAll(f.path("releases/.build-stale"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(f.path("releases/not-a-release"), 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := f.o.Promote(ctx, rpc.ReleaseParams{Site: f.site, ReleaseID: r[3]}); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(f.path("releases"))
	var got []string
	for _, e := range ents {
		got = append(got, e.Name())
	}
	want := []string{r[1], r[2], r[3], r[4], "not-a-release"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("releases after promote: %v, want %v", got, want)
	}
}
