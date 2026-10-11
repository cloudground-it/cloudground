package sites

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// SPEC §3.1: every site, PHP or not, has a slice unit with its accounting
// and limits; a changed limit is rewritten and loaded, and the unit goes
// with the site.
func TestSliceUnitCarriesTheLimits(t *testing.T) {
	e := newEnv(t)
	e.MemTotalMB = func() int64 { return 4096 }
	ctx := context.Background()
	for _, s := range []state.Site{e.site(4, "app.example.com", state.SitePHP), e.site(5, "static.example.com", state.SiteStatic)} {
		e.create(t, s)
		b, err := os.ReadFile(e.slicePath(s))
		if err != nil {
			t.Fatalf("%s: no slice unit: %v", s.Domain, err)
		}
		// 4096 MB on the test machine: half of it by default.
		for _, want := range []string{"\nMemoryAccounting=yes\n", "\nIOAccounting=yes\n", "\nMemoryMax=2048M\n", "\nTasksMax=512\n"} {
			if !strings.Contains(string(b), want) {
				t.Errorf("%s: the slice unit lacks %q:\n%s", s.Domain, want, b)
			}
		}
		if strings.Contains(string(b), "CPUQuota") {
			t.Errorf("%s: a CPU quota by default:\n%s", s.Domain, b)
		}

		s.Config.Resources.MemoryMaxMB, s.Config.Resources.CPUQuotaPct, s.Config.Resources.TasksMax = 256, 150, 64
		reloads := reloadsOf(e)
		files, err := e.Render(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		b, _ = os.ReadFile(e.slicePath(s))
		for _, want := range []string{"\nMemoryHigh=230M\n", "\nMemoryMax=256M\n", "\nCPUQuota=150%\n", "\nTasksMax=64\n"} {
			if !strings.Contains(string(b), want) {
				t.Errorf("%s: after the change the slice unit lacks %q:\n%s", s.Domain, want, b)
			}
		}
		if reloadsOf(e) == reloads {
			t.Errorf("%s: new limits written but systemd was not reloaded", s.Domain)
		}
		recorded := false
		for _, f := range files {
			recorded = recorded || f.Path == e.slicePath(s)
		}
		if !recorded {
			t.Errorf("%s: the slice unit is not among the managed files (drift)", s.Domain)
		}

		if _, err := e.Delete(ctx, rpc.SiteRef{Site: s}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(e.slicePath(s)); !os.IsNotExist(err) {
			t.Errorf("%s: the slice unit outlived the site: %v", s.Domain, err)
		}
	}
}

func reloadsOf(e *env) int {
	n := 0
	for _, o := range e.systemd.Ops() {
		if o == "daemon-reload" {
			n++
		}
	}
	return n
}
