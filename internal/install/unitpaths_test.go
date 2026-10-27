package install

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agent/ops/sftp"
	"github.com/cloudground-it/cloudground/internal/phpfpm"
	"github.com/cloudground-it/cloudground/internal/state"
	"github.com/cloudground-it/cloudground/internal/units"
)

// everyImageHas are the paths a unit may name that every Ubuntu 24.04 and
// 26.04 image has, container images included (SPEC §3 step 10). /dev/shm is
// also in the private /dev that PrivateDevices makes (phpfpm SPEC §4.1).
var everyImageHas = []string{"/etc", "/srv", "/run", "/var/log", "/usr/local/bin", "/dev/shm"}

// covered says why path p will exist when a unit starts, or "" when nothing
// makes sure it does.
func covered(p string) string {
	if slices.Contains(everyImageHas, p) {
		return "every image"
	}
	for _, d := range directories {
		if d.path == p {
			return "the dirs step"
		}
	}
	return ""
}

// Every path the embedded units name exists before the units start: in the
// dirs table, optional ("-"), or on every image. A path missing on a fresh
// server fails the unit at NAMESPACE (226) for ever: that is how the agent
// never started on a reimaged 26.04 for want of /var/lib/letsencrypt.
func TestUnitPathsExistBeforeTheUnitsStart(t *testing.T) {
	seen := map[string]bool{}
	for _, name := range units.Names {
		b, ok := units.Get(name)
		if !ok {
			t.Fatalf("%s: not embedded", name)
		}
		for _, p := range units.RequiredPaths(strings.Split(string(b), "\n")) {
			seen[p] = true
			if !filepath.IsAbs(p) || filepath.Clean(p) != p {
				t.Errorf("%s: %q is not a clean absolute path", name, p)
				continue
			}
			if covered(p) == "" {
				t.Errorf("%s names %s, which a fresh server may not have: add it to directories (steps.go) or, only if the unit never writes it, prefix it with -", name, p)
			}
		}
	}
	// The parser read the units at all.
	for _, p := range []string{"/var/lib/letsencrypt", "/var/spool/cron", "/srv", "/var/lib/cloudground"} {
		if !seen[p] {
			t.Errorf("%s not found in the units: the parser missed it", p)
		}
	}
}

// The same for the fixed paths of the site sandbox (the PHP master's unit,
// the shell, cron and wp-cli runs) and the shell's extra lines; the site's
// own tree and log directory are made by provisioning before any of them.
func TestSandboxPathsExistBeforeTheRunsStart(t *testing.T) {
	site := state.Site{Domain: "example.test", RootPath: "/srv/sites/example.test", SystemUser: "cg-site-1"}
	own := []string{site.RootPath, filepath.Join(phpfpm.LogRoot, site.Domain)}
	lines := append(phpfpm.RunSandbox(site), sftp.ShellQuiet...)
	ps := units.RequiredPaths(lines)
	if !slices.Contains(ps, "/srv/sites") {
		t.Fatalf("the parser missed the sandbox's paths: %v", ps)
	}
	for _, p := range ps {
		if slices.Contains(own, p) {
			continue
		}
		if covered(p) == "" {
			t.Errorf("the site sandbox names %s, which a fresh server may not have", p)
		}
	}
}
