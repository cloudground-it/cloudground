// Package phpfpm renders a site's own PHP-FPM master, the systemd unit and
// the one-pool configuration it runs, and sizes them for the machine
// (SPEC.md). It only produces text; ops/sites writes, tests and loads it.
package phpfpm

import "path/filepath"

// LogRoot holds one root-owned log directory per domain; the PHP error log
// goes there rather than into the site's tree (SPEC §2).
var LogRoot = "/var/log/cloudground"

// MasterConfDir holds one master configuration per site user.
var MasterConfDir = "/etc/cloudground-php"

// runDir is the unit's runtime directory: socket and pid file.
func runDir(user string) string { return filepath.Join("/run/cloudground/php", user) }

// UnitName is the systemd unit of user's master.
func UnitName(user string) string { return "cloudground-php-" + user + ".service" }

// SocketFor is the socket user's master listens on.
func SocketFor(user string) string { return filepath.Join(runDir(user), "php.sock") }

// confPath is the master configuration file of user.
func confPath(user string) string { return filepath.Join(MasterConfDir, user+".conf") }

// errorLog is the PHP error log of domain.
func errorLog(domain string) string { return filepath.Join(LogRoot, domain, "php-error.log") }
