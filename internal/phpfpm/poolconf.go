package phpfpm

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/cloudground-it/cloudground/internal/state"
)

// defaultBudgetMB sizes a pool rendered without a budget (SPEC §5).
const defaultBudgetMB = 1024

// reloadDrainCapSec caps how long a reload waits for running requests
// (process_control_timeout, SPEC §5.2): the site answers no uncached request
// while it waits.
const reloadDrainCapSec = 30

// limits are a pool's per-request limits (SPEC §3.3).
type limits struct {
	memoryMB, uploadMB, execSec, terminateSec int
}

func limitsFor(site state.Site) limits {
	l := limits{memoryMB: 256, uploadMB: 64, execSec: 120}
	if site.Type == state.SiteWooCommerce {
		l.memoryMB = 512
	}
	php := site.Config.PHP
	if php.MemoryLimitMB > 0 {
		l.memoryMB = php.MemoryLimitMB
	}
	if php.UploadMaxMB > 0 {
		l.uploadMB = php.UploadMaxMB
	}
	if php.MaxExecutionSeconds > 0 {
		l.execSec = php.MaxExecutionSeconds
	}
	l.terminateSec = php.RequestTimeoutSeconds()
	return l
}

// onDemand: only a Balanced site that is not a store keeps no idle workers
// (SPEC §3.4).
func onDemand(site state.Site) bool {
	return site.Profile == state.ProfileBalanced && site.Type != state.SiteWooCommerce
}

// twoFactor is the connector's two-factor mode for a WordPress site, ""
// for none (SPEC §5.3).
func twoFactor(site state.Site) string {
	if site.Type != state.SiteWordPress && site.Type != state.SiteWooCommerce {
		return ""
	}
	switch m := site.Config.WPTwoFactor; m {
	case state.TwoFactorOptional, state.TwoFactorRequired:
		return m
	}
	return ""
}

