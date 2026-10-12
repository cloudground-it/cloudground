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

// #75: the connector's half of the hardening reaches PHP the same way;
// the readme item is nginx's alone, and a path that is not one is left out.
func TestPoolHardeningEnv(t *testing.T) {
	for _, tc := range []struct {
		typ  state.SiteType
		h    state.WPHardening
		want string
	}{
		{state.SiteWordPress, state.WPHardening{}, ""},
		{state.SiteWordPress, state.WPHardening{BlockReadme: true}, ""},
		{state.SiteWooCommerce, state.WPHardening{XMLRPCOff: true, FileEditOff: true, HideVersion: true, BlockReadme: true, LoginPath: "my-door"},
			"env[CLOUDGROUND_HARDENING] = xmlrpc,file_edit,version\nenv[CLOUDGROUND_LOGIN_PATH] = my-door\n"},
		{state.SiteWordPress, state.WPHardening{FileEditOff: true, LoginPath: "x\nphp_admin_value[y] = z"}, "env[CLOUDGROUND_HARDENING] = file_edit\n"},
		{state.SitePHP, state.WPHardening{XMLRPCOff: true}, ""},
	} {
		s := state.Site{ID: 7, Domain: "a.example.com", Type: tc.typ, SystemUser: "cg-site-7", PHPVersion: "8.4", RootPath: "/srv/sites/a.example.com"}
		s.Config.WPHardening = tc.h
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
			t.Errorf("%s %+v: env lines %q, want %q", tc.typ, tc.h, got, tc.want)
		}
	}
}
