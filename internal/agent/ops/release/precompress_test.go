package release

import (
	"bytes"
	"compress/gzip"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/connector"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/state"
)

func testRoot(t *testing.T, dir string) *sitefs.Root {
	t.Helper()
	r, err := sitefs.Open(dir, sitefs.Owner{UID: os.Getuid(), GID: os.Getgid()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func gunzip(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func noise(n int) string {
	rng := rand.New(rand.NewPCG(1, 2))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(rng.Uint32())
	}
	return string(b)
}

func TestPrecompress(t *testing.T) {
	dir := t.TempDir()
	css := strings.Repeat("body{color:red;margin:0;padding:0}\n", 200)
	mkTree(t, dir, map[string]string{
		"a/style.css":   css,
		"a/b/app.JS":    strings.Repeat("function f(){return 1}\n", 100),
		"tiny.css":      "a{}",
		"photo.jpg":     strings.Repeat("x", 5000),
		"random.svg":    noise(4000), // gzip would not be smaller
		"index.php":     strings.Repeat("<?php echo 1; ?>\n", 200),
		"huge.json":     strings.Repeat("{}", 6<<20),
		"has-gz.css":    css,
		"has-gz.css.gz": "current",
	})
	before := time.Now().Add(-time.Minute)
	os.Chtimes(filepath.Join(dir, "has-gz.css"), before, before)
	os.Mkdir(filepath.Join(dir, "dir.css.gz"), 0o755)
	mkTree(t, dir, map[string]string{"dir.css": css})
	n, err := Precompress(ctx, testRoot(t, dir), ".", 3)
	if err != nil || n != 2 {
		t.Fatalf("Precompress = %d, %v; want 2 files", n, err)
	}
	if gunzip(t, filepath.Join(dir, "a/style.css.gz")) != css {
		t.Fatal("style.css.gz does not hold style.css")
	}
	gunzip(t, filepath.Join(dir, "a/b/app.JS.gz"))
	for _, p := range []string{"tiny.css.gz", "photo.jpg.gz", "random.svg.gz", "index.php.gz", "huge.json.gz"} {
		if _, err := os.Lstat(filepath.Join(dir, p)); err == nil {
			t.Errorf("%s should not exist", p)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "has-gz.css.gz")); string(b) != "current" {
		t.Error("a .gz newer than its source was rebuilt")
	}
	if fi, _ := os.Stat(filepath.Join(dir, "a/style.css.gz")); fi.Mode().Perm() != 0o644 {
		t.Errorf(".gz mode %v", fi.Mode().Perm())
	}
}

// Precompression must not write through a planted temporary name, must
// rebuild a .gz older than its source, and must remove one whose source no
// longer compresses: gzip_static would go on serving the old asset.
func TestPrecompressStaleAndPlanted(t *testing.T) {
	dir := t.TempDir()
	css := filepath.Join(dir, "app.css")
	mkTree(t, dir, map[string]string{"app.css": strings.Repeat("body{color:red}\n", 200), "logo.svg": noise(3000)})
	victim := filepath.Join(t.TempDir(), "victim")
	os.WriteFile(victim, []byte("keep"), 0o600)
	os.Symlink(victim, css+".gz.tmp")
	if _, err := Precompress(ctx, testRoot(t, dir), ".", 1); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(victim); string(got) != "keep" {
		t.Fatal("precompression wrote through a planted .gz.tmp symlink")
	}

	old := time.Now().Add(-time.Hour)
	for _, p := range []string{css + ".gz", filepath.Join(dir, "logo.svg.gz")} {
		os.WriteFile(p, []byte("stale"), 0o644)
		os.Chtimes(p, old, old)
	}
	if _, err := Precompress(ctx, testRoot(t, dir), ".", 1); err != nil {
		t.Fatal(err)
	}
	if gunzip(t, css+".gz") != strings.Repeat("body{color:red}\n", 200) {
		t.Fatal("a .gz older than its source was kept")
	}
	if _, err := os.Lstat(filepath.Join(dir, "logo.svg.gz")); err == nil {
		t.Fatal("a stale .gz of an asset that no longer compresses was kept")
	}
}

// The checksum is the tree's, whatever the number of workers, and changes
// with any file, .gz files written by the deploy included.
func TestChecksum(t *testing.T) {
	sum := func(workers int, files map[string]string) string {
		dir := t.TempDir()
		mkTree(t, dir, files)
		os.Symlink("../../shared/wp-config.php", filepath.Join(dir, "wp-config.php"))
		res, err := scan(ctx, testRoot(t, dir), ".", workers, true)
		if err != nil {
			t.Fatal(err)
		}
		return res.checksum
	}
	files := map[string]string{"a.css": strings.Repeat("a{}\n", 600), "b/c.php": "<?php", "b/d.txt": "x"}
	base := sum(1, files)
	if base != sum(8, files) {
		t.Fatal("the checksum depends on the number of workers")
	}
	changed := map[string]string{"a.css": strings.Repeat("a{}\n", 600), "b/c.php": "<?php ", "b/d.txt": "x"}
	if base == sum(4, changed) {
		t.Fatal("the checksum did not change with a file")
	}
	moved := map[string]string{"a.css": strings.Repeat("a{}\n", 600), "b/c.ph": "p<?php", "b/d.txt": "x"}
	if base == sum(4, moved) {
		t.Fatal("the checksum did not change with a name")
	}
}

// The fixture is about as compressible as real code, so the benchmark
// below measures what a deploy really does.
func TestFixtureLooksLikeAWordPressTree(t *testing.T) {
	dir := t.TempDir()
	size := writeFixture(t, dir, 1500)
	n, err := Precompress(ctx, testRoot(t, dir), ".", 0)
	if err != nil {
		t.Fatal(err)
	}
	if n < 300 || size < 10<<20 {
		t.Fatalf("1500 files, %d bytes, %d compressed", size, n)
	}
}

// BenchmarkDeploy builds a release from a WordPress + WooCommerce sized
// tree (15,000 files): copy, layout links, connector, precompression and
// checksum, as DeployRelease does. Run it as root (the container suite):
//
//	go test -run '^$' -bench Deploy -benchtime 10x ./internal/agent/ops/release/
func BenchmarkDeploy(b *testing.B) {
	f := &fixture{owner: sitefs.Owner{UID: os.Getuid(), GID: os.Getgid()}}
	root := filepath.Join(b.TempDir(), "shop.example.com")
	f.site = state.Site{ID: 3, Domain: "shop.example.com", Type: state.SiteWooCommerce, SystemUser: "cg-site-3", RootPath: root}
	f.site.Config.ObjectCache = true
	f.o = &Ops{Open: func(s state.Site) (*sitefs.Root, error) { return sitefs.Open(s.RootPath, f.owner) }}
	for _, d := range []string{"releases", "shared/uploads"} {
		os.MkdirAll(filepath.Join(root, d), 0o750)
	}
	os.WriteFile(filepath.Join(root, "shared/wp-config.php"), []byte("<?php\n"), 0o640)
	os.WriteFile(filepath.Join(root, "shared/object-cache.php"), []byte(connector.ObjectCacheAPCu), 0o640)
	size := writeFixture(b, filepath.Join(root, "deploy/build"), 15000)
	b.Logf("fixture: 15000 files, %d MB", size>>20)
	b.ResetTimer()
	for i := range b.N {
		id := ids(b.N)[i]
		t0 := time.Now()
		if _, err := f.deploy(id); err != nil {
			b.Fatal(err)
		}
		b.Logf("run %d: %v", i, time.Since(t0))
		b.StopTimer()
		os.RemoveAll(filepath.Join(root, "releases", id))
		b.StartTimer()
	}
}
