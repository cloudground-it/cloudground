package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
	"github.com/cloudground-it/cloudground/internal/velocity"
)

// Tests of the agent as a whole: the domains wired together, behind the
// RPC server's validation and locks.

// A site record the agent cannot trust is refused before anything runs.
func TestCreateSiteRefusesABadRecord(t *testing.T) {
	a, h := testAgent(t)
	for _, s := range []state.Site{
		{Domain: "evil.com; }", SystemUser: "cg-site-1"},
		{Domain: "ok.example.com", SystemUser: "root"},
		{Domain: "ok.example.com", SystemUser: "cg-site-1", PHPVersion: "7.4; rm -rf /"},
		{Domain: "ok.example.com", SystemUser: "cg-site-1", RootPath: "/etc"},
	} {
		if _, err := rpcCall(a, agentrpc.CreateSite, rpc.CreateSiteParams{Site: s}); err == nil {
			t.Errorf("accepted %+v", s)
		}
	}
	if h.touched() {
		t.Fatal("a refused record reached the machine")
	}
}

// A creation holds the site's lock: an apply or a deletion of the same
// domain waits until it is done. It used to run without it.
func TestCreateSiteHoldsTheSiteLock(t *testing.T) {
	a, _ := testAgent(t)
	s := staticSite(a)
	unlock := a.lockSite(s.Domain)
	done := make(chan error, 1)
	go func() {
		_, err := rpcCall(a, agentrpc.CreateSite, rpc.CreateSiteParams{Site: s})
		done <- err
	}()
	select {
	case err := <-done:
		unlock()
		t.Fatalf("CreateSite ran while the site was locked (err %v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := os.Lstat(s.RootPath); err == nil {
		unlock()
		t.Fatal("the tree was made while the site was locked")
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// Many creations, applies and deletions at once, some of them on the same
// sites, end with exactly the live sites configured: one vhost each, and
// nothing of a deleted site.
func TestSiteStorm(t *testing.T) {
	a, h := testAgent(t)
	site := func(i int) state.Site {
		s := staticSite(a)
		s.ID, s.Domain, s.SystemUser = int64(10+i), fmt.Sprintf("s%d.example.com", i), fmt.Sprintf("cg-site-%d", 10+i)
		s.RootPath = filepath.Join(a.Paths.SitesRoot, s.Domain)
		return s
	}
	for i := 0; i < 4; i++ {
		mustCreate(t, a, site(i))
	}
	var wg sync.WaitGroup
	call := func(fn func()) { wg.Add(1); go func() { defer wg.Done(); fn() }() }
	for i := 0; i < 4; i++ {
		old, fresh := site(i), site(4+i)
		call(func() { _, _ = rpcCall(a, agentrpc.DeleteSite, rpc.SiteRef{Site: old}) })
		// An apply racing the deletion either runs first or finds the site gone.
		call(func() { _, _ = rpcCall(a, agentrpc.ApplySiteConfig, rpc.SiteRef{Site: old}) })
		call(func() {
			if _, err := rpcCall(a, agentrpc.CreateSite, rpc.CreateSiteParams{Site: fresh}); err != nil {
				t.Error(err)
			}
		})
		call(func() { _, _ = rpcCall(a, agentrpc.ApplySiteConfig, rpc.SiteRef{Site: fresh}) })
	}
	wg.Wait()
	vhosts, _ := filepath.Glob(filepath.Join(a.Paths.NginxSites, "*.conf"))
	var got []string
	for _, v := range vhosts {
		got = append(got, strings.TrimSuffix(filepath.Base(v), ".conf"))
	}
	want := []string{"s4.example.com", "s5.example.com", "s6.example.com", "s7.example.com"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("vhosts %v, want %v", got, want)
	}
	for i := 0; i < 4; i++ {
		s := site(i)
		if _, err := os.Lstat(s.RootPath); err == nil {
			t.Errorf("%s's tree is back", s.Domain)
		}
		if _, ok := h.accounts.User(s.SystemUser); ok {
			t.Errorf("%s's user is back", s.Domain)
		}
	}
}

// Re-applying a site deleted meanwhile fails instead of putting its vhost
// back.
func TestApplyAfterDeleteDoesNotResurrect(t *testing.T) {
	a, _ := testAgent(t)
	s := staticSite(a)
	if _, err := rpcCall(a, agentrpc.CreateSite, rpc.CreateSiteParams{Site: s}); err != nil {
		t.Fatal(err)
	}
	if _, err := rpcCall(a, agentrpc.DeleteSite, rpc.SiteRef{Site: s}); err != nil {
		t.Fatal(err)
	}
	if _, err := rpcCall(a, agentrpc.ApplySiteConfig, rpc.SiteRef{Site: s}); err == nil {
		t.Fatal("a deleted site was applied")
	}
	if _, err := os.Stat(filepath.Join(a.Paths.NginxSites, s.Domain+".conf")); !os.IsNotExist(err) {
		t.Fatal("the deleted site's vhost is back")
	}
}

// The agent checks the record itself: a compromised API cannot inject into
// rendered configuration or point a site at another directory.
func TestAgentDistrustsTheSiteRecord(t *testing.T) {
	a, _ := testAgent(t)
	s := staticSite(a)
	if _, err := rpcCall(a, agentrpc.CreateSite, rpc.CreateSiteParams{Site: s}); err != nil {
		t.Fatal(err)
	}
	typ, prof := s, s
	typ.Type = state.SiteType("static\nerror_log /etc/x")
	prof.Profile = state.Profile("balanced;")
	for _, bad := range []state.Site{typ, prof} {
		if _, err := rpcCall(a, agentrpc.ApplySiteConfig, rpc.SiteRef{Site: bad}); err == nil {
			t.Fatalf("rendered %q / %q", bad.Type, bad.Profile)
		}
	}
	elsewhere := s
	elsewhere.RootPath = "/etc"
	for name, call := range map[string]func() error{
		"ListFiles": func() error {
			_, err := rpcCall(a, agentrpc.ListFiles, rpc.ListFilesParams{Site: elsewhere})
			return err
		},
		"ReadFile": func() error {
			_, err := rpcCall(a, agentrpc.ReadFile, rpc.ReadFileParams{Site: elsewhere, RelPath: "shadow"})
			return err
		},
		"IssueCertificate": func() error {
			_, err := rpcCall(a, agentrpc.IssueCert, rpc.CertificateParams{Site: elsewhere, Email: "ops@example.com"})
			return err
		},
	} {
		if call() == nil {
			t.Errorf("%s accepted a root outside the sites directory", name)
		}
	}
}

// A staging clone copies production's release; a clone that hits a link
// planted in production refuses to follow it, and its half-made site goes.
func TestStagingCloneAndItsRollback(t *testing.T) {
	a, h := testAgent(t)
	prod := staticSite(a)
	mustCreate(t, a, prod)
	stg := stagingOf(a, prod)
	if _, err := a.stagingHandlers().Create(context.Background(), rpc.StagingParams{Production: prod, Staging: stg}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(stg.RootPath, "releases/initial/index.html")); err != nil {
		t.Fatalf("staging content: %v", err)
	}
	if fi, err := os.Lstat(stg.RootPath); err != nil || fi.Mode().Perm() != 0o750 {
		t.Fatalf("staging root %v %v", fi.Mode(), err)
	}
	for _, c := range h.exec.Calls() {
		if c.Name == "chown" || c.Name == "chmod" || c.Name == "cp" {
			t.Fatalf("a coreutils tool ran on a site tree: %s", c.Line())
		}
	}

	broken := stg
	broken.ID, broken.Domain, broken.SystemUser = 5, "broken.docs.example.com", "cg-site-5"
	broken.RootPath = filepath.Join(a.Paths.SitesRoot, broken.Domain)
	uploads := filepath.Join(prod.RootPath, "shared/uploads")
	if err := os.RemoveAll(uploads); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", uploads); err != nil {
		t.Fatal(err)
	}
	if _, err := a.stagingHandlers().Create(context.Background(), rpc.StagingParams{Production: prod, Staging: broken}); err == nil {
		t.Fatal("cloned through a planted link")
	}
	if _, err := os.Stat(filepath.Join(broken.RootPath, "shared/uploads/passwd")); err == nil {
		t.Fatal("a host directory was copied")
	}
	if _, err := os.Lstat(broken.RootPath); !os.IsNotExist(err) {
		t.Fatalf("the half-made staging site is still there: %v", err)
	}
	if _, ok := h.accounts.User(broken.SystemUser); ok {
		t.Fatal("the half-made staging user is still there")
	}
}

// The agent refuses a protected WooCommerce table at its own boundary,
// before the database is reached.
func TestProtectedStagingTables(t *testing.T) {
	a, h := testAgent(t)
	queried := false
	h.mariadb.OnQuery = func(string, []any) ([]string, error) { queried = true; return nil, nil }
	prod := staticSite(a)
	prod.Type, prod.PHPVersion = state.SiteWooCommerce, "8.4"
	prod.Config.Database = state.DatabaseConfig{Enabled: true, Name: "site_3", User: "site_3"}
	stg := stagingOf(a, prod)
	stg.Config.Database = state.DatabaseConfig{Enabled: true, Name: "site_4", User: "site_4"}
	if _, err := a.stagingHandlers().Push(context.Background(), rpc.SyncStagingDBParams{Production: prod, Staging: stg, Tables: []string{"wp_posts"}}); err == nil {
		t.Fatal("pushed a protected table")
	}
	if queried || len(h.mariadb.Statements()) != 0 {
		t.Fatal("the database was reached")
	}
}

// A purge removes the cached page; drift finds a vhost edited by hand.
func TestPurgeAndDrift(t *testing.T) {
	a, _ := testAgent(t)
	s := staticSite(a)
	s.Type, s.PHPVersion, s.Config.CacheEnabled = state.SiteWordPress, "8.4", true
	mustCreate(t, a, s)
	u, _ := url.Parse("https://docs.example.com/post/")
	entry := velocity.CacheFilePath(filepath.Join(a.Paths.CacheRoot, "docs.example.com"), velocity.CacheKey("https", "GET", u.Host, u.RequestURI()))
	if err := os.MkdirAll(filepath.Dir(entry), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entry, []byte("cached"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := rpcCall(a, agentrpc.PurgeCache, rpc.PurgeParams{Site: s, URLs: []string{u.String()}})
	if err != nil || res.Removed != 1 {
		t.Fatalf("purge removed %d: %v", res.Removed, err)
	}

	vhost := filepath.Join(a.Paths.NginxSites, "docs.example.com.conf")
	orig, _ := os.ReadFile(vhost)
	sum := sha256.Sum256(orig)
	if err := os.WriteFile(vhost, []byte("# by hand\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	drift, err := rpcCall(a, agentrpc.CheckDrift, rpc.DriftParams{Expected: map[string]string{vhost: hex.EncodeToString(sum[:])}})
	if err != nil || len(drift.Drifted) != 1 || drift.Drifted[0].Path != vhost {
		t.Fatalf("drift %+v %v", drift.Drifted, err)
	}
}

// An import takes a .sql file of the site's own, never through a link.
func TestDatabaseImportInput(t *testing.T) {
	a, h := testAgent(t)
	s := staticSite(a)
	mustCreate(t, a, s)
	s.Config.Database = state.DatabaseConfig{Enabled: true, Name: "site_3", User: "site_3"}
	if _, err := rpcCall(a, agentrpc.DBImport, rpc.DBImportParams{Site: s, Database: "site_3", RelPath: "shared/import.txt"}); err == nil || !strings.Contains(err.Error(), ".sql") {
		t.Fatalf("a non-SQL import: %v", err)
	}
	secret := filepath.Join(t.TempDir(), "shadow.sql")
	if err := os.WriteFile(secret, []byte("root:x:..."), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(s.RootPath, "shared/dump.sql")); err != nil {
		t.Fatal(err)
	}
	if _, err := rpcCall(a, agentrpc.DBImport, rpc.DBImportParams{Site: s, Database: "site_3", RelPath: "shared/dump.sql"}); err == nil {
		t.Fatal("imported through a link")
	}
	if len(h.mariadb.Imported) != 0 {
		t.Fatal("the outside file reached the database")
	}
	if _, err := rpcCall(a, agentrpc.DBExport, rpc.DBExportParams{Site: s, Database: "mysql"}); err == nil || !strings.Contains(err.Error(), "invalid database name") {
		t.Fatalf("exported another schema: %v", err)
	}
}

// A certificate the operator brings is installed only if it pairs with its
// key and covers the site; nginx then serves it; deleting the site removes
// it and no other.
func TestCustomCertificateLifecycle(t *testing.T) {
	a, _ := testAgent(t)
	s := staticSite(a)
	mustCreate(t, a, s)
	other, otherKey := selfSigned(t, "elsewhere.example.com")
	good, goodKey := selfSigned(t, s.Domain)
	for _, bad := range []rpc.CustomCertParams{{Site: s, Fullchain: other, Key: otherKey}, {Site: s, Fullchain: good, Key: otherKey}} {
		if _, err := rpcCall(a, agentrpc.CustomCert, bad); err == nil {
			t.Fatal("installed a certificate that does not fit")
		}
	}
	if _, err := rpcCall(a, agentrpc.CustomCert, rpc.CustomCertParams{Site: s, Fullchain: good, Key: goodKey}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(a.Paths.CustomCertDir, s.Domain)
	vhost, _ := os.ReadFile(filepath.Join(a.Paths.NginxSites, s.Domain+".conf"))
	if !strings.Contains(string(vhost), filepath.Join(dir, "fullchain.pem")) {
		t.Fatal("nginx does not serve it")
	}
	if fi, _ := os.Stat(filepath.Join(dir, "privkey.pem")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v", fi.Mode().Perm())
	}
	another := filepath.Join(a.Paths.CustomCertDir, "other.example.com")
	if err := os.MkdirAll(another, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(another, "privkey.pem"), []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := rpcCall(a, agentrpc.DeleteSite, rpc.SiteRef{Site: s}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatal("the certificate outlived its site")
	}
	if _, err := os.Stat(filepath.Join(another, "privkey.pem")); err != nil {
		t.Fatal("another site's certificate went too")
	}
}

// A site's PHP master and the wp-cli runs of its WordPress go in its slice;
// deleting the site stops the slice.
func TestSiteProcessesShareTheSlice(t *testing.T) {
	a, h := testAgent(t)
	s := staticSite(a)
	s.Type, s.PHPVersion = state.SitePHP, "8.4"
	mustCreate(t, a, s)
	if got := h.systemd.SliceOf("cloudground-php-cg-site-3.service"); got != "cg-site-3.slice" {
		t.Fatalf("master slice %q", got)
	}
	if _, err := a.wpHandlers().Run(context.Background(), s, "core", "version"); err != nil {
		t.Fatal(err)
	}
	last := h.exec.Calls()[len(h.exec.Calls())-1]
	// A transient service in the slice, with the PHP master's sandbox.
	if last.Name != "systemd-run" || !strings.Contains(last.Line(), " --slice=cg-site-3.slice ") || !strings.Contains(last.Line(), " --uid=cg-site-3 ") ||
		!strings.Contains(last.Line(), " -p NoNewPrivileges=yes ") || !strings.Contains(last.Line(), " -- wp --path=") {
		t.Fatalf("wp-cli ran as %q", last.Line())
	}
	if _, err := rpcCall(a, agentrpc.DeleteSite, rpc.SiteRef{Site: s}); err != nil {
		t.Fatal(err)
	}
	if !h.systemd.Did("stop", "cg-site-3.slice") {
		t.Fatal("the slice was not stopped")
	}
	if u, ok := h.systemd.Unit("cloudground-php-cg-site-3.service"); ok {
		t.Fatalf("the master is still loaded: %+v", u)
	}
}

// Every production path is set: an empty one joins to a relative path.
func TestDefaultPathsAreAllSet(t *testing.T) {
	v := reflect.ValueOf(DefaultPaths())
	for i := 0; i < v.NumField(); i++ {
		if s, ok := v.Field(i).Interface().(string); ok && !filepath.IsAbs(s) {
			t.Errorf("DefaultPaths().%s = %q", v.Type().Field(i).Name, s)
		}
	}
	if !strings.HasPrefix(DefaultPaths().WorkDir, "/var/lib/cloudground-agent/") {
		t.Fatal("the default work directory is inside the API user's")
	}
}

// The work directory must not be reachable through a link.
func TestEnsureWorkDirRefusesLinks(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "real"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	a := &Agent{Paths: Paths{WorkDir: filepath.Join(root, "link", "work")}}
	if err := a.EnsureWorkDir(); err == nil {
		t.Fatal("accepted a work directory behind a link")
	}
	a.Paths.WorkDir = filepath.Join(root, "real", "work")
	if err := a.EnsureWorkDir(); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(a.Paths.WorkDir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
}

// A staging diff lists what changed; a selective push takes only the
// chosen paths from staging, into a release that is otherwise production's.
func TestStagingDiffAndSelectivePush(t *testing.T) {
	a, _ := testAgent(t)
	tree := func(id int64, domain string, files map[string]string) state.Site {
		s := state.Site{ID: id, Domain: domain, Type: state.SiteStatic, SystemUser: fmt.Sprintf("cg-site-%d", id),
			RootPath: filepath.Join(a.Paths.SitesRoot, domain)}
		rel := filepath.Join(s.RootPath, "releases", "20260101-000000")
		for p, c := range files {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(rel, p)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(rel, p), []byte(c), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink("releases/20260101-000000", filepath.Join(s.RootPath, "current")); err != nil {
			t.Fatal(err)
		}
		return s
	}
	prod := tree(3, "shop.example.com", map[string]string{"index.php": "<?php // same",
		"wp-content/themes/t/style.css": "old", "wp-content/plugins/p/p.php": "<?php // prod", "gone.txt": "x"})
	stg := tree(4, "staging.shop.example.com", map[string]string{"index.php": "<?php // same",
		"wp-content/themes/t/style.css": "new!", "wp-content/plugins/p/p.php": "<?php // staging", "added.txt": "y"})
	stg.StagingOf = prod.ID

	d, err := a.stagingHandlers().Diff(context.Background(), rpc.StagingParams{Production: prod, Staging: stg})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(d.Added) != "[added.txt]" || fmt.Sprint(d.Removed) != "[gone.txt]" ||
		fmt.Sprint(d.Modified) != "[wp-content/plugins/p/p.php wp-content/themes/t/style.css]" {
		t.Fatalf("diff %+v", d)
	}
	deploy := func(id, path string) error {
		_, err := a.releaseHandlers().Deploy(context.Background(), rpc.DeployParams{Site: prod,
			SourceDir: filepath.Join(prod.RootPath, "current"), ReleaseID: id, SourceSite: &stg, Overlay: []string{path}})
		return err
	}
	if err := deploy("20260102-000000", "wp-content/themes/t"); err != nil {
		t.Fatal(err)
	}
	read := func(p string) string {
		b, _ := os.ReadFile(filepath.Join(prod.RootPath, "releases/20260102-000000", p))
		return string(b)
	}
	if read("wp-content/themes/t/style.css") != "new!" {
		t.Fatal("the chosen path was not taken from staging")
	}
	if read("wp-content/plugins/p/p.php") != "<?php // prod" || read("gone.txt") != "x" {
		t.Fatal("a path that was not chosen changed")
	}
	for _, bad := range []string{"../../etc", "wp-config.php", "wp-content/uploads/x"} {
		if deploy("20260103-000000", bad) == nil {
			t.Fatalf("pushed %q", bad)
		}
	}
}
