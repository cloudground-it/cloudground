package install

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/cloudground-it/cloudground/internal/install/managed"
)

// `cgctl uninstall` (SPEC §9): a list of actions, each idempotent, built
// from what is on the machine. The default tier takes CloudGround away and
// keeps the sites' data, their databases and the backups; --remove-sites
// takes the sites too; --purge-packages the packages the installer
// installed. Remote backup repositories are never touched.

// UninstallOptions are the tiers of one run.
type UninstallOptions struct {
	RemoveSites   bool
	PurgePackages bool
	// KeptDir receives what the operator needs afterwards: a copy of the
	// panel's state and the backup credentials (SPEC §9.3).
	KeptDir string
}

// DefaultKeptDir is where an uninstall leaves what it keeps for the operator.
const DefaultKeptDir = "/root/cloudground-uninstall"

// Action is one thing an uninstall does.
type Action struct {
	ID   string
	What string
	Do   func(ctx context.Context) error
	// Fatal stops the run when it fails: nothing after it may run without
	// it (the state is copied out before it is removed).
	Fatal bool
	// Follow-up actions (a reload, a stop) mean something only beside a
	// removal: a machine with nothing but them has nothing to uninstall.
	Follow bool
}

// SiteRef is a site found on the machine: its id, domain and user. A site
// known only from its user, or only from its directory, has the other
// fields empty.
type SiteRef struct {
	ID     int64
	Domain string
	User   string
}

// Inventory is what an uninstall found on the machine.
type Inventory struct {
	Sites      []SiteRef
	ShellUsers []string
	// Databases and DBUsers are the sites' (the agent's registry, and the
	// names the panel gives); Scratch are the agent's own temporary ones.
	Databases, DBUsers           []string
	ScratchDBs, ScratchUsers     []string
	MariaDB                      bool // the server answered
	Record                       InstallRecord
	Lineages                     []string // certbot lineages of the sites
	PanelLineage                 bool
	SiteVhosts, SiteConfs, Units []string
	// SliceUnits are the sites' slice units, with their limits (#19).
	SliceUnits []string
	// The functions (#143): their users cg-fn-<id>, their socket and
	// service units (v0.8.x site functions' too) and their lineages.
	FnUsers, FnUnits, FnLineages []string
}

var (
	siteUserRe    = regexp.MustCompile(`^cg-site-([1-9][0-9]*)$`)
	fnUserRe      = regexp.MustCompile(`^cg-fn-[1-9][0-9]*$`)
	fnLineageRe   = regexp.MustCompile(`^cloudground-(functions|fn-[1-9][0-9]*)$`)
	shellUserRe   = regexp.MustCompile(`^cg-shell-[1-9][0-9]*$`)
	dbNameRe      = regexp.MustCompile(`^[a-z][a-z0-9_]{2,47}$`)
	siteDBRe      = regexp.MustCompile(`^site_[1-9][0-9]*$`)
	scratchDBRe   = regexp.MustCompile(`^cg[vmh]_[0-9a-f]{16}$`)
	scratchUserRe = regexp.MustCompile(`^cgq_[0-9a-f]{16}$`)
	domainRe      = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)
)

// The agent's files the inventory reads.
const (
	siteIndexFile  = "/var/lib/cloudground-agent/sites.json"
	dbRegistryFile = "/var/lib/cloudground-agent/databases.json"
	sitesRoot      = "/srv/sites"
	functionsRoot  = "/srv/functions"
	unitDir        = "/etc/systemd/system"
	mariadbTuning  = "/etc/mysql/mariadb.conf.d/99-cloudground.cnf"
	nginxPre       = managed.NginxConf + ".cloudground-pre"
	acmeWebroot    = "/var/www/cloudground-acme"
)

