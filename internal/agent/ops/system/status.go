package system

import (
	"context"

	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/sysprobe"
	"github.com/cloudground-it/cloudground/internal/version"
)

// SystemStatus is the dashboard's capacity snapshot (SPEC.md §1). It starts
// no process: the three running flags are unit properties read over D-Bus.
func (o *Ops) SystemStatus(ctx context.Context) (rpc.SystemStatusResult, error) {
	f, err := sysprobe.Probe(o.Paths.SitesRoot)
	if err != nil {
		return rpc.SystemStatusResult{}, err
	}
	res := rpc.SystemStatusResult{
		CPUCount:        f.CPUCount,
		Load1:           f.Load1,
		MemTotalMB:      f.MemTotalMB,
		MemAvailableMB:  f.MemAvailableMB,
		DiskTotalMB:     f.DiskTotalMB,
		DiskFreeMB:      f.DiskFreeMB,
		UptimeSeconds:   f.UptimeSeconds,
		AgentVersion:    version.Version,
		CPUHeadroomPct:  cpuHeadroom(f.Load1, f.CPUCount),
		MemHeadroomPct:  sysprobe.HeadroomPct(f.MemAvailableMB, f.MemTotalMB),
		DiskHeadroomPct: sysprobe.HeadroomPct(f.DiskFreeMB, f.DiskTotalMB),
	}
	res.NginxRunning = o.unitActive(ctx, "nginx")
	res.MariaDBRunning = o.unitActive(ctx, "mariadb")
	res.RedisRunning = o.unitActive(ctx, "redis-server")
	return res, nil
}

// cpuHeadroom is the share of the CPUs the 1-minute load leaves idle.
func cpuHeadroom(load1 float64, cpus int) int {
	pct := 100 - int(load1*100/float64(max(cpus, 1)))
	return min(max(pct, 0), 100)
}

// unitActive reads one unit's ActiveState. A systemd that cannot be asked
// reads as not running: the load numbers are still worth returning.
func (o *Ops) unitActive(ctx context.Context, unit string) bool {
	p, err := o.Systemd.UnitProps(ctx, unit)
	if err != nil {
		o.log().Warn("system status: could not read a unit", "unit", unit, "err", err)
		return false
	}
	return p.ActiveState == "active"
}
