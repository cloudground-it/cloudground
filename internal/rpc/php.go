package rpc

import (
	"fmt"
	"slices"

	"github.com/cloudground-it/cloudground/internal/site"
)

// PHPExtension is one of the extensions the panel offers to install: its
// Debian package is php<version>-<Name>, and Modules are the names `php -m`
// prints for it (lowercase), any of which shows it is loaded.
type PHPExtension struct {
	Name    string
	Modules []string
}

// PHPExtensions are the common extensions the panel installs (#29): the
// ones the installer puts in (internal/install MainPackages) and the few
// sites most often ask for besides. Nothing outside this list is ever
// installed: the name chooses a package that apt installs as root.
var PHPExtensions = []PHPExtension{
	{"apcu", []string{"apcu"}},
	{"bcmath", []string{"bcmath"}},
	{"curl", []string{"curl"}},
	{"gd", []string{"gd"}},
	{"gmp", []string{"gmp"}},
	{"igbinary", []string{"igbinary"}},
	{"imagick", []string{"imagick"}},
	{"intl", []string{"intl"}},
	{"ldap", []string{"ldap"}},
	{"mbstring", []string{"mbstring"}},
	{"memcached", []string{"memcached"}},
	{"mysql", []string{"mysqli", "pdo_mysql"}},
	{"pgsql", []string{"pgsql", "pdo_pgsql"}},
	{"redis", []string{"redis"}},
	{"soap", []string{"soap"}},
	{"sqlite3", []string{"sqlite3", "pdo_sqlite"}},
	{"xml", []string{"xml", "dom", "simplexml"}},
	{"zip", []string{"zip"}},
}

// KnownPHPExtension reports whether name is in PHPExtensions.
func KnownPHPExtension(name string) bool {
	return slices.ContainsFunc(PHPExtensions, func(e PHPExtension) bool { return e.Name == name })
}

// PHPExtensionsParams names the PHP version whose extensions to list.
type PHPExtensionsParams struct {
	Version string `json:"version"`
}

// Validate requires a PHP version a site may run.
func (p *PHPExtensionsParams) Validate() error {
	if !site.ValidPHPVersion(p.Version) {
		return fmt.Errorf("unsupported php version %q", p.Version)
	}
	return nil
}

// PHPExtensionState is one of the common extensions and whether the
// version loads it.
type PHPExtensionState struct {
	Name      string `json:"name"`
	Installed bool   `json:"installed"`
}

// PHPExtensionsResult is what a PHP version loads: every module `php-fpm -m`
// prints (lowercase, sorted), and the common extensions with whether each
// is among them.
type PHPExtensionsResult struct {
	Version    string              `json:"version"`
	Modules    []string            `json:"modules"`
	Extensions []PHPExtensionState `json:"extensions"`
}

// PHPExtensionInstallParams installs one common extension for a PHP
// version; the agent then restarts the masters that run that version, so
// they load it.
type PHPExtensionInstallParams struct {
	Version   string `json:"version"`
	Extension string `json:"extension"`
}

// Validate requires a PHP version a site may run and an extension of the
// list.
func (p *PHPExtensionInstallParams) Validate() error {
	if !site.ValidPHPVersion(p.Version) {
		return fmt.Errorf("unsupported php version %q", p.Version)
	}
	if !KnownPHPExtension(p.Extension) {
		return fmt.Errorf("unknown extension %q: the panel installs only the common ones", p.Extension)
	}
	return nil
}