// TakeInventory looks at the machine and changes nothing.
func (e *Env) TakeInventory(ctx context.Context) Inventory {
	var inv Inventory
	inv.Record, _ = e.ReadInstallRecord()
	byID := map[int64]*SiteRef{}
	site := func(id int64) *SiteRef {
		if s, ok := byID[id]; ok {
			return s
		}
		s := &SiteRef{ID: id, User: "cg-site-" + strconv.FormatInt(id, 10)}
		byID[id] = s
		return s
	}
	var index struct {
		Sites map[string]string `json:"sites"`
	}
	if b, err := e.readFile(siteIndexFile); err == nil && json.Unmarshal(b, &index) == nil {
		for k, dom := range index.Sites {
			if id, err := strconv.ParseInt(k, 10, 64); err == nil && id > 0 && domainRe.MatchString(dom) {
				site(id).Domain = dom
			}
		}
	}
	for _, u := range e.passwdUsers() {
		if m := siteUserRe.FindStringSubmatch(u); m != nil {
			id, _ := strconv.ParseInt(m[1], 10, 64)
			site(id)
		} else if _, _, ok := e.Acct.User(u); ok && shellUserRe.MatchString(u) {
			inv.ShellUsers = append(inv.ShellUsers, u)
		} else if ok && fnUserRe.MatchString(u) {
			inv.FnUsers = append(inv.FnUsers, u)
		}
	}
	known := map[string]bool{}
	for _, s := range byID {
		known[s.Domain] = true
	}
	var orphans []SiteRef
	if entries, err := os.ReadDir(e.path(sitesRoot)); err == nil {
		for _, d := range entries {
			if d.IsDir() && !known[d.Name()] && domainRe.MatchString(d.Name()) {
				orphans = append(orphans, SiteRef{Domain: d.Name()})
			}
		}
	}
	for _, s := range byID {
		inv.Sites = append(inv.Sites, *s)
	}
	sort.Slice(inv.Sites, func(i, j int) bool { return inv.Sites[i].ID < inv.Sites[j].ID })
	inv.Sites = append(inv.Sites, orphans...)

	var reg struct {
		Databases map[string]int64 `json:"databases"`
		Users     map[string]any   `json:"users"`
	}
	if b, err := e.readFile(dbRegistryFile); err == nil && json.Unmarshal(b, &reg) == nil {
		for n := range reg.Databases {
			inv.Databases = append(inv.Databases, n)
		}
		for n := range reg.Users {
			inv.DBUsers = append(inv.DBUsers, n)
		}
	}
	if e.Cmd.LookPath("mariadb") {
		dbs, err1 := e.Cmd.Run(ctx, "mariadb", "-N", "-B", "-e", "SELECT schema_name FROM information_schema.schemata")
		users, err2 := e.Cmd.Run(ctx, "mariadb", "-N", "-B", "-e", "SELECT user FROM mysql.user WHERE host = 'localhost'")
		if err1 == nil && err2 == nil {
			inv.MariaDB = true
			for _, n := range strings.Fields(dbs) {
				switch {
				case siteDBRe.MatchString(n):
					inv.Databases = append(inv.Databases, n)
				case scratchDBRe.MatchString(n):
					inv.ScratchDBs = append(inv.ScratchDBs, n)
				}
			}
			for _, n := range strings.Fields(users) {
				switch {
				case siteDBRe.MatchString(n):
					inv.DBUsers = append(inv.DBUsers, n)
				case scratchUserRe.MatchString(n):
					inv.ScratchUsers = append(inv.ScratchUsers, n)
				}
			}
		}
	}
	inv.Databases = uniqueValid(inv.Databases)
	inv.DBUsers = uniqueValid(inv.DBUsers)

	for _, s := range inv.Sites {
		if s.Domain != "" && e.exists("/etc/letsencrypt/renewal/"+s.Domain+".conf") {
			inv.Lineages = append(inv.Lineages, s.Domain)
		}
	}
	inv.PanelLineage = e.exists("/etc/letsencrypt/renewal/cloudground-panel.conf")
	for _, r := range e.glob("/etc/letsencrypt/renewal/cloudground-f*.conf") {
		if n := strings.TrimSuffix(filepath.Base(r), ".conf"); fnLineageRe.MatchString(n) {
			inv.FnLineages = append(inv.FnLineages, n)
		}
	}
	inv.FnUnits = append(e.glob(unitDir+"/cloudground-fn-*.socket"), e.glob(unitDir+"/cloudground-fn-*@.service")...)
	inv.SiteVhosts = e.cloudgroundVhosts()
	inv.SiteConfs = e.glob("/etc/nginx/conf.d/cloudground-*.conf")
	inv.Units = e.glob(unitDir + "/cloudground-php-*.service")
	for _, u := range e.glob(unitDir + "/cg-site-*.slice") {
		if siteUserRe.MatchString(strings.TrimSuffix(filepath.Base(u), ".slice")) {
			inv.SliceUnits = append(inv.SliceUnits, u)
		}
	}
	return inv
}

func uniqueValid(names []string) []string {
	var out []string
	for _, n := range names {
		if dbNameRe.MatchString(n) && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// passwdUsers are the user names of /etc/passwd.
func (e *Env) passwdUsers() []string {
	b, err := e.readFile("/etc/passwd")
	if err != nil {
		return nil
	}
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if name, _, ok := strings.Cut(sc.Text(), ":"); ok && name != "" {
			out = append(out, name)
		}
	}
	return out
}

// glob answers system paths (not Root-prefixed) matching pattern.
func (e *Env) glob(pattern string) []string {
	matches, _ := filepath.Glob(e.path(pattern))
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, e.unpath(m))
	}
	sort.Strings(out)
	return out
}

// unpath is path's inverse.
func (e *Env) unpath(full string) string {
	if e.Root == "" || e.Root == "/" {
		return full
	}
	rel, err := filepath.Rel(e.Root, full)
	if err != nil {
		return full
	}
	return "/" + rel
}

// cloudgroundVhosts are the files in sites-enabled CloudGround wrote: the
// panel's, and every vhost that starts with its marker.
func (e *Env) cloudgroundVhosts() []string {
	var out []string
	for _, p := range e.glob("/etc/nginx/sites-enabled/*") {
		if p == managed.Vhost {
			continue // the installer's: removed with the managed files
		}
		if strings.HasPrefix(filepath.Base(p), "cloudground-") {
			out = append(out, p)
			continue
		}
		b, err := e.readFile(p)
		if err == nil && bytes.Contains(head(b, 512), []byte("Managed by CloudGround")) {
			out = append(out, p)
		}
	}
	return out
}

func head(b []byte, n int) []byte {
	if len(b) > n {
		return b[:n]
	}
	return b
}

