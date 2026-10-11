package sites

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"

	"github.com/cloudground-it/cloudground/internal/phpfpm"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// The configuration a site renders to (vhost, cache zone, global files,
// PHP pool and unit, staging password file) is pinned byte for byte in
// testdata/golden, rendered by the agent before R-3.7 (commit 7021ce9).
// The rewrite must not change a single byte of it: the templates are
// rewritten separately (Phase 5), with their own benchmarks. The slice
// drop-in is the one file the rewrite adds; it is checked on its own.

var updateGolden = flag.Bool("update-golden", false, "rewrite the golden site renders")

const goldenDir = "testdata/golden"

// goldenSites is one site of every kind the renderer distinguishes.
func goldenSites(sitesRoot string) []state.Site {
	mk := func(id int64, domain string, typ state.SiteType, php string) state.Site {
		return state.Site{
			ID: id, Domain: domain, Type: typ, Profile: state.ProfileBalanced, Engine: state.EngineNginx,
			PHPVersion: php, SystemUser: fmt.Sprintf("cg-site-%d", id),
			RootPath: filepath.Join(sitesRoot, domain), Status: state.SiteActive,
		}
	}
	static := mk(1, "static.example.com", state.SiteStatic, "")
	php := mk(2, "php.example.com", state.SitePHP, "8.4")
	php.Config.Resources.MemoryHighMB = 512
	php.Config.PHP = state.PHPSettings{MemoryLimitMB: 384, UploadMaxMB: 64, MaxExecutionSeconds: 300}
	laravel := mk(3, "laravel.example.com", state.SiteLaravel, "8.3")
	laravel.Config.PublicRoot = "public"
	laravel.Config.Resources = state.Resources{MemoryHighMB: 256, PHPWorkers: 3}
	wp := mk(4, "blog.example.com", state.SiteWordPress, "8.4")
	wp.Config.CacheEnabled = true
	wp.Config.HSTS = true
	wp.Config.Resources.MemoryHighMB = 1024
	woo := mk(5, "shop.example.com", state.SiteWooCommerce, "8.5")
	woo.Profile = state.ProfileCommerce
	woo.Aliases = []string{"www.shop.example.com"}
	woo.Config.CacheEnabled = true
	woo.Config.CacheTTLSec = 600
	woo.Config.Resources.MemoryHighMB = 2048
	proxy := mk(6, "app.example.com", state.SiteProxy, "")
	proxy.Config.ProxyUpstream = "http://127.0.0.1:3000"
	stg := mk(7, "staging.blog.example.com", state.SiteWordPress, "8.4")
	stg.StagingOf = wp.ID
	stg.Config.CacheEnabled = true
	stg.Config.StagingAuthHash = "$2a$10$" + strings.Repeat("b", 53)
	stg.Config.Resources.MemoryHighMB = 512
	max := mk(8, "fast.example.com", state.SitePHP, "8.4")
	max.Profile = state.ProfileMaximum
	max.Config.CacheEnabled = true
	max.Config.Resources.MemoryHighMB = 768
	return []state.Site{static, php, laravel, wp, woo, proxy, stg, max}
}

// goldenBuilds are the nginx builds the renderer reads from `nginx -V`.
var goldenBuilds = map[string]string{
	"nginx124": "nginx version: nginx/1.24.0 (Ubuntu)\nconfigure arguments: --with-http_ssl_module --with-http_v2_module",
	"nginx128": "nginx version: nginx/1.28.0 (Ubuntu)\nconfigure arguments: --with-http_ssl_module --with-http_v2_module --with-http_v3_module",
}

// goldenText is what a render produced: the managed files it reported and
// the content and mode of each file on disk, with root replaced by /ROOT.
func goldenText(t *testing.T, root string, files []rpc.ManagedFile) string {
	t.Helper()
	var b strings.Builder
	paths := make([]string, 0, len(files))
	for _, f := range files {
		// A file's hash covers the paths in it, which differ per run; the
		// content itself is compared below, and the hash checked against it.
		if b, err := os.ReadFile(f.Path); err == nil {
			if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != f.SHA256 {
				t.Errorf("%s: reported hash does not match the file", f.Path)
			}
			paths = append(paths, f.Path)
			continue
		}
		paths = append(paths, f.Path+" "+f.SHA256)
	}
	sort.Strings(paths)
	b.WriteString("## managed\n")
	for _, p := range paths {
		b.WriteString(strings.ReplaceAll(p, root, "/ROOT") + "\n")
	}
	var onDisk []string
	for _, f := range files {
		if fi, err := os.Lstat(f.Path); err == nil && fi.Mode().IsRegular() {
			onDisk = append(onDisk, f.Path)
		}
	}
	sort.Strings(onDisk)
	for _, p := range onDisk {
		fi, _ := os.Lstat(p)
		content, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "## %s %v\n%s", strings.ReplaceAll(p, root, "/ROOT"), fi.Mode().Perm(), strings.ReplaceAll(string(content), root, "/ROOT"))
	}
	return b.String()
}

// checkDropin checks the slice drop-in of a PHP site's master.
func checkDropin(t *testing.T, path string, s state.Site) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "[Service]\nSlice=" + s.SystemUser + ".slice\n"; !strings.HasSuffix(string(b), want) {
		t.Errorf("slice drop-in:\n%s", b)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o644 {
		t.Errorf("drop-in mode %v", fi.Mode().Perm())
	}
}

// checkGolden compares got with the golden file name, or rewrites it.
func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(goldenDir, name+".golden")
	if *updateGolden {
		if err := os.MkdirAll(goldenDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no golden render (run with -update-golden on the reference build): %v", err)
	}
	if string(want) != got {
		t.Errorf("%s: the render changed.\n--- want\n%s\n--- got\n%s", name, want, got)
	}
}

func TestRenderIsByteIdentical(t *testing.T) {
	// The modes the goldens record are the ones a server gets: rendered
	// under the agent's UMask=0027, not the test's own umask.
	old := syscall.Umask(0o027)
	defer syscall.Umask(old)
	defer func(n int) { phpfpm.CPUCount = n }(phpfpm.CPUCount)
	phpfpm.CPUCount = 4
	for build, info := range goldenBuilds {
		for _, site := range goldenSites("/srv/sites") {
			t.Run(build+"/"+site.Domain, func(t *testing.T) {
				e := newEnv(t)
				e.nginx.Build = info
				// The slice's default memory limit is half the machine's:
				// a fixed machine, or the render follows the test host's.
				e.MemTotalMB = func() int64 { return 4096 }
				// The reference agent had no binary directory set: its
				// units named the bare program.
				e.Paths.PHPBinDir = ""
				root := e.root
				site.RootPath = filepath.Join(e.Paths.SitesRoot, site.Domain)
				if site.Domain == "shop.example.com" {
					// An operator-supplied certificate.
					dir := filepath.Join(e.Paths.CustomCertDir, site.Domain)
					if err := os.MkdirAll(dir, 0o700); err != nil {
						t.Fatal(err)
					}
					for _, f := range []string{"fullchain.pem", "privkey.pem"} {
						if err := os.WriteFile(filepath.Join(dir, f), []byte("pem"), 0o600); err != nil {
							t.Fatal(err)
						}
					}
				}
				files, err := e.Render(t.Context(), site)
				if err != nil {
					t.Fatal(err)
				}
				var templated []rpc.ManagedFile
				for _, f := range files {
					if f.Path == e.dropinPath(site) {
						checkDropin(t, f.Path, site)
						continue
					}
					templated = append(templated, f)
				}
				checkGolden(t, build+"-"+site.Domain, goldenText(t, root, templated))
			})
		}
	}
}
