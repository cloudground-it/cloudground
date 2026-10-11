package agent

import "context"

// helperUnits are the transient units the agent's helpers run in (host
// SPEC: helpers): restic, mariadb-dump, the mariadb client and certbot, and
// the sandboxed files backups.
var helperUnits = []string{"cg-helper-*.service", "cg-restic-*.service"}

// SweepHelpers stops every helper unit still loaded. Run it at start,
// before serving: a helper is not the agent's child, so one started by an
// agent that has since stopped or died outlives it, with nobody reading its
// output, and a restic among them holds its repository's lock. The stop is
// the unit's own (SIGINT for restic, which removes its lock).
func (a *Agent) SweepHelpers(ctx context.Context) {
	if a.Systemd == nil {
		return
	}
	units, err := a.Systemd.ListUnits(ctx, helperUnits...)
	if err != nil {
		a.log().Warn("helper units left by an earlier agent not listed", "err", err)
		return
	}
	for _, u := range units {
		if u.ActiveState == "inactive" {
			continue
		}
		if err := a.Systemd.Stop(ctx, u.Name); err != nil {
			a.log().Warn("a helper unit left by an earlier agent not stopped", "unit", u.Name, "err", err)
			continue
		}
		a.log().Info("stopped a helper unit left by an earlier agent", "unit", u.Name)
	}
}