// UninstallActions is the uninstall, in order (SPEC §9.2). noSystemd
// leaves out what needs systemd running (a container).
func UninstallActions(e *Env, inv Inventory, o UninstallOptions) []Action {
	if o.KeptDir == "" {
		o.KeptDir = DefaultKeptDir
	}
	noSystemd := e.Sd == nil || !e.Sd.Running()
	var a []Action
	add := func(id, what string, do func(ctx context.Context) error) {
		a = append(a, Action{ID: id, What: what, Do: do})
	}
	follow := func(id, what string, do func(ctx context.Context) error) {
		a = append(a, Action{ID: id, What: what, Do: do, Follow: true})
	}
	rm := func(id string, paths ...string) {
		var present []string
		for _, p := range paths {
			if e.exists(p) {
				present = append(present, p)
			}
		}
		if len(present) == 0 {
			return
		}
		add(id, "remove "+strings.Join(present, " "), func(context.Context) error { return e.removeAll(present...) })
	}
	sd := func(args ...string) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			if err := e.Sd.Do(ctx, args...); err != nil {
				e.logf("warning: %v", err)
			}
			return nil
		}
	}

	// 1. The panel stops, its state goes to the kept directory first.
	if !noSystemd {
		follow("services.stop", "stop and disable cloudground-update-guard, cloudground-api.socket, cloudground-api, cloudground-agent",
			func(ctx context.Context) error {
				_ = e.Sd.Do(ctx, "stop", "cloudground-update-guard.service")
				for _, u := range []string{"cloudground-api.socket", "cloudground-api.service", "cloudground-agent.service"} {
					if err := e.Sd.Do(ctx, "disable", "--now", u); err != nil {
						e.logf("warning: %v", err)
					}
				}
				return nil
			})
	}
	if e.exists("/var/lib/cloudground/state.db") || e.exists("/var/lib/cloudground/secret.key") {
		a = append(a, Action{ID: "keep.state", Fatal: true,
			What: "copy the panel's state and key to " + o.KeptDir + "/state, the backup credentials to " + o.KeptDir + "/backup.env",
			Do:   func(context.Context) error { return e.keepPanelState(o.KeptDir) }})
	}

	// 2. The sites' processes and wiring: PHP masters, cron, shells, vhosts.
	if !noSystemd {
		follow("sites.stop", "stop cg-site.slice (every site's PHP, cron and shell processes)", sd("stop", "cg-site.slice"))
		for _, u := range inv.Units {
			add("site.unit", "stop and disable "+filepath.Base(u), sd("disable", "--now", filepath.Base(u)))
		}
	}
	for _, u := range inv.Units {
		rm("site.unit-files", u, u+".d")
	}
	rm("site.slice-units", inv.SliceUnits...)
	// The functions' processes and units (#143): their slice holds every
	// invocation, their sockets would start new ones.
	if !noSystemd {
		follow("functions.stop", "stop cg-fn.slice (every function's processes)", sd("stop", "cg-fn.slice"))
		for _, u := range inv.FnUnits {
			if strings.HasSuffix(u, ".socket") {
				add("fn.unit", "stop and disable "+filepath.Base(u), sd("disable", "--now", filepath.Base(u)))
			}
		}
	}
	rm("fn.unit-files", append(append([]string{}, inv.FnUnits...), unitDir+"/cg-fn.slice")...)
	rm("nginx.site-files", append(append(append([]string{}, inv.SiteVhosts...), inv.SiteConfs...), "/etc/nginx/cloudground-auth")...)
	rm("cron.files", append(e.glob("/etc/cron.d/cloudground-*"), e.glob("/var/spool/cron/crontabs/cg-site-*")...)...)
	for _, u := range inv.ShellUsers {
		add("user.shell", "remove the shell login "+u, e.delUser(u))
	}
	for _, s := range inv.Sites {
		if s.User == "" {
			continue
		}
		if uid, _, ok := e.Acct.User(s.User); ok {
			mask := fmt.Sprintf("%s/user@%d.service", unitDir, uid)
			if target, err := os.Readlink(e.path(mask)); err == nil && target == "/dev/null" {
				rm("site.shell-mask", mask)
			}
		}
	}
	if e.Acct != nil {
		if _, ok := e.Acct.Group("cg-shell"); ok {
			add("group.cg-shell", "remove the group cg-shell", e.cmdWarn("groupdel", "cg-shell"))
		}
	}

	// 3. The sites themselves (--remove-sites), while MariaDB and certbot run.
	if o.RemoveSites {
		for _, s := range inv.Sites {
			if s.User == "" {
				continue
			}
			if _, _, ok := e.Acct.User(s.User); ok {
				add("site.user", "kill the processes of "+s.User+", remove the user and its group", e.delSiteUser(s.User))
			}
		}
		if inv.MariaDB {
			for _, u := range append(append([]string{}, inv.DBUsers...), inv.ScratchUsers...) {
				add("site.db-user", "drop the database account "+u+"@localhost", e.sql("DROP USER IF EXISTS '"+u+"'@'localhost'"))
			}
			for _, d := range append(append([]string{}, inv.Databases...), inv.ScratchDBs...) {
				add("site.database", "drop the database "+d, e.sql("DROP DATABASE IF EXISTS `"+d+"`"))
			}
		} else if len(inv.Databases)+len(inv.DBUsers) > 0 {
			add("site.databases", "MariaDB does not answer: the sites' databases cannot be dropped", func(context.Context) error {
				return fmt.Errorf("MariaDB does not answer: databases %s and accounts %s were not dropped; start MariaDB and run the uninstall again",
					strings.Join(inv.Databases, " "), strings.Join(inv.DBUsers, " "))
			})
		}
		for _, l := range inv.Lineages {
			add("site.certificate", "delete the certificate "+l+" (certbot delete)", e.cmdWarn("certbot", "delete", "--cert-name", l, "--non-interactive"))
		}
		rm("site.trees", sitesRoot)
		rm("site.tuning", mariadbTuning)
		// The functions' users and code go with the sites (#143).
		for _, u := range inv.FnUsers {
			add("fn.user", "kill the processes of "+u+", remove the user and its group", e.delSiteUser(u))
		}
		rm("fn.trees", functionsRoot)
	} else {
		for _, u := range inv.FnUsers {
			// The function's code stays its own, as a site's files do.
			follow("fn.lock", "lock the function user "+u+" (its code stays its own)",
				e.cmdWarn("usermod", "--lock", "--expiredate", "1", "--shell", "/usr/sbin/nologin", u))
		}
		for _, s := range inv.Sites {
			if s.User == "" {
				continue
			}
			if _, _, ok := e.Acct.User(s.User); ok {
				// Its files stay owned by it; its logins end with the
				// panel that guarded them (no chroot, no gate).
				follow("site.lock", "lock the site user "+s.User+" (its files stay its own)",
					e.cmdWarn("usermod", "--lock", "--expiredate", "1", "--shell", "/usr/sbin/nologin", s.User))
			}
		}
		if e.exists("/etc/cloudground/certs/custom") {
			add("keep.custom-certs", "move the certificates brought for the sites to "+o.KeptDir+"/custom-certs", func(context.Context) error {
				return e.moveInto("/etc/cloudground/certs/custom", filepath.Join(o.KeptDir, "custom-certs"))
			})
		}
	}
	for _, l := range inv.FnLineages {
		add("cert.functions", "delete the functions' certificate "+l+" (certbot delete)", e.cmdWarn("certbot", "delete", "--cert-name", l, "--non-interactive"))
	}
	if inv.PanelLineage {
		add("cert.panel", "delete the panel's certificate cloudground-panel (certbot delete)",
			e.cmdWarn("certbot", "delete", "--cert-name", "cloudground-panel", "--non-interactive"))
	}

	// 4. The installer's files, the agent's global ones.
	rm("ssh.config", managed.SSHD, "/etc/ssh/sshd_config.d/11-cloudground-shell.conf", "/etc/ssh/sshd_config.d/09-cloudground-sftp-keys.conf")
	if e.Cmd.LookPath("sshd") {
		follow("ssh.reload", "sshd -t, then reload ssh", func(ctx context.Context) error {
			if out, err := e.Cmd.Run(ctx, "sshd", "-t"); err != nil {
				return fmt.Errorf("sshd -t: %v: %s", err, strings.TrimSpace(out))
			}
			if !noSystemd {
				if err := e.Sd.Do(ctx, "reload", "ssh"); err != nil {
					_ = e.Sd.Do(ctx, "reload", "sshd")
				}
			}
			return nil
		})
	}
	rm("sysctl", managed.Sysctl)
	if e.Cmd.LookPath("nft") {
		// The block list's table (internal/blocklist SPEC §5): with the
		// agent gone nothing would keep it current, and its blocks would
		// stay until a reboot.
		follow("netfilter", "delete the block list's netfilter table (inet cloudground)", func(ctx context.Context) error {
			if _, err := e.Cmd.Run(ctx, "nft", "list", "table", "inet", "cloudground"); err != nil {
				return nil // there is none
			}
			if out, err := e.Cmd.Run(ctx, "nft", "delete", "table", "inet", "cloudground"); err != nil {
				return fmt.Errorf("nft delete table inet cloudground: %v: %s", err, strings.TrimSpace(out))
			}
			return nil
		})
	}
	rm("logrotate", managed.Logrotate)
	rm("needrestart", managed.Needrestart)
	rm("certbot.hook", managed.RenewHook, "/etc/letsencrypt/cloudground-cloudflare.ini")
	rm("nginx.bootstrap", managed.Vhost)
	if e.exists(nginxPre) || e.nginxTuned() {
		add("nginx.conf", "take the worker tuning out of "+managed.NginxConf, func(context.Context) error { return e.untuneNginx() })
	}
	if t := inv.Record.NginxDefaultSite; t != "" && !e.exists("/etc/nginx/sites-enabled/default") && e.existsFollow(t) {
		add("nginx.default-site", "put back the distribution's default site (sites-enabled/default -> "+t+")", func(context.Context) error {
			return os.Symlink(t, e.path("/etc/nginx/sites-enabled/default"))
		})
	}
	if e.Cmd.LookPath("nginx") {
		follow("nginx.reload", "nginx -t, then reload nginx", func(ctx context.Context) error {
			if out, err := e.Cmd.Run(ctx, "nginx", "-t"); err != nil {
				return fmt.Errorf("nginx -t after removing CloudGround's files: %v: %s", err, strings.TrimSpace(out))
			}
			if !noSystemd {
				if on, _ := e.Sd.Active(ctx, "nginx"); on {
					_ = e.Sd.Do(ctx, "reload", "nginx")
				}
			}
			return nil
		})
		// It served only CloudGround's sites, which are gone: left on, its stock
		// site would hold port 80 and a reinstall would refuse (#212).
		if !noSystemd {
			follow("nginx.stop", "stop and disable nginx (it served only CloudGround's sites; systemctl enable --now nginx starts it again)", sd("disable", "--now", "nginx"))
		}
	}
	var unitFiles []string
	for _, u := range []string{managed.AgentUnit, managed.APIUnit, managed.SocketUnit} {
		unitFiles = append(unitFiles, u, u+".d")
	}
	unitFiles = append(unitFiles, e.wantsLinks()...)
	rm("units", unitFiles...)
	if !noSystemd {
		follow("systemd.reload", "systemctl daemon-reload", sd("daemon-reload"))
	}
	var bins []string
	for _, b := range append(append(append([]string{}, Binaries...), OptionalBinaries...), "cg-site-cron") {
		p := filepath.Join(BinDir, b)
		bins = append(bins, p, p+".bak", filepath.Join(BinDir, "."+b+".cg-update"))
	}
	rm("binaries", bins...)

	// 5. The state and the directories.
	rm("state", "/etc/cloudground", "/etc/cloudground-php", "/etc/cloudground-fn", "/var/lib/cloudground", "/var/lib/cloudground-agent",
		"/var/lib/cloudground-signatures", "/var/lib/cloudground-runtimes", "/etc/cloudground-app", "/etc/clamav/cloudground-freshclam.conf", "/var/cache/cloudground", "/var/log/cloudground", "/run/cloudground", acmeWebroot,
		"/var/log/cloudground-install.log", "/tmp/cloudground-install.log")
	if e.Acct != nil {
		if _, _, ok := e.Acct.User("cloudground"); ok {
			add("user.cloudground", "remove the user cloudground", e.delUser("cloudground"))
		}
		if _, ok := e.Acct.Group("cloudground"); ok {
			add("group.cloudground", "remove the group cloudground", e.cmdWarn("groupdel", "cloudground"))
		}
		for _, g := range []string{"cg-sftp-off", "cg-sftp-keys"} {
			if _, ok := e.Acct.Group(g); ok {
				add("group."+g, "remove the group "+g, e.cmdWarn("groupdel", g))
			}
		}
	}

	if len(inv.Record.Dirs) > 0 {
		dirs := append([]string{}, inv.Record.Dirs...)
		var present []string
		for _, d := range dirs {
			if e.exists(d) {
				present = append(present, d)
			}
		}
		if len(present) > 0 {
			follow("dirs.created", "remove "+strings.Join(present, " ")+" where empty (the installer created them)", func(context.Context) error {
				for _, d := range present {
					e.removeEmptyTree(d)
				}
				return nil
			})
		}
	}
	if !o.RemoveSites && e.exists(sitesRoot) {
		follow("site.root", "remove "+sitesRoot+" if no site is left in it", func(context.Context) error {
			_ = os.Remove(e.path(sitesRoot)) // only an empty directory goes
			return nil
		})
	}
	if !o.RemoveSites && e.exists(functionsRoot) {
		follow("fn.root", "remove "+functionsRoot+" if no function is left in it", func(context.Context) error {
			_ = os.Remove(e.path(functionsRoot)) // only an empty directory goes
			return nil
		})
	}

	// 6. The packages the installer installed (--purge-packages).
	if o.PurgePackages {
		a = append(a, e.purgeActions(inv, o)...)
	}
	if !slices.ContainsFunc(a, func(x Action) bool { return !x.Follow }) {
		return nil // nothing of CloudGround's is left here
	}
	return a
}

