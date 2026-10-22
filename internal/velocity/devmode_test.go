package velocity

import (
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/state"
)

// Development mode (SPEC §1.5): while the end is set the site has no cache,
// whatever the clock says; clearing it gives the policy back unchanged.
func TestDevModeBypassesTheCache(t *testing.T) {
	site := state.Site{Type: state.SiteWordPress, Config: state.SiteConfig{CacheEnabled: true}}
	normal := PolicyFor(site)
	if !normal.Enabled {
		t.Fatal("control: the site has a cache")
	}
	for _, until := range []time.Time{time.Now().Add(time.Hour), time.Now().Add(-time.Hour)} {
		site.Config.DevModeUntil = until
		if PolicyFor(site).Enabled {
			t.Errorf("a cache with development mode set until %s", until)
		}
	}
	site.Config.DevModeUntil = time.Time{}
	if got := PolicyFor(site); got.Enabled != normal.Enabled || got.TTLSec != normal.TTLSec {
		t.Errorf("after the mode: %+v, want %+v", got, normal)
	}
}
