// Package syspkg is the one list of the Ubuntu packages a CloudGround
// release needs. The installer installs them (internal/install SPEC §3,
// packages.base and packages.main), and the agent checks them each time it
// starts (internal/agent/ops/panel SPEC §2.6), so a self-update brings in a
// package its release added (#113) and the two lists cannot drift apart.
package syspkg

import "slices"

// Base comes first in an install: what adding the PPA and the downloads
// need.
var Base = []string{"software-properties-common", "curl", "gnupg", "ca-certificates", "bzip2"}

// system is the data plane's packages that do not depend on PHP's version.
// redis-server is no longer in it (R-6.1): nothing ever turns Redis on.
// cron runs the sites' jobs (/etc/cron.d); minimal images ship without it.
// nftables is the block list's (internal/blocklist SPEC §5): ufw drives
// netfilter without it, and not every image has the nft command.
// LibBPF is what systemd loads to enforce the sandbox's SocketBindDeny
// (internal/phpfpm SPEC §4.1); iproute2 brings it on most images, a
// minimal one may lack it, and systemd then ignores the line.
var system = []string{"nginx", "mariadb-server", "certbot", "python3-certbot-dns-cloudflare", "git", "unzip", "cron", "nftables", LibBPF}

// LibBPF is the package of libbpf.so.1.
const LibBPF = "libbpf1"

// NeedsReexec reports whether installing these packages calls for
// `systemctl daemon-reexec`: systemd tries to load libbpf once, and a PID 1
// that started without it keeps ignoring SocketBind* until it is executed
// again.
func NeedsReexec(installed []string) bool { return slices.Contains(installed, LibBPF) }

// phpExtensions are the PHP packages installed for a PHP version, as
// php<version>-<name>. php-redis is no longer among them (R-6.1): the object
// cache is APCu only.
var phpExtensions = []string{"fpm", "mysql", "curl", "gd", "intl", "mbstring", "soap", "xml", "zip", "imagick", "bcmath", "apcu"}

// Main is the data plane for PHP version php: System, then php's packages.
func Main(php string) []string {
	pkgs := System()
	for _, ext := range phpExtensions {
		pkgs = append(pkgs, "php"+php+"-"+ext)
	}
	return pkgs
}

// System is the data plane's packages that do not depend on PHP's version.
func System() []string { return slices.Clone(system) }

// Release is what every server of this release has whatever its PHP
// versions: Base, then System. The agent installs the missing ones before
// it starts. The PHP packages are left out: a server's PHP versions are
// its own (the installer's default one, and those the panel added).
func Release() []string { return append(slices.Clone(Base), system...) }
