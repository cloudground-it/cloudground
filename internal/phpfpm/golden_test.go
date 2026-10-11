package phpfpm

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/state"
)

// The golden files pin every rendered byte and every sized number over a
// matrix of machines and sites (Phase 5 gate: a rewrite must not move one).
// They were captured from the code before the rewrite; regenerate them only
// for an intended change, with CG_GOLDEN_UPDATE=1, and the diff must be
// exactly that change.

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if os.Getenv("CG_GOLDEN_UPDATE") != "" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (CG_GOLDEN_UPDATE=1 writes it)", err)
	}
	if string(want) != got {
		wl, gl := strings.Split(string(want), "\n"), strings.Split(got, "\n")
		for i := 0; i < len(wl) || i < len(gl); i++ {
			var w, g string
			if i < len(wl) {
				w = wl[i]
			}
			if i < len(gl) {
				g = gl[i]
			}
			if w != g {
				t.Fatalf("%s differs from the golden at line %d:\nwant %q\ngot  %q", name, i+1, w, g)
			}
		}
	}
}

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

func withCPUs(t *testing.T, n int) {
	old := CPUCount
	t.Cleanup(func() { CPUCount = old })
	CPUCount = n
}

var (
	goldenCPUs  = []int{0, 1, 2, 3, 4, 8, 16, 32}
	goldenTypes = []state.SiteType{state.SiteWordPress, state.SiteWooCommerce, state.SiteLaravel,
		state.SitePHP, state.SiteStatic, state.SiteProxy, ""}
	goldenProfiles = []state.Profile{state.ProfileBalanced, state.ProfileCommerce, state.ProfileMaximum, ""}
)

func goldenSite(id int64, typ state.SiteType, prof state.Profile) state.Site {
	return state.Site{
		ID: id, Domain: fmt.Sprintf("site%d.example.com", id), Type: typ, Profile: prof,
		PHPVersion: "8.4", SystemUser: fmt.Sprintf("cg-site-%d", id),
		RootPath: fmt.Sprintf("/srv/sites/site%d.example.com", id),
	}
}

// goldenConfigs are the per-site overrides the matrix crosses.
var goldenConfigs = []struct {
	name string
	set  func(*state.Site)
}{
	{"default", func(*state.Site) {}},
	{"php-overrides", func(s *state.Site) {
		s.Config.PHP = state.PHPSettings{MemoryLimitMB: 768, UploadMaxMB: 200, MaxExecutionSeconds: 300}
	}},
	{"short-exec", func(s *state.Site) { s.Config.PHP.MaxExecutionSeconds = 30 }},
	{"exec-115", func(s *state.Site) { s.Config.PHP.MaxExecutionSeconds = 115 }},
	{"workers-7", func(s *state.Site) { s.Config.Resources.PHPWorkers = 7 }},
	{"workers-1", func(s *state.Site) { s.Config.Resources.PHPWorkers = 1 }},
	{"memory-high", func(s *state.Site) { s.Config.Resources.MemoryHighMB = 2048 }},
}

func TestGoldenSizeWorkers(t *testing.T) {
	var b strings.Builder
	for _, cpu := range goldenCPUs {
		withCPUs(t, cpu)
		for _, mem := range []int{-1, 0, 79, 80, 159, 160, 200, 240, 320, 480, 512, 1024, 2048, 4096, 5120, 8192, 65536} {
			for _, req := range []int{-1, 0, 1, 2, 3, 6, 10, 64, 100} {
				fmt.Fprintf(&b, "cpu=%d mem=%d req=%d %+v\n", cpu, mem, req, SizeWorkers(mem, req))
			}
		}
	}
	checkGolden(t, "workers", b.String())
}

func TestGoldenNames(t *testing.T) {
	var b strings.Builder
	fmt.Fprintf(&b, "LogRoot=%s\nMasterConfDir=%s\n", LogRoot, MasterConfDir)
	for _, u := range []string{"cg-site-1", "cg-site-1234", ""} {
		fmt.Fprintf(&b, "UnitName(%q)=%s\nSocketFor(%q)=%s\n", u, UnitName(u), u, SocketFor(u))
	}
	for _, typ := range goldenTypes {
		fmt.Fprintf(&b, "SizeSiteCaches(%q)=%+v\n", typ, SizeSiteCaches(typ))
	}
	checkGolden(t, "names", b.String())
}

