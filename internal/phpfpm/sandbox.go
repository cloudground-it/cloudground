package phpfpm

import (
	"path/filepath"

	"github.com/cloudground-it/cloudground/internal/state"
)

// Sandbox is the confinement every long-lived process of a site runs in, as
// systemd unit-file lines: the PHP master's unit (RenderUnit) and the site's
// SSH shell (ops/sftp SPEC §5.4, passed to systemd-run as -p) both take it
// from here, so the two cannot drift apart.
//
// The site sees its own tree and nothing of any other site: /srv/sites is
// an empty tmpfs with only this site bound into it, and the same for its
// logs. This, not open_basedir, is the tenant boundary. Other tenants'
// processes are invisible (ProtectProc), /tmp and /dev are private, setuid
// binaries gain nothing (NoNewPrivileges, no capabilities), and no
// namespace can be made.
//
// PrivateDevices binds the host's /dev/shm into the private /dev, where
// every site could leave files for every other: each unit mounts a tmpfs of
// its own over it instead, capped at ShmSize. PHP does not need it (APCu
// and OPcache map anonymous memory, sessions live in the site's tmp), but
// glibc's POSIX semaphores and shm_open do, so it stays writable.
// RunHardening is what a site's runs outside the request path (its SSH
// shell, its cron jobs, wp-cli) add to Sandbox: no setuid or setgid file can
// be made, an IPC namespace of their own, the system-service syscall set
// (anything else fails with EPERM; Sandbox already locks the personality). The PHP
// master takes only MasterHardening: it is the request path, and each of
// the others needs an A/B benchmark there first (SPEC §4.1). RemoveIPC is left out: it
// removes every IPC object of the uid when the unit stops, and the uid is
// also the site's PHP-FPM's; with PrivateIPC and PrivateDevices a run's IPC
// is its own anyway.
var RunHardening = []string{
	"RestrictSUIDSGID=yes",
	"PrivateIPC=yes",
	"SystemCallFilter=@system-service",
	"SystemCallErrorNumber=EPERM",
}

// MasterHardening is what the PHP master adds to Sandbox: an IPC namespace
// of its own, measured at no cost on the request path (SPEC §4.1). APCu and
// OPcache map anonymous memory, which the namespace does not touch.
var MasterHardening = []string{
	"PrivateIPC=yes",
}

// RunSandbox is a site's run's confinement: Sandbox plus RunHardening.
func RunSandbox(site state.Site) []string {
	return append(Sandbox(site), RunHardening...)
}

// ShmSize caps a site's own /dev/shm (Sandbox): the memory one site can pin
// there, where the host's is half of RAM.
const ShmSize = "64M"

func Sandbox(site state.Site) []string {
	logDir := filepath.Join(LogRoot, site.Domain)
	return []string{
		"ProtectSystem=strict",
		"TemporaryFileSystem=/srv/sites:ro /var/log/cloudground:ro",
		"TemporaryFileSystem=/dev/shm:size=" + ShmSize + ",mode=1777,nosuid,nodev,noexec",
		"BindPaths=" + site.RootPath + " " + logDir,
		"ReadWritePaths=" + site.RootPath + " " + logDir,
		"InaccessiblePaths=-/etc/nginx -/etc/cloudground -/etc/letsencrypt -/etc/mysql -/var/lib/cloudground -/var/lib/cloudground-agent -/var/cache/cloudground -/var/log/nginx -/var/backups",
		"ProtectHome=yes",
		"PrivateTmp=yes",
		"PrivateDevices=yes",
		"ProtectProc=invisible",
		"ProcSubset=pid",
		"NoNewPrivileges=yes",
		"ProtectKernelTunables=yes",
		"ProtectKernelModules=yes",
		"ProtectKernelLogs=yes",
		"ProtectControlGroups=yes",
		"ProtectClock=yes",
		"RestrictNamespaces=yes",
		"RestrictRealtime=yes",
		"LockPersonality=yes",
		"RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6",
		"CapabilityBoundingSet=",
	}
}