// loginPathRe is the sign-in path the connector takes; anything else is
// left out of the pool (internal/site validates it before).
var loginPathRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,63}$`)

// hardening is what the connector does of a WordPress site's hardening:
// its items, comma-separated, and the sign-in path (SPEC §5.3). The
// readme item is nginx's alone.
func hardening(site state.Site) (items, login string) {
	if site.Type != state.SiteWordPress && site.Type != state.SiteWooCommerce {
		return "", ""
	}
	h := site.Config.WPHardening
	var on []string
	for _, it := range []struct {
		on   bool
		name string
	}{{h.XMLRPCOff, "xmlrpc"}, {h.FileEditOff, "file_edit"}, {h.HideVersion, "version"}} {
		if it.on {
			on = append(on, it.name)
		}
	}
	if loginPathRe.MatchString(h.LoginPath) {
		login = h.LoginPath
	}
	return strings.Join(on, ","), login
}

// SendmailPath is PHP's sendmail_path on a site that sends mail: the
// panel's mail gate client, run as the site's user (api SPEC §14.2).
const SendmailPath = "/usr/local/bin/panel-agent sendmail -t -i"

// mailFromRe is a sender the pool may carry: an address with no space,
// quote, semicolon or control character, which the pool file could not
// hold as one value.
var mailFromRe = regexp.MustCompile(`^[A-Za-z0-9._%+=-]{1,64}@[A-Za-z0-9.-]{1,253}$`)

// mailOut says whether site's mail() goes to the mail gate, and its sender
// for the connector (SPEC §5.3): never on a staging copy, which keeps
// sending nothing, nor on a site whose mail is off.
func mailOut(site state.Site) (sends bool, from string) {
	if site.StagingOf != 0 || site.Config.Mail.Off {
		return false, ""
	}
	if mailFromRe.MatchString(site.Config.Mail.From) {
		from = site.Config.Mail.From
	}
	return true, from
}

// RenderPool renders site's master configuration, its pool sized against
// budgetMB (SPEC §5), and the path it belongs at.
func RenderPool(site state.Site, budgetMB int) (path, content string, err error) {
	if site.SystemUser == "" || site.PHPVersion == "" {
		return "", "", fmt.Errorf("site %q missing system user or php version", site.Domain)
	}
	if budgetMB <= 0 {
		budgetMB = defaultBudgetMB
	}
	u := site.SystemUser
	w := SizeWorkers(budgetMB, site.Config.Resources.PHPWorkers)
	l := limitsFor(site)
	tmp := filepath.Join(site.RootPath, "tmp")

	var f lines
	f.add("; %s", header)
	f.add("; Site: %s (id %d). This master runs as the site user", site.Domain, site.ID)
	f.add("; under %s; it has no other pools.", UnitName(u))
	f.add("")

	f.add("[global]")
	f.add("pid = %s", filepath.Join(runDir(u), "fpm.pid"))
	f.add("error_log = syslog")
	f.add("syslog.ident = php-fpm-%s", u)
	f.add("daemonize = no")
	f.add("; Ten workers crashing within one minute make the master restart itself.")
	f.add("emergency_restart_threshold = 10")
	f.add("emergency_restart_interval = 1m")
	f.add("process_control_timeout = %ds", min(l.terminateSec, reloadDrainCapSec))
	f.add("")

	f.add("[%s]", u)
	f.add("listen = %s", SocketFor(u))
	f.add("listen.mode = 0660")
	if onDemand(site) {
		f.addAll(
			"; Workers start on the first request and stop when idle, so a quiet site",
			"; holds no memory; kept spare by \"dynamic\" on many small sites, idle",
			"; workers would take hundreds of MB.",
		)
		f.add("pm = ondemand")
		f.add("pm.max_children = %d", w.Max)
		f.add("pm.process_idle_timeout = 30s")
	} else {
		f.add("pm = dynamic")
		f.add("pm.max_children = %d", w.Max)
		f.add("pm.start_servers = %d", w.StartServe)
		f.add("pm.min_spare_servers = %d", w.MinSpare)
		f.add("pm.max_spare_servers = %d", w.MaxSpare)
	}
	f.add("pm.max_requests = %d", w.MaxRequests)
	f.add("pm.status_path = /__cloudground/fpm-status")
	f.add("")

	f.addAll(
		"; Connections waiting on the socket. With the default of 511 a burst",
		"; overflows the queue and nginx answers 502; a long queue makes the burst",
		"; wait instead. max_children still bounds the work done at once.",
	)
	f.add("listen.backlog = 65535")
	f.add("")
	f.addAll(
		"; Open files per worker. A plugin-heavy WordPress with persistent outbound",
		"; connections can pass 1024 and then fails at random places.",
	)
	f.add("rlimit_files = 65536")
	f.add("")
	f.add("request_terminate_timeout = %ds", l.terminateSec)
	f.add("catch_workers_output = yes")
	f.add("")

	f.addAll(
		"; The unit's private mount namespace, where no other site is visible,",
		"; isolates the site (open_basedir is not used). Uploads, temporary files",
		"; and sessions stay in the site's tmp folder.",
	)
	admin := func(kind, key, value string) { f.add("php_admin_%s[%s] = %s", kind, key, value) }
	admin("value", "upload_tmp_dir", tmp)
	admin("value", "sys_temp_dir", tmp)
	admin("value", "session.save_path", tmp+"/sessions")
	admin("value", "error_log", errorLog(site.Domain))
	admin("flag", "log_errors", "on")
	admin("flag", "display_errors", "off")
	admin("value", "disable_functions", "exec,passthru,shell_exec,system,proc_open,popen")
	f.add("")

	f.add("; Per-request limits")
	admin("value", "memory_limit", fmt.Sprintf("%dM", l.memoryMB))
	admin("value", "upload_max_filesize", fmt.Sprintf("%dM", l.uploadMB))
	admin("value", "post_max_size", fmt.Sprintf("%dM", l.uploadMB))
	admin("value", "max_execution_time", fmt.Sprint(l.execSec))
	f.add("")

	if mode := twoFactor(site); mode != "" {
		f.addAll(
			"; Two-factor sign-in, read by the connector (connector/SPEC.md 2.11).",
			"; Here rather than in wp-config.php, which the site can edit.",
		)
		f.add("env[CLOUDGROUND_2FA] = %s", mode)
		f.add("")
	}
	if items, login := hardening(site); items != "" || login != "" {
		f.add("; WordPress hardening, the connector's half (connector/SPEC.md 2.12).")
		if items != "" {
			f.add("env[CLOUDGROUND_HARDENING] = %s", items)
		}
		if login != "" {
			f.add("env[CLOUDGROUND_LOGIN_PATH] = %s", login)
		}
		f.add("")
	}
	if sends, from := mailOut(site); sends {
		f.addAll(
			"; mail() hands the message to the panel's mail gate, which knows the",
			"; site by its user and sends it through the relay (api SPEC 14.2).",
		)
		admin("value", "sendmail_path", SendmailPath)
		if from != "" {
			f.add("env[CLOUDGROUND_MAIL_FROM] = %s", from)
		}
		f.add("")
	}

	f.addAll(
		"; Object cache in APCu, shared memory inside PHP itself: no separate",
		"; service and no socket on a single server. The master's command line",
		"; sets its size.",
	)
	admin("flag", "apc.enabled", "on")
	admin("flag", "apc.enable_cli", "off")
	f.add("")

	f.addAll(
		"; Compiled scripts are cached. The cache and JIT buffer sizes apply to the",
		"; whole master and are given on its command line.",
	)
	admin("flag", "opcache.enable", "on")
	admin("value", "opcache.validate_timestamps", "1")
	admin("value", "opcache.revalidate_freq", "60")
	f.addAll(
		"; The tracing JIT is turned on on the master's command line: PHP reads",
		"; the JIT mode only when the master starts.",
	)
	admin("flag", "opcache.enable_cli", "off")
	f.add("")

	f.addAll(
		"; PHP compresses its own output: the page cache stores the compressed body",
		"; and serves hits without compressing again. Pages that are not cached are",
		"; compressed here as well, instead of by nginx, so there is a single path.",
	)
	admin("flag", "zlib.output_compression", "on")
	admin("value", "zlib.output_compression_level", "5")
	return confPath(u), f.String(), nil
}