// delSiteUser ends a user's processes and removes it and its group: a
// site's or a function's (--remove-sites).
func (e *Env) delSiteUser(u string) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		_, _ = e.Cmd.Run(ctx, "pkill", "-KILL", "-u", u)
		if out, err := e.Cmd.Run(ctx, "userdel", "-f", u); err != nil && !strings.Contains(out, "does not exist") {
			return fmt.Errorf("userdel %s: %v: %s", u, err, strings.TrimSpace(out))
		}
		if _, ok := e.Acct.Group(u); ok {
			_, _ = e.Cmd.Run(ctx, "groupdel", u)
		}
		return nil
	}
}

// PrintUninstallPlan writes one line per action.
func PrintUninstallPlan(w io.Writer, actions []Action) {
	for _, act := range actions {
		fmt.Fprintf(w, "- %-20s %s\n", act.ID, act.What)
	}
	fmt.Fprintf(w, "%d action(s)\n", len(actions))
}

// RunUninstall runs every action, in order. A failure is reported and the
// run goes on (each action is idempotent, so a second run finishes the
// job), except a Fatal one, which stops it.
func RunUninstall(ctx context.Context, e *Env, actions []Action, w io.Writer) error {
	var errs []error
	for _, act := range actions {
		fmt.Fprintf(w, "  %s\n", act.What)
		e.logf("uninstall: %s: %s", act.ID, act.What)
		if err := act.Do(ctx); err != nil {
			err = fmt.Errorf("%s: %w", act.ID, err)
			fmt.Fprintf(w, "    failed: %v\n", err)
			e.logf("uninstall: %v", err)
			errs = append(errs, err)
			if act.Fatal {
				return fmt.Errorf("stopped before removing anything else: %w", errors.Join(errs...))
			}
		}
		if ctx.Err() != nil {
			return errors.Join(append(errs, ctx.Err())...)
		}
	}
	return errors.Join(errs...)
}