func TestGoldenPools(t *testing.T) {
	var b strings.Builder
	id := int64(0)
	for _, cpu := range []int{2, 8} {
		withCPUs(t, cpu)
		for _, typ := range goldenTypes {
			for _, prof := range goldenProfiles {
				for _, mem := range []int{0, 160, 1024, 4096} {
					for _, cfg := range goldenConfigs {
						id++
						s := goldenSite(id, typ, prof)
						cfg.set(&s)
						path, content, err := RenderPool(s, mem)
						fmt.Fprintf(&b, "cpu=%d type=%q profile=%q mem=%d cfg=%s path=%s err=%v sha=%s\n",
							cpu, typ, prof, mem, cfg.name, path, err, digest(content))
					}
				}
			}
		}
	}
	// Missing identity fields are refused.
	for _, s := range []state.Site{{Domain: "a.test", PHPVersion: "8.4"}, {Domain: "b.test", SystemUser: "cg-site-2"}} {
		path, content, err := RenderPool(s, 1024)
		fmt.Fprintf(&b, "missing: path=%q content=%q err=%v\n", path, content, err)
	}
	checkGolden(t, "pools", b.String())

	// A few in full, so a difference can be read and not only detected.
	withCPUs(t, 2)
	var full strings.Builder
	samples := []struct {
		s   state.Site
		mem int
	}{
		{goldenSite(1, state.SiteWordPress, state.ProfileBalanced), 1024},
		{goldenSite(2, state.SiteWooCommerce, state.ProfileCommerce), 4096},
		{goldenSite(3, state.SiteLaravel, state.ProfileMaximum), 0},
	}
	ov := goldenSite(4, state.SitePHP, state.ProfileBalanced)
	goldenConfigs[1].set(&ov)
	samples = append(samples, struct {
		s   state.Site
		mem int
	}{ov, 160})
	for _, c := range samples {
		_, content, err := RenderPool(c.s, c.mem)
		fmt.Fprintf(&full, "===== %s mem=%d err=%v\n%s", c.s.Domain, c.mem, err, content)
	}
	checkGolden(t, "pool-samples", full.String())
}

func TestGoldenUnits(t *testing.T) {
	var b strings.Builder
	id := int64(100)
	for _, typ := range goldenTypes {
		for _, bin := range []string{"/usr/sbin", "/opt/php/bin"} {
			for _, ver := range []string{"8.3", "8.4"} {
				id++
				s := goldenSite(id, typ, state.ProfileBalanced)
				s.PHPVersion = ver
				name, content, err := RenderUnit(s, bin)
				fmt.Fprintf(&b, "type=%q bin=%s ver=%s name=%s err=%v sha=%s\n", typ, bin, ver, name, err, digest(content))
			}
		}
	}
	for _, s := range []state.Site{
		{Domain: "a.test", PHPVersion: "8.4", RootPath: "/srv/sites/a.test"},
		{Domain: "b.test", SystemUser: "cg-site-2", RootPath: "/srv/sites/b.test"},
		{Domain: "c.test", SystemUser: "cg-site-3", PHPVersion: "8.4"},
	} {
		name, content, err := RenderUnit(s, "/usr/sbin")
		fmt.Fprintf(&b, "missing: name=%q content=%q err=%v\n", name, content, err)
	}
	for _, typ := range []state.SiteType{state.SiteWordPress, state.SiteWooCommerce} {
		_, content, err := RenderUnit(goldenSite(9, typ, state.ProfileCommerce), "/usr/sbin")
		fmt.Fprintf(&b, "===== %s err=%v\n%s", typ, err, content)
	}
	checkGolden(t, "units", b.String())
}

func TestGoldenSandbox(t *testing.T) {
	var b strings.Builder
	for _, s := range []state.Site{goldenSite(1, state.SiteWordPress, ""), {Domain: "x.test", RootPath: "/srv/sites/x.test/staging"}, {}} {
		fmt.Fprintf(&b, "===== %q\n%s\n", s.Domain, strings.Join(Sandbox(s), "\n"))
	}
	checkGolden(t, "sandbox", b.String())
}
