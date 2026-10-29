package phpfpm

import (
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/state"
)

// The master's unit carries the sandbox exactly as Sandbox gives it, the
// site shell's list (R-4.5): one place, so the two cannot drift; then its
// own MasterHardening.
func TestUnitCarriesTheSandbox(t *testing.T) {
	s := state.Site{Domain: "a.example", SystemUser: "cg-site-4", PHPVersion: "8.3", RootPath: "/srv/sites/a.example"}
	_, unit, err := RenderUnit(s, "/usr/sbin")
	if err != nil {
		t.Fatal(err)
	}
	lines := Sandbox(s)
	if !strings.Contains(unit, "\n"+strings.Join(append(lines[:len(lines):len(lines)], MasterHardening...), "\n")+"\n\n[Install]") {
		t.Fatalf("the unit's sandbox is not Sandbox():\n%s", unit)
	}
	for _, want := range []string{"BindPaths=/srv/sites/a.example /var/log/cloudground/a.example", "NoNewPrivileges=yes",
		"ProtectProc=invisible", "PrivateTmp=yes", "RestrictNamespaces=yes", "CapabilityBoundingSet=",
		"TemporaryFileSystem=/dev/shm:size=64M,mode=1777,nosuid,nodev,noexec", "SocketBindDeny=1-65535"} {
		found := false
		for _, l := range lines {
			found = found || l == want
		}
		if !found {
			t.Errorf("Sandbox lacks %q", want)
		}
	}
}

// F-6-2: no TCP or UDP port for a site's code. The rule names ports 1-65535,
// not "any": systemd 259's "any" also refuses bind(port 0), which a client
// that binds before it sends needs, and a kernel-chosen ephemeral port
// takes nobody's. Every run inherits it, and only AllowPortBind opens one.
func TestNoPortBinds(t *testing.T) {
	s := state.Site{Domain: "a.example", SystemUser: "cg-site-4", RootPath: "/srv/sites/a.example"}
	for name, lines := range map[string][]string{"Sandbox": Sandbox(s), "RunSandbox": RunSandbox(s)} {
		n := 0
		for _, l := range lines {
			if strings.HasPrefix(l, "SocketBind") {
				n++
				if l != "SocketBindDeny=1-65535" {
					t.Errorf("%s: %q", name, l)
				}
			}
		}
		if n != 1 {
			t.Errorf("%s has %d SocketBind lines, want the one deny", name, n)
		}
	}
	if got := AllowPortBind(8000); got != "SocketBindAllow=tcp:8000" {
		t.Errorf("AllowPortBind: %q", got)
	}
}
