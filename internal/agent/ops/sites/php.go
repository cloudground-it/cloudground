package sites

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/phpfpm"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/site"
)

// fpmBinary is the FPM program of a PHP version (Debian and Ubuntu put it
// in /usr/sbin).
func (o *Ops) fpmBinary(version string) string {
	dir := o.Paths.PHPBinDir
	if dir == "" {
		dir = "/usr/sbin"
	}
	return filepath.Join(dir, "php-fpm"+version)
}

// PHPVersions is the PHP versions this machine can run a site's master
// with, newest first. A version is installed when its FPM binary is an
// executable regular file: /etc/php/<version> proves nothing, since
// rendering a pool creates it (AGENTS.md).
func (o *Ops) PHPVersions() []string {
	matches, _ := filepath.Glob(o.fpmBinary("*"))
	var out []string
	for _, m := range matches {
		v := strings.TrimPrefix(filepath.Base(m), "php-fpm")
		if !site.ValidPHPVersion(v) {
			continue
		}
		if fi, err := os.Stat(m); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
			out = append(out, v)
		}
	}
	slices.Sort(out)
	slices.Reverse(out)
	return out
}

// checkPHPInstalled refuses a version the machine does not have, naming
// the ones it has. "" (the panel's default) passes, and so does anything
// when no version can be found at all (a layout this code does not know).
func (o *Ops) checkPHPInstalled(version string) error {
	if version == "" {
		return nil
	}
	have := o.PHPVersions()
	if len(have) == 0 || slices.Contains(have, version) {
		return nil
	}
	return agentrpc.Invalid("PHP %s is not installed on this server (available: %s)", version, strings.Join(have, ", "))
}

// aptEnv is apt's environment: no question on a terminal, the C locale.
var aptEnv = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C", "DEBIAN_FRONTEND=noninteractive"}

// PHPExtensions lists what a PHP version loads (SPEC §9): `php-fpm<v> -m`,
// the FPM binary the sites' masters run, so the list is what they load, and
// the common extensions (rpc.PHPExtensions) with whether each is loaded.
func (o *Ops) PHPExtensions(ctx context.Context, p rpc.PHPExtensionsParams) (rpc.PHPExtensionsResult, error) {
	if err := o.checkPHPExists(p.Version); err != nil {
		return rpc.PHPExtensionsResult{}, err
	}
	res, err := o.Exec.Run(ctx, host.Cmd{Name: o.fpmBinary(p.Version), Args: []string{"-m"}, Env: aptEnv[:2], Timeout: 30 * time.Second})
	if err != nil {
		return rpc.PHPExtensionsResult{}, fmt.Errorf("php-fpm%s -m: %w", p.Version, err)
	}
	return phpModules(p.Version, string(res.Stdout)), nil
}

// phpModules reads `php -m`: one module a line under "[PHP Modules]" and
// "[Zend Modules]", the Zend ones repeated in both.
func phpModules(version, out string) rpc.PHPExtensionsResult {
	seen := map[string]bool{}
	r := rpc.PHPExtensionsResult{Version: version, Modules: []string{}}
	for _, line := range strings.Split(out, "\n") {
		m := strings.ToLower(strings.TrimSpace(line))
		if m == "" || strings.HasPrefix(m, "[") || seen[m] {
			continue
		}
		seen[m] = true
		r.Modules = append(r.Modules, m)
	}
	slices.Sort(r.Modules)
	for _, e := range rpc.PHPExtensions {
		r.Extensions = append(r.Extensions, rpc.PHPExtensionState{Name: e.Name, Installed: slices.ContainsFunc(e.Modules, func(m string) bool { return seen[m] })})
	}
	return r
}

// InstallPHPExtension installs one common extension for a PHP version
// (SPEC §9): `apt-get install php<v>-<ext>`, then a restart of every site
// master that runs that version (versionMasters), since a master loads its
// extensions when it starts. The answer is what the version loads
// afterwards.
func (o *Ops) InstallPHPExtension(ctx context.Context, p rpc.PHPExtensionInstallParams) (rpc.PHPExtensionsResult, error) {
	if err := o.checkPHPExists(p.Version); err != nil {
		return rpc.PHPExtensionsResult{}, err
	}
	pkg := "php" + p.Version + "-" + p.Extension
	if _, err := o.Exec.Run(ctx, host.Cmd{Name: "systemd-run", Args: aptInstallArgs(pkg), Env: aptEnv[:2], Timeout: 13 * time.Minute}); err != nil {
		return rpc.PHPExtensionsResult{}, fmt.Errorf("apt-get install %s: %w", pkg, err)
	}
	var failed []error
	for _, unit := range o.versionMasters(p.Version) {
		if err := o.Systemd.Restart(ctx, unit); err != nil {
			failed = append(failed, fmt.Errorf("%s: %w", unit, err))
		}
	}
	if err := errors.Join(failed...); err != nil {
		return rpc.PHPExtensionsResult{}, fmt.Errorf("%s is installed, but PHP did not restart for: %w", pkg, err)
	}
	return o.PHPExtensions(ctx, rpc.PHPExtensionsParams{Version: p.Version})
}

// aptInstallArgs is the systemd-run argv (after the binary) that installs
// pkg (SPEC §9). apt cannot run in the agent's own sandbox: /usr, dpkg's
// database and apt's cache are read-only there (ProtectSystem=strict), so
// apt-get runs as a transient root service, which systemd starts outside
// it. It waits for its end and passes apt's output and exit status back.
// RuntimeMaxSec bounds the run even when the agent stops waiting first: a
// dpkg run is not interrupted by the caller going away. apt waits for a
// lock another apt holds (unattended-upgrades) instead of failing at once.
func aptInstallArgs(pkg string) []string {
	a := []string{"--quiet", "--collect", "--wait", "--pipe", "--service-type=exec",
		"--description=CloudGround: install " + pkg, "-p", "RuntimeMaxSec=720"}
	for _, kv := range aptEnv {
		a = append(a, "--setenv="+kv)
	}
	return append(a, "--", "apt-get", "-o", "DPkg::Lock::Timeout=120", "install", "-y", "-qq", "--no-install-recommends", pkg)
}

// versionMasters are the units of the site masters that run version: the
// agent's own unit files (cloudground-php-cg-site-<id>.service), read
// rather than asked for, whose ExecStart starts that version's FPM binary.
func (o *Ops) versionMasters(version string) []string {
	files, _ := filepath.Glob(filepath.Join(o.Paths.SystemdDir, phpfpm.UnitName("cg-site-*")))
	bin := o.fpmBinary(version) + " "
	var out []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			if rest, ok := strings.CutPrefix(line, "ExecStart="); ok && strings.HasPrefix(rest, bin) {
				out = append(out, filepath.Base(f))
			}
		}
	}
	return out
}

// checkPHPExists refuses a version whose FPM binary is not on the machine.
func (o *Ops) checkPHPExists(version string) error {
	if !slices.Contains(o.PHPVersions(), version) {
		return agentrpc.Invalid("PHP %s is not installed on this server", version)
	}
	return nil
}
