package install

import (
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/install/managed"
)

// SR-T8: the sysctl file pins the kernel's link protections, so doctor and
// the installer keep them on whatever the distribution ships.
func TestSysctlPinsLinkProtection(t *testing.T) {
	got := render(t, managed.Sysctl, Vars{})
	for _, line := range []string{"fs.protected_hardlinks = 1", "fs.protected_symlinks = 1"} {
		if !strings.Contains("\n"+got, "\n"+line+"\n") {
			t.Errorf("the sysctl file lacks %q:\n%s", line, got)
		}
	}
}