// purgeActions are --purge-packages: the recorded packages of the purgeable
// kinds, the recorded PPA and tools. MariaDB is kept while it holds a
// database CloudGround did not make or did not remove.
func (e *Env) purgeActions(inv Inventory, o UninstallOptions) []Action {
	var a []Action
	var pkgs []string
	keepDB := ""
	for _, p := range inv.Record.Packages {
		if !purgeable(p) {
			continue
		}
		if strings.HasPrefix(p, "mariadb-") {
			if !o.RemoveSites {
				keepDB = "the sites' databases are kept (no --remove-sites)"
				continue
			}
		}
		pkgs = append(pkgs, p)
	}
	if keepDB != "" {
		a = append(a, Action{ID: "packages.keep", Follow: true, What: "keep mariadb-server: " + keepDB, Do: func(context.Context) error { return nil }})
	}
	if len(pkgs) > 0 {
		a = append(a, Action{ID: "packages.purge", What: "apt-get purge " + strings.Join(pkgs, " ") + ", then the dependencies only they needed (apt's autoremove rule; held packages stay)",
			Do: func(ctx context.Context) error {
				if slices.ContainsFunc(pkgs, func(p string) bool { return strings.HasPrefix(p, "mariadb-") }) {
					if others := e.otherDatabases(ctx); len(others) > 0 {
						pkgs = slices.DeleteFunc(pkgs, func(p string) bool { return strings.HasPrefix(p, "mariadb-") })
						e.logf("keeping mariadb-server: it holds %s", strings.Join(others, " "))
					}
				}
				if len(pkgs) == 0 {
					return nil
				}
				// What was orphaned before is the operator's; what the purge
				// orphans (nginx-common, mariadb-server-core, php8.5-common…)
				// came with CloudGround and goes with it.
				before := e.orphans(ctx)
				profiles := e.apparmorProfiles(ctx, pkgs)
				// Even when a purge fails part way, what it took is unloaded.
				defer e.unloadProfiles(&profiles)
				args := append([]string{"purge", "-y", "-qq"}, pkgs...)
				if out, err := e.Cmd.Run(ctx, "apt-get", args...); err != nil {
					return fmt.Errorf("apt-get purge: %v: %s", err, strings.TrimSpace(out))
				}
				if before != nil {
					// A held package is the operator's decision: it stays.
					held, _ := e.Cmd.Run(ctx, "apt-mark", "showhold")
					var deps []string
					for _, p := range e.orphans(ctx) {
						if !slices.Contains(before, p) && !slices.Contains(strings.Fields(held), p) {
							deps = append(deps, p)
						}
					}
					if len(deps) > 0 {
						profiles = append(profiles, e.apparmorProfiles(ctx, deps)...)
						args := append([]string{"purge", "-y", "-qq"}, deps...)
						if out, err := e.Cmd.Run(ctx, "apt-get", args...); err != nil {
							return fmt.Errorf("apt-get purge (dependencies): %v: %s", err, strings.TrimSpace(out))
						}
					}
				}
				if slices.Contains(pkgs, "mariadb-server") {
					// mariadb-server's postinst writes debian.cnf and its purge
					// leaves it: the only file in /etc/mysql, it goes with it.
					if e.onlyFile("/etc/mysql", "debian.cnf") {
						if err := e.removeAll("/etc/mysql"); err != nil {
							return err
						}
					}
				}
				if slices.Contains(pkgs, "certbot") {
					// The installer made it for certbot's sandbox; empty, it is ours.
					_ = os.Remove(e.path("/var/lib/letsencrypt")) // only an empty directory goes
				}
				return nil
			}})
	}
	for _, ppa := range inv.Record.PPAs {
		ppa := ppa
		a = append(a, Action{ID: "packages.ppa", What: "remove ppa:" + ppa,
			Do: func(ctx context.Context) error {
				if out, err := e.Cmd.Run(ctx, "add-apt-repository", "--remove", "-y", "ppa:"+ppa); err != nil {
					return fmt.Errorf("add-apt-repository --remove: %v: %s", err, strings.TrimSpace(out))
				}
				return nil
			}})
	}
	var tools []string
	for _, t := range inv.Record.Tools {
		if filepath.Dir(t) == BinDir && e.exists(t) {
			tools = append(tools, t)
		}
	}
	if len(tools) > 0 {
		a = append(a, Action{ID: "packages.tools", What: "remove " + strings.Join(tools, " "),
			Do: func(context.Context) error { return e.removeAll(tools...) }})
	}
	if len(a) == 0 {
		a = append(a, Action{ID: "packages.none", Follow: true, What: "no package was recorded as installed by CloudGround: nothing to purge",
			Do: func(context.Context) error { return nil }})
	}
	return a
}

