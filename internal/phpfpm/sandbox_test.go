package phpfpm

import (
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/state"
)

// The master's unit carries the sandbox exactly as Sandbox gives it, the
// site shell's list (R-4.5): one place, so the two cannot drift.
func TestUnitCarriesTheSandbox(t *testing.T) {
	s := state.Site{Domain: "a.example", SystemUser: "cg-site-4", PHPVersion: "8.3", RootPath: "/srv/sites/a.example"}
	_, unit, err := RenderUnit(s, "/usr/sbin")
	if err != nil {
		t.Fatal(err)
	}
	lines := Sandbox(s)
	if !strings.Contains(unit, "\n"+strings.Join(lines, "\n")+"\n\n[Install]") {
		t.Fatalf("the unit's sandbox is not Sandbox():\n%s", unit)
	}
	for _, want := range []string{"BindPaths=/srv/sites/a.example /var/log/cloudground/a.example", "NoNewPrivileges=yes",
		"ProtectProc=invisible", "PrivateTmp=yes", "RestrictNamespaces=yes", "CapabilityBoundingSet="} {
		found := false
		for _, l := range lines {
			found = found || l == want
		}
		if !found {
			t.Errorf("Sandbox lacks %q", want)
		}
	}
}
