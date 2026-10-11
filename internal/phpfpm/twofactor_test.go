package phpfpm

import (
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/state"
)

// #74: the connector's two-factor mode reaches PHP through the pool's
// environment, on WordPress sites only, and only for a mode it knows.
func TestPoolTwoFactorEnv(t *testing.T) {
	for _, tc := range []struct {
		typ  state.SiteType
		mode string
		want string
	}{
		{state.SiteWordPress, "", ""},
		{state.SiteWordPress, state.TwoFactorOptional, "env[CLOUDGROUND_2FA] = optional\n"},
		{state.SiteWooCommerce, state.TwoFactorRequired, "env[CLOUDGROUND_2FA] = required\n"},
		{state.SiteWordPress, "always\nphp_admin_value[x] = y", ""},
		{state.SitePHP, state.TwoFactorRequired, ""},
	} {
		s := state.Site{ID: 7, Domain: "a.example.com", Type: tc.typ, SystemUser: "cg-site-7", PHPVersion: "8.4", RootPath: "/srv/sites/a.example.com"}
		s.Config.WPTwoFactor = tc.mode
		_, conf, err := RenderPool(s, 1024)
		if err != nil {
			t.Fatal(err)
		}
		got := ""
		for _, line := range strings.SplitAfter(conf, "\n") {
			if strings.HasPrefix(line, "env[") {
				got += line
			}
		}
		if got != tc.want {
			t.Errorf("%s %q: env lines %q, want %q", tc.typ, tc.mode, got, tc.want)
		}
	}
}
