package site

import (
	"testing"

	"github.com/cloudground-it/cloudground/internal/state"
)

func TestSupportsPostgres(t *testing.T) {
	for typ, want := range map[state.SiteType]bool{state.SitePHP: true, state.SiteLaravel: true, state.SiteWordPress: false,
		state.SiteWooCommerce: false, state.SiteStatic: false, state.SiteProxy: false} {
		if got := SupportsPostgres(typ); got != want {
			t.Errorf("%s: %v, want %v", typ, got, want)
		}
	}
}