// purgeable: the data plane the installer brings, never the base tools
// (curl, gnupg, ca-certificates…) nor git, unzip or cron, which the system
// may have come to rely on.
func purgeable(p string) bool {
	switch {
	case p == "nginx", p == "certbot", p == "python3-certbot-dns-cloudflare", p == "mariadb-server":
		return true
	case strings.HasPrefix(p, "php") && strings.Contains(p, "-"):
		return true
	}
	return false
}

// apparmorFS is where the kernel lists the loaded AppArmor profiles.
const apparmorFS = "/sys/kernel/security/apparmor"

var (
	apparmorNamed    = regexp.MustCompile(`(?m)^\s*profile\s+(?:"([^"]+)"|([^\s"{]+))`)
	apparmorAttached = regexp.MustCompile(`(?m)^\s*(?:"(/[^"]+)"|(/[^\s"{]+))\s+(?:flags=\([^)]*\)\s*)?\{`)
)

// profileNames are the profile names the AppArmor policy text defines.
func profileNames(text string) []string {
	var names []string
	for _, re := range []*regexp.Regexp{apparmorNamed, apparmorAttached} {
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			names = append(names, m[1]+m[2])
		}
	}
	return names
}

// apparmorProfiles are the profiles the packages' files in /etc/apparmor.d
// define. A purge deletes the files but leaves the profiles loaded until a
// reboot: Ubuntu's "mariadbd" then stops another MariaDB's mariadbd from
// setting up its data directory (CAP_DAC_OVERRIDE and setgid denied).
func (e *Env) apparmorProfiles(ctx context.Context, pkgs []string) []string {
	out, _ := e.Cmd.Run(ctx, "dpkg-query", append([]string{"-L"}, pkgs...)...)
	var names []string
	for _, f := range strings.Split(out, "\n") {
		f = strings.TrimSpace(f)
		if filepath.Dir(f) != "/etc/apparmor.d" {
			continue
		}
		if b, err := os.ReadFile(e.path(f)); err == nil {
			names = append(names, profileNames(string(b))...)
		}
	}
	return names
}

