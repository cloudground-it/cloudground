package site

import (
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/state"
)

// Fase 9b: names are chosen, within one shape; reserved and system names
// never pass, and another site's default name is not claimable.
func TestDBNames(t *testing.T) {
	for _, ok := range []string{"shop", "shop_data", "wp_blog2", "abc", strings.Repeat("a", 48), "site_data", "cgdata", "cg_x"} {
		if err := CheckDBName(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"", "ab", "Shop", "1shop", "_shop", "shop-data", "shop.data", "shop data", "shop`", "shop%", "shop\\",
		strings.Repeat("a", 49), "mariadb.sys",
		"mysql", "sys", "performance_schema", "information_schema", "root", "debian_sys_maint", "mariadb_sys", "test",
		"cgq_0123456789abcdef", "cgv_x", "cgm_abc", "cgh_abc", "cgz_new",
		"mysql_x", "sys_x", "test_x", "mariadb_x",
	} {
		if err := CheckDBName(bad); err == nil {
			t.Errorf("%q passes", bad)
		}
	}
	if err := ClaimableBy("site_7", 7); err != nil {
		t.Errorf("a site's own default: %v", err)
	}
	for _, other := range []string{"site_8", "site_07", "site_0", "site_70"} {
		if err := ClaimableBy(other, 7); err == nil {
			t.Errorf("%q claimable by site 7", other)
		}
	}
	if PanelDefault(42) != "site_42" || PanelDefaultSite("site_42") != 42 || PanelDefaultSite("site_042") != 0 || PanelDefaultSite("shop") != 0 {
		t.Error("PanelDefault/PanelDefaultSite")
	}
	if !ValidGrant("all") || !ValidGrant("read") || ValidGrant("write") || ValidGrant("") {
		t.Error("ValidGrant")
	}
}

// The primary database a site record names must be a valid name the site
// may claim; an external or absent database is not checked.
func TestValidateChecksThePrimaryDatabase(t *testing.T) {
	db := func(name, user string) state.Site {
		return with(func(s *state.Site) {
			s.Config.Database = state.DatabaseConfig{Enabled: true, Name: name, User: user}
		})
	}
	for _, ok := range []state.Site{db("site_7", "site_7"), db("shop", "shop_admin")} {
		if _, err := Validate(root, ok); err != nil {
			t.Errorf("%q/%q: %v", ok.Config.Database.Name, ok.Config.Database.User, err)
		}
	}
	for _, name := range []string{"mysql", "root", "sys", "performance_schema", "information_schema",
		"debian_sys_maint", "mariadb.sys", "site_8", "site_07", "", "SITE_7", "cgq_0123456789abcdef"} {
		for _, s := range []state.Site{db(name, name), db("site_7", name), db(name, "site_7")} {
			if _, err := Validate(root, s); err == nil {
				t.Errorf("%q/%q passes", s.Config.Database.Name, s.Config.Database.User)
			}
		}
	}
	s := db("site_0", "site_0")
	s.ID = 0
	if _, err := Validate(root, s); err == nil {
		t.Error("a site without an id passes")
	}
	ext := db("wordpress", "wp_user")
	ext.Config.Database.External = true
	if _, err := Validate(root, ext); err != nil {
		t.Errorf("an external database: %v", err)
	}
	off := db("anything", "anything")
	off.Config.Database.Enabled = false
	if _, err := Validate(root, off); err != nil {
		t.Errorf("no managed database: %v", err)
	}
}
