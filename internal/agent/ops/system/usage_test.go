package system

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/rpc"
)

// api SPEC §11.5: the slice's counters as the kernel writes them, a site
// with no running slice, and the disk walk only when asked.
func TestSiteUsage(t *testing.T) {
	root := t.TempDir()
	o := &Ops{Paths: Paths{CgroupRoot: filepath.Join(root, "cg"), SitesRoot: filepath.Join(root, "sites"), LogRoot: filepath.Join(root, "logs")}}
	slice := filepath.Join(root, "cg", "cg.slice", "cg-site.slice", "cg-site-3.slice")
	files := map[string]string{
		"cpu.stat":       "usage_usec 4200000\nuser_usec 4000000\nsystem_usec 200000\n",
		"memory.current": "123456789\n",
		"memory.max":     "268435456\n",
		"pids.current":   "7\n",
		"memory.events":  "low 0\nhigh 4\nmax 9\noom 1\noom_kill 2\n",
		"io.stat":        "8:0 rbytes=1000 wbytes=2000 rios=1 wios=2 dbytes=0 dios=0\n8:16 rbytes=10 wbytes=20 rios=1 wios=1 dbytes=0 dios=0\n",
	}
	for name, c := range files {
		mustWrite(t, filepath.Join(slice, name), c)
	}
	mustWrite(t, filepath.Join(root, "sites", "a.example", "current", "index.php"), strings.Repeat("x", 10000))
	mustWrite(t, filepath.Join(root, "logs", "a.example", "access.log"), "line\n")

	p := rpc.SiteUsageParams{Sites: []rpc.UsageTarget{
		{ID: 3, SystemUser: "cg-site-3", Domain: "a.example"},
		{ID: 4, SystemUser: "cg-site-4", Domain: "b.example"},
	}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	res, err := o.SiteUsage(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	a, b := res.Sites[0], res.Sites[1]
	want := rpc.SiteUsage{ID: 3, Running: true, CPUUsec: 4200000, MemoryBytes: 123456789, MemoryMaxBytes: 268435456,
		IOReadBytes: 1010, IOWriteBytes: 2020, Tasks: 7, OOMKills: 2, DiskBytes: -1, DiskInodes: -1}
	if a != want {
		t.Errorf("site 3: %+v, want %+v", a, want)
	}
	if b != (rpc.SiteUsage{ID: 4, DiskBytes: -1, DiskInodes: -1}) {
		t.Errorf("a site with no slice: %+v", b)
	}

	p.Sites[0].Disk, p.Sites[1].Disk = true, true
	res, _ = o.SiteUsage(context.Background(), p)
	if a := res.Sites[0]; a.DiskBytes < 10000 || a.DiskInodes != 5 {
		// a.example, current, index.php; a.example (logs), access.log
		t.Errorf("disk use %d bytes, %d inodes", a.DiskBytes, a.DiskInodes)
	}
	if b := res.Sites[1]; b.DiskBytes != 0 || b.DiskInodes != 0 {
		t.Errorf("a site with no directories: %+v", b)
	}
}

// The parameters name sites' own users and domains, nothing else.
func TestSiteUsageParamsValidate(t *testing.T) {
	for _, bad := range []rpc.UsageTarget{
		{ID: 3, SystemUser: "cg-site-4", Domain: "a.example"},
		{ID: 3, SystemUser: "root", Domain: "a.example"},
		{ID: 3, SystemUser: "cg-site-3", Domain: "../etc"},
	} {
		p := rpc.SiteUsageParams{Sites: []rpc.UsageTarget{bad}}
		if p.Validate() == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