// definedProfiles are the names the files left in /etc/apparmor.d define.
func (e *Env) definedProfiles() map[string]bool {
	defined := map[string]bool{}
	entries, _ := os.ReadDir(e.path("/etc/apparmor.d"))
	for _, d := range entries {
		if !d.Type().IsRegular() {
			continue
		}
		if b, err := os.ReadFile(e.path("/etc/apparmor.d/" + d.Name())); err == nil {
			for _, n := range profileNames(string(b)) {
				defined[n] = true
			}
		}
	}
	return defined
}

// unloadProfiles takes out of the kernel the profiles still loaded that no
// file defines any more: a purge that failed, or another package's file
// with the same name, keeps its profile.
func (e *Env) unloadProfiles(names *[]string) {
	loaded, err := os.ReadFile(e.path(apparmorFS + "/profiles"))
	if err != nil {
		return
	}
	defined := e.definedProfiles()
	for _, n := range *names {
		if defined[n] || !strings.Contains("\n"+string(loaded), "\n"+n+" (") {
			continue
		}
		if err := os.WriteFile(e.path(apparmorFS+"/.remove"), []byte(n), 0o600); err != nil {
			e.logf("AppArmor profile %s stays loaded until a reboot: %v", n, err)
		}
	}
}

// onlyFile: dir holds the file name and nothing else but empty directories.
func (e *Env) onlyFile(dir, name string) bool {
	root := e.path(dir)
	found, other := false, false
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir():
			return nil
		case p == filepath.Join(root, name) && d.Type().IsRegular():
			found = true
			return nil
		}
		other = true
		return fs.SkipAll
	})
	return err == nil && found && !other
}

// orphans are the packages apt-get would autoremove now, nil when apt-get
// cannot say (then no dependency is purged).
func (e *Env) orphans(ctx context.Context) []string {
	out, err := e.Cmd.Run(ctx, "apt-get", "-s", "autoremove")
	if err != nil {
		return nil
	}
	orphans := []string{}
	for _, l := range strings.Split(out, "\n") {
		// Purg when the operator's apt configuration purges by default.
		if f := strings.Fields(l); len(f) >= 2 && (f[0] == "Remv" || f[0] == "Purg") {
			orphans = append(orphans, f[1])
		}
	}
	return orphans
}

// otherDatabases are the databases MariaDB still holds beyond its own.
func (e *Env) otherDatabases(ctx context.Context) []string {
	out, err := e.Cmd.Run(ctx, "mariadb", "-N", "-B", "-e", "SELECT schema_name FROM information_schema.schemata")
	if err != nil {
		return []string{"(MariaDB did not answer)"}
	}
	var others []string
	for _, n := range strings.Fields(out) {
		switch n {
		case "mysql", "information_schema", "performance_schema", "sys":
			continue
		}
		others = append(others, n)
	}
	return others
}

// sql runs one statement as MariaDB's root over its socket. Every name in
// it was matched against dbNameRe or the scratch patterns.
func (e *Env) sql(stmt string) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		if out, err := e.Cmd.Run(ctx, "mariadb", "-e", stmt); err != nil {
			return fmt.Errorf("%s: %v: %s", stmt, err, strings.TrimSpace(out))
		}
		return nil
	}
}

