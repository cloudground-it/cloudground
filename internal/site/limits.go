package site

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/cloudground-it/cloudground/internal/state"
)

// Bounds of the limits a site may be given (api SPEC §3.4).
const (
	MinMemoryMaxMB = 128
	MaxMemoryMaxMB = 1 << 20
	MinCPUQuotaPct = 10
	MaxCPUQuotaPct = 100 * 256
	MinTasksMax    = 32
	MaxTasksMax    = 65536
)

// Limits are a site's limits as the kernel enforces them on its slice.
type Limits struct {
	MemoryMaxMB int `json:"memory_max_mb"`
	// MemoryHighMB is where the kernel starts reclaiming and slowing the
	// site, before MemoryMax kills: nine tenths of it.
	MemoryHighMB int `json:"memory_high_mb"`
	// CPUQuotaPct is 0 when the site may use every CPU.
	CPUQuotaPct int `json:"cpu_quota_pct"`
	TasksMax    int `json:"tasks_max"`
}

// EffectiveLimits are r's limits with the measured defaults filled in, on a
// server of memTotalMB (ops/sites SPEC §3.1). The memory default is half the
// server, never under state.DefaultMemoryFloorMB: one site cannot take the
// memory every other site and the database need, and a normal WooCommerce
// site (561 MB at its peak under the bench) never reaches it.
func EffectiveLimits(r state.Resources, memTotalMB int64) Limits {
	l := Limits{MemoryMaxMB: r.MemoryMaxMB, CPUQuotaPct: r.CPUQuotaPct, TasksMax: r.TasksMax}
	if l.MemoryMaxMB <= 0 {
		l.MemoryMaxMB = max(int(memTotalMB/2), state.DefaultMemoryFloorMB)
	}
	if l.TasksMax <= 0 {
		l.TasksMax = state.DefaultTasksMax
	}
	l.MemoryHighMB = l.MemoryMaxMB * 9 / 10
	return l
}

// SliceUnitName is the file name of a site's slice unit.
func SliceUnitName(systemUser string) string { return Slice(systemUser) }

// SliceUnit renders the unit file of a site's slice (ops/sites SPEC §3.1):
// the accounting the usage figures are read from, and the limits.
func SliceUnit(s state.Site, l Limits) (string, error) {
	if !ValidSystemUser(s.SystemUser) {
		return "", fmt.Errorf("site %q: invalid system user", s.Domain)
	}
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	w("# Managed by CloudGround — do not edit. Changes are detected as drift.")
	w("[Unit]")
	w("Description=Site %s: its PHP master, cron, wp-cli and shell", s.Domain)
	w("")
	w("[Slice]")
	w("# Counted for the panel's usage figures.")
	w("CPUAccounting=yes")
	w("MemoryAccounting=yes")
	w("IOAccounting=yes")
	w("TasksAccounting=yes")
	w("# Over MemoryHigh the kernel reclaims and slows the site; at MemoryMax it")
	w("# kills one of its processes. No other site is touched either way.")
	w("MemoryHigh=%dM", l.MemoryHighMB)
	w("MemoryMax=%dM", l.MemoryMaxMB)
	if l.CPUQuotaPct > 0 {
		w("CPUQuota=%d%%", l.CPUQuotaPct)
	}
	w("TasksMax=%d", l.TasksMax)
	return b.String(), nil
}

// SlicePath is where a site's slice unit is written in systemdDir.
func SlicePath(systemdDir, systemUser string) string {
	return filepath.Join(systemdDir, SliceUnitName(systemUser))
}
