package agent

import (
	"context"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agent/host"
)

// #66: at start, the helper units an earlier agent left running are
// stopped; nothing else is touched.
func TestSweepHelpersStopsLeftovers(t *testing.T) {
	sd := host.NewFakeSystemd()
	for _, u := range []host.UnitProps{
		{Name: "cg-helper-restic-1-1.service", ActiveState: "active"},
		{Name: "cg-restic-2-1.service", ActiveState: "deactivating"},
		{Name: "cg-helper-certbot-3-2.service", ActiveState: "inactive"},
		{Name: "cg-wp-site_1-4-1.service", ActiveState: "active"},
		{Name: "nginx.service", ActiveState: "active"},
	} {
		sd.SetUnit(u)
	}
	a := &Agent{Systemd: sd}
	a.SweepHelpers(context.Background())
	for _, u := range []string{"cg-helper-restic-1-1.service", "cg-restic-2-1.service"} {
		if !sd.Did("stop", u) {
			t.Errorf("%s not stopped", u)
		}
	}
	for _, u := range []string{"cg-helper-certbot-3-2.service", "cg-wp-site_1-4-1.service", "nginx.service"} {
		if sd.Did("stop", u) {
			t.Errorf("%s stopped", u)
		}
	}
}