// cmdWarn runs a command whose failure is a warning in the log.
func (e *Env) cmdWarn(name string, args ...string) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		if out, err := e.Cmd.Run(ctx, name, args...); err != nil {
			e.logf("warning: %s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(out))
		}
		return nil
	}
}

// delUser removes a user; one already gone is no error.
func (e *Env) delUser(name string) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		if _, _, ok := e.Acct.User(name); !ok {
			return nil
		}
		if out, err := e.Cmd.Run(ctx, "userdel", "-f", name); err != nil && !strings.Contains(out, "does not exist") {
			return fmt.Errorf("userdel %s: %v: %s", name, err, strings.TrimSpace(out))
		}
		return nil
	}
}

// removeAll removes each path and everything under it, never following a
// symlink (os.RemoveAll removes the link itself).
func (e *Env) removeAll(paths ...string) error {
	var errs []error
	for _, p := range paths {
		if p == "" || p == "/" || !filepath.IsAbs(p) {
			errs = append(errs, fmt.Errorf("refusing to remove %q", p))
			continue
		}
		if err := os.RemoveAll(e.path(p)); err != nil {
			errs = append(errs, err)
		} else {
			e.logf("removed %s", p)
		}
	}
	return errors.Join(errs...)
}

// moveInto moves the directory from into dir (created 0700).
func (e *Env) moveInto(from, dir string) error {
	if !e.exists(from) {
		return nil
	}
	if err := os.MkdirAll(e.path(filepath.Dir(dir)), 0o700); err != nil {
		return err
	}
	if e.exists(dir) {
		dir += ".1"
	}
	if err := os.Rename(e.path(from), e.path(dir)); err != nil {
		return err
	}
	return os.Chmod(e.path(dir), 0o700)
}

// removeEmptyTree removes p and the directories under it, bottom up, as far
// as they are empty: a file anyone put there keeps its directories.
func (e *Env) removeEmptyTree(p string) {
	full := e.path(p)
	var dirs []string
	_ = filepath.WalkDir(full, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Remove(dirs[i]) // fails, harmlessly, on a directory with something in it
	}
}

// existsFollow reports whether p, followed, exists.
func (e *Env) existsFollow(p string) bool {
	_, err := os.Stat(e.path(p))
	return err == nil
}

// wantsLinks are the links `systemctl enable` made for the panel's units
// (left behind when systemd is not running to disable them).
func (e *Env) wantsLinks() []string {
	var out []string
	for _, p := range e.glob(unitDir + "/*.wants/*") {
		if strings.HasPrefix(filepath.Base(p), "cloudground-") {
			out = append(out, p)
		}
	}
	return out
}

func (e *Env) nginxTuned() bool {
	b, err := e.readFile(managed.NginxConf)
	return err == nil && bytes.Contains(b, []byte(TunedMarker))
}

var (
	tunedLines = regexp.MustCompile(`(?m)^# ` + TunedMarker + `\n(worker_rlimit_nofile [0-9]+;\n)?`)
	// The installer's own value only: a later edit of the operator's stays.
	workerConn = regexp.MustCompile(`(?m)^([[:space:]]*)worker_connections[[:space:]]+` + strconv.Itoa(WorkerConnections) + `;`)
	origConn   = regexp.MustCompile(`(?m)^[[:space:]]*worker_connections[[:space:]]+([0-9]+);`)
)

// untuneNginx takes the installer's edit out of nginx.conf (SPEC §9.2):
// when the file is still exactly what the edit made of the saved original,
// the original comes back byte for byte; otherwise (the operator edited it
// since) only the edit's own lines are taken out and worker_connections
// gets the original's value back, everything else kept. nginx -t is run by
// the next action; the saved original goes once it has been used.
func (e *Env) untuneNginx() error {
	cur, err := e.readFile(managed.NginxConf)
	if err != nil {
		return err
	}
	fi, err := e.stat(managed.NginxConf)
	if err != nil {
		return err
	}
	uid, gid, err := e.FS.Owner(e.path(managed.NginxConf))
	if err != nil {
		return err
	}
	pre, preErr := e.readFile(nginxPre)
	var next []byte
	switch {
	case preErr == nil && bytes.Equal(TuneNginxConf(pre), cur):
		next = pre
	case bytes.Contains(cur, []byte(TunedMarker)):
		next = tunedLines.ReplaceAll(cur, nil)
		orig := "768" // Ubuntu's own
		if preErr == nil {
			if m := origConn.FindSubmatch(pre); m != nil {
				orig = string(m[1])
			}
		}
		next = workerConn.ReplaceAll(next, []byte("${1}worker_connections "+orig+";"))
	default:
		next = cur
	}
	if !bytes.Equal(next, cur) {
		if err := e.replaceKeeping(managed.NginxConf, next, fi, uid, gid); err != nil {
			return err
		}
	}
	if preErr == nil {
		return e.removeAll(nginxPre)
	}
	if !errors.Is(preErr, fs.ErrNotExist) {
		return preErr
	}
	return nil
}

// StoppedNginx says whether the uninstall stopped nginx (it is installed and
// systemd runs), for the closing summary (SPEC §9.2).
func (e *Env) StoppedNginx() bool {
	return e.Cmd.LookPath("nginx") && e.Sd != nil && e.Sd.Running()
}
