package site

import (
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/state"
)

// ops/sites SPEC §3.1: the defaults, and what an explicit value changes.
func TestEffectiveLimits(t *testing.T) {
	for _, c := range []struct {
		r     state.Resources
		total int64
		want  Limits
	}{
		{state.Resources{}, 3814, Limits{MemoryMaxMB: 1907, MemoryHighMB: 1716, TasksMax: 512}},
		{state.Resources{}, 1024, Limits{MemoryMaxMB: 1024, MemoryHighMB: 921, TasksMax: 512}},
		{state.Resources{}, 0, Limits{MemoryMaxMB: 1024, MemoryHighMB: 921, TasksMax: 512}},
		{state.Resources{MemoryMaxMB: 256, CPUQuotaPct: 50, TasksMax: 64}, 3814, Limits{MemoryMaxMB: 256, MemoryHighMB: 230, CPUQuotaPct: 50, TasksMax: 64}},
	} {
		if got := EffectiveLimits(c.r, c.total); got != c.want {
			t.Errorf("%+v on %d MB: %+v, want %+v", c.r, c.total, got, c.want)
		}
	}
}

func TestSliceUnit(t *testing.T) {
	s := state.Site{Domain: "a.example", SystemUser: "cg-site-7"}
	u, err := SliceUnit(s, Limits{MemoryMaxMB: 256, MemoryHighMB: 230, CPUQuotaPct: 150, TasksMax: 64})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[Slice]\n", "\nIOAccounting=yes\n", "\nMemoryHigh=230M\n", "\nMemoryMax=256M\n", "\nCPUQuota=150%\n", "\nTasksMax=64\n"} {
		if !strings.Contains(u, want) {
			t.Errorf("slice unit lacks %q:\n%s", want, u)
		}
	}
	u, _ = SliceUnit(s, Limits{MemoryMaxMB: 1024, MemoryHighMB: 921, TasksMax: 512})
	if strings.Contains(u, "CPUQuota") {
		t.Errorf("no quota asked, but the unit has one:\n%s", u)
	}
	if _, err := SliceUnit(state.Site{Domain: "x", SystemUser: "root"}, Limits{}); err == nil {
		t.Error("a slice for a user that is not a site's was rendered")
	}
	if SlicePath("/etc/systemd/system", "cg-site-7") != "/etc/systemd/system/cg-site-7.slice" {
		t.Error(SlicePath("/etc/systemd/system", "cg-site-7"))
	}
}
