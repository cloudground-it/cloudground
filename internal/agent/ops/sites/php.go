package sites

import (
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
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
