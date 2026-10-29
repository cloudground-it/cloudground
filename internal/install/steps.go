package install

import (
	"bytes"
	"compress/bzip2"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudground-it/cloudground/internal/install/managed"
	"github.com/cloudground-it/cloudground/internal/syspkg"
)

// Step is one idempotent piece of the install (SPEC §1).
type Step struct {
	ID    string
	Group string // the progress line it is shown under
	// Check looks and changes nothing: needed, and why (or why not).
	Check func(ctx context.Context) (needed bool, why string)
	// Apply makes the machine match; only called when Check said needed.
	Apply func(ctx context.Context) error

	// planned tells the later checks of a --plan run that this step
	// would have been applied.
	planned func()
}

// Options are the choices of one run.
type Options struct {
	OS      OS
	Upgrade bool
	// From is a local build (unverified); Release a downloaded release,
	// verified before the steps run (VerifyRelease). At most one is set.
	From, Release string
	SkipPackages  bool
	// Container: no systemd, kernel or firewall work (SPEC §1).
	Container bool
	// Doctor leaves out the binaries step (there is nothing to compare to).
	Doctor bool
}

// The pinned tools (SPEC §3 steps 6–8).
const (
	ResticVersion   = "0.19.1"
	ResticURL       = "https://github.com/restic/restic/releases/download/v0.19.1/restic_0.19.1_linux_amd64.bz2"
	ResticSHA256    = "f415415624dcc452f2a02b8c33641791a8c6d6d3b65bbb3543fcf9a25151585c"
	WPCLIVersion    = "2.12.0"
	WPCLIURL        = "https://github.com/wp-cli/wp-cli/releases/download/v2.12.0/wp-cli-2.12.0.phar"
	WPCLISHA256     = "ce34ddd838f7351d6759068d09793f26755463b4a4610a5a5c0a97b68220d85c"
	ComposerVersion = "2.10.3"
	ComposerURL     = "https://getcomposer.org/download/2.10.3/composer.phar"
	ComposerSHA256  = "7a2d379d5b8ffdaa028580ef26494c36d2feef4b178d3dd1473a4dbc5e17c8d6"
)

// Pin is a download fixed by version and hash.
type Pin struct{ Version, URL, SHA256 string }

// The pins the steps use (tests point them at fixtures).
var (
	pinRestic   = Pin{ResticVersion, ResticURL, ResticSHA256}
	pinWPCLI    = Pin{WPCLIVersion, WPCLIURL, WPCLISHA256}
	pinComposer = Pin{ComposerVersion, ComposerURL, ComposerSHA256}
)

// BasePackages come first: what adding the PPA and the downloads need. The
// lists live in internal/syspkg, which the agent reads too (#113).
var BasePackages = syspkg.Base

// MainPackages is the data plane for PHP version php (syspkg.Main).
func MainPackages(php string) []string { return syspkg.Main(php) }

// The progress groups, in order: the old installer's ten lines.
const (
	gPackages = "Installing nginx, PHP, MariaDB, restic"
	gUsers    = "Creating users, directories and secrets"
	gBinaries = "Installing CloudGround binaries"
	gKernel   = "Tuning kernel and nginx workers"
	gNginx    = "Configuring nginx"
	gMariaDB  = "Tuning MariaDB"
	gServices = "Starting services"
	gFirewall = "Opening the firewall"
	gLogs     = "Log rotation and certificate renewal"
)

// directories is SPEC §3 step 10.
var directories = []struct {
	path string
	w    want
}{
	{"/srv/sites", want{mode: 0o755}},
	// The functions' code (internal/functions SPEC §2): traversed, never
	// listed, by the functions' users.
	{"/srv/functions", want{mode: 0o711, user: "root", group: "root"}},
	{"/var/log/cloudground", want{mode: 0o755}},
	{"/var/cache/cloudground", want{mode: 0o755}},
	{"/var/lib/cloudground", want{mode: 0o750, user: "cloudground", group: "cloudground"}},
	{"/var/www/cloudground-acme", want{mode: 0o755}},
	{"/run/cloudground", want{mode: 0o755}},
	{"/run/cloudground/php", want{mode: 0o755}},
	// Each site's PHP master reads its pool here as the site's user; the
	// agent's UMask=0027 would make it 0750 (the agent sets 0755 too).
	{"/etc/cloudground-php", want{mode: 0o755, user: "root", group: "root"}},
	{"/var/lib/cloudground-agent", want{mode: 0o700, user: "root", group: "root"}},
	{"/var/lib/cloudground-agent/work", want{mode: 0o700, user: "root", group: "root"}},
	{"/etc/cloudground", want{mode: 0o750, group: "cloudground"}},
	{"/etc/cloudground/certs", want{mode: 0o750, group: "cloudground"}},
	// Named by the agent's unit (ReadWritePaths): systemd refuses to start
	// a unit whose path is missing, and these exist only once certbot has
	// run and once cron is installed. The modes are the ones certbot and
	// cron's package give them.
	{"/var/lib/letsencrypt", want{mode: 0o755}},
	{"/var/spool/cron", want{mode: 0o755}},
}

// The secrets (SPEC §3 steps 12–15).
const (
	AgentToken = "/etc/cloudground/agent.token"
	certDir    = "/etc/cloudground/certs"
)

type certSpec struct {
	id, name, cn      string
	pemW, keyW        want
	keyGroupIfPresent string // the key's group, set only when it exists
}

var certs = []certSpec{
	{id: "cert.panel", name: "panel", cn: "cloudground-panel",
		pemW: want{mode: 0o600, user: "root", group: "cloudground"}, keyW: want{mode: 0o640, user: "root", group: "cloudground"}},
	{id: "cert.panel-ip", name: "panel-ip", cn: "cloudground-panel",
		pemW: want{mode: 0o600, user: "root", group: "root"}, keyW: want{mode: 0o640, user: "root", group: "root"}},
	{id: "cert.fallback", name: "fallback", cn: "fallback.invalid",
		pemW: want{mode: 0o644, user: "root", group: "root"}, keyW: want{mode: 0o640, user: "root", group: "root"}, keyGroupIfPresent: "www-data"},
}

// Firewall rules opened; firewallRulesFor puts the ports sshd listens on in
// place of 22/tcp.
var firewallRules = []string{"22/tcp", "80/tcp", "443/tcp", "8443/tcp", "443/udp"}

// Services enabled and started, in order.
var services = []string{"cloudground-api.socket", "cloudground-agent", "cloudground-api", "nginx"}

// run is the state one list of steps shares.
type run struct {
	e *Env
	o Options

	mu      sync.Mutex
	changed map[string]bool

	quicOnce sync.Once
	quic     bool
}

func (r *run) mark(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.changed[id] = true
}

func (r *run) wasChanged(ids ...string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range ids {
		if r.changed[id] {
			return true
		}
	}
	return false
}

// vars are the template inputs, probed once.
func (r *run) vars(ctx context.Context) Vars {
	r.quicOnce.Do(func() {
		out, err := r.e.Cmd.Run(ctx, "nginx", "-V")
		r.quic = err == nil && strings.Contains(out, "http_v3_module")
	})
	return Vars{QUIC: r.quic}
}

func ok(why string) (bool, string)   { return false, why }
func need(why string) (bool, string) { return true, why }

// skipped returns a check that reports why the step does nothing.
func skipped(why string) func(context.Context) (bool, string) {
	return func(context.Context) (bool, string) { return false, why }
}

const (
	whySkipPackages = "--skip-packages: skipped"
	whyNoSystemd    = "systemd is not running: skipped"
)

// Steps returns the install, in order (SPEC §3).
func Steps(e *Env, o Options) []Step {
	r := &run{e: e, o: o, changed: map[string]bool{}}
	php := o.OS.PHP
	noSystemd := o.Container || e.Sd == nil || !e.Sd.Running()

	var s []Step
	add := func(st Step) { s = append(s, st) }

	// ---- packages
	pkgStep := func(id string, pkgs []string) Step {
		return Step{ID: id, Group: gPackages,
			Check: func(ctx context.Context) (bool, string) {
				missing, err := e.Pkg.Missing(ctx, pkgs)
				if err != nil {
					return need("cannot ask the package manager: " + err.Error())
				}
				if len(missing) > 0 {
					return need("not installed: " + strings.Join(missing, " "))
				}
				return ok("installed")
			},
			Apply: func(ctx context.Context) error {
				missing, err := e.Pkg.Missing(ctx, pkgs)
				if err != nil {
					return err
				}
				if err := e.Pkg.Update(ctx); err != nil {
					return err
				}
				if err := e.Pkg.Install(ctx, missing); err != nil {
					return err
				}
				e.recordOrWarn(func(rec *InstallRecord) { rec.Packages = addOnce(rec.Packages, missing...) })
				return nil
			}}
	}
	if o.SkipPackages {
		add(Step{ID: "packages.base", Group: gPackages, Check: skipped(whySkipPackages)})
		if o.OS.PPA {
			add(Step{ID: "packages.ppa", Group: gPackages, Check: skipped(whySkipPackages)})
		}
		add(Step{ID: "packages.main", Group: gPackages, Check: skipped(whySkipPackages)})
	} else {
		add(pkgStep("packages.base", BasePackages))
		if o.OS.PPA {
			add(Step{ID: "packages.ppa", Group: gPackages,
				Check: func(context.Context) (bool, string) {
					matches, _ := filepath.Glob(e.path("/etc/apt/sources.list.d/*ondrej*php*"))
					if len(matches) == 0 {
						return need("ppa:ondrej/php is not configured")
					}
					return ok("ppa:ondrej/php configured")
				},
				Apply: func(ctx context.Context) error {
					// A failure here shows up as apt's own message for the
					// PHP packages, one step later (as before).
					if err := e.Pkg.AddPPA(ctx, "ondrej/php"); err != nil {
						e.logf("add-apt-repository: %v", err)
					} else {
						e.recordOrWarn(func(rec *InstallRecord) { rec.PPAs = addOnce(rec.PPAs, "ondrej/php") })
					}
					return e.Pkg.Update(ctx)
				}})
		}
		add(pkgStep("packages.main", MainPackages(php)))
	}

	unitOff := func(id, unit, why string) Step {
		if noSystemd {
			return Step{ID: id, Group: gPackages, Check: skipped(whyNoSystemd)}
		}
		return Step{ID: id, Group: gPackages,
			Check: func(ctx context.Context) (bool, string) {
				exists, err := e.Sd.Exists(ctx, unit)
				if err != nil {
					return need("cannot ask systemd: " + err.Error())
				}
				if !exists {
					return ok(unit + " is not installed")
				}
				en, err1 := e.Sd.Enabled(ctx, unit)
				ac, err2 := e.Sd.Active(ctx, unit)
				if err := errors.Join(err1, err2); err != nil {
					return need("cannot ask systemd: " + err.Error())
				}
				if en || ac {
					return need(unit + " is enabled or running; " + why)
				}
				return ok(unit + " is off")
			},
			Apply: func(ctx context.Context) error {
				// As before: a unit that will not stop is a warning, not a
				// reason to leave the server half installed.
				if err := e.Sd.Do(ctx, "disable", "--now", unit); err != nil {
					e.logf("warning: disable --now %s: %v", unit, err)
				}
				return nil
			}}
	}
	add(unitOff("php.distro-fpm-off", "php"+php+"-fpm.service", "every site runs its own FPM master"))
	add(unitOff("redis.off", "redis-server.service", "nothing uses Redis"))

	if o.SkipPackages {
		for _, id := range []string{"tools.restic", "tools.wp-cli", "tools.composer"} {
			add(Step{ID: id, Group: gPackages, Check: skipped(whySkipPackages)})
		}
	} else {
		add(Step{ID: "tools.restic", Group: gPackages,
			Check: func(ctx context.Context) (bool, string) {
				out, _ := e.Cmd.Run(ctx, filepath.Join(BinDir, "restic"), "version")
				if f := strings.Fields(out); len(f) >= 2 && f[1] == pinRestic.Version {
					return ok("restic " + pinRestic.Version)
				}
				return need("restic " + pinRestic.Version + " is not installed")
			},
			Apply: func(ctx context.Context) error {
				b, err := r.download(ctx, pinRestic.URL, pinRestic.SHA256, "restic")
				if err != nil {
					return err
				}
				bin, err := io.ReadAll(bzip2.NewReader(bytes.NewReader(b)))
				if err != nil {
					return fmt.Errorf("restic: %w", err)
				}
				return r.installTool(filepath.Join(BinDir, "restic"), bin)
			}})
		add(pinnedTool("tools.wp-cli", "wp", "wp-cli", pinWPCLI, r))
		add(pinnedTool("tools.composer", "composer", "composer", pinComposer, r))
	}

	// ---- users, directories, secrets
	add(Step{ID: "user.cloudground", Group: gUsers,
		Check: func(context.Context) (bool, string) {
			if _, _, found := e.Acct.User("cloudground"); found {
				return ok("user cloudground exists")
			}
			return need("no user cloudground")
		},
		Apply: func(ctx context.Context) error { return e.Acct.AddSystemUser(ctx, "cloudground") }})
	add(Step{ID: "dirs", Group: gUsers,
		Check: func(context.Context) (bool, string) {
			var diffs []string
			for _, d := range directories {
				if why := e.dirDiff(d.path, d.w); why != "" {
					diffs = append(diffs, why)
				}
			}
			if len(diffs) > 0 {
				return need(strings.Join(diffs, "; "))
			}
			return ok("in place")
		},
		Apply: func(context.Context) error {
			for _, d := range directories {
				if e.dirDiff(d.path, d.w) == "" {
					continue
				}
				if foreignDirs[d.path] {
					e.recordNewDir(d.path)
				}
				if err := e.ensureDir(d.path, d.w); err != nil {
					return err
				}
			}
			return nil
		}})
	envFile, _ := managed.Lookup(managed.PanelEnv)
	envW := want{mode: envFile.Mode, user: "root", group: envFile.Group}
	add(Step{ID: "panel.env", Group: gUsers,
		Check: func(context.Context) (bool, string) {
			cur, err := e.readFile(managed.PanelEnv)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return need(err.Error())
			}
			if errors.Is(err, fs.ErrNotExist) {
				return need(managed.PanelEnv + ": missing")
			}
			w := envW
			w.content = SetEnvLine(cur, "CLOUDGROUND_PHP_VERSION", php)
			if why := e.fileDiff(managed.PanelEnv, w); why != "" {
				return need(why)
			}
			return ok("CLOUDGROUND_PHP_VERSION=" + php)
		},
		Apply: func(context.Context) error {
			cur, err := e.readFile(managed.PanelEnv)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			return e.writeFile(managed.PanelEnv, SetEnvLine(cur, "CLOUDGROUND_PHP_VERSION", php), envW)
		}})
	// The token's content is never rewritten (the API and the agent share
	// it), but its mode and owner are put back on every run (H-3-2): an
	// older or hand-made file readable by anyone on the box would hand them
	// the agent.
	tokenW := want{mode: 0o640, user: "root", group: "cloudground"}
	add(Step{ID: "token.agent", Group: gUsers,
		Check: func(context.Context) (bool, string) {
			if !e.exists(AgentToken) {
				return need(AgentToken + ": missing")
			}
			if why := e.fileDiff(AgentToken, tokenW); why != "" {
				return need(why)
			}
			return ok("present (never rewritten), " + fmt.Sprintf("%04o", tokenW.mode) + " root:cloudground")
		},
		Apply: func(context.Context) error {
			if e.exists(AgentToken) {
				fi, err := e.stat(AgentToken)
				if err != nil {
					return err
				}
				if !fi.Mode().IsRegular() {
					return fmt.Errorf("%s is not a regular file: remove it, and run the installer again to make a new token", AgentToken)
				}
				if err := e.own(e.path(AgentToken), tokenW); err != nil {
					return err
				}
				return os.Chmod(e.path(AgentToken), tokenW.mode)
			}
			tok, err := newToken(48)
			if err != nil {
				return err
			}
			return e.writeFile(AgentToken, []byte(tok), tokenW)
		}})
	for _, c := range certs {
		pemPath := filepath.Join(certDir, c.name+".pem")
		keyPath := filepath.Join(certDir, c.name+".key")
		add(Step{ID: c.id, Group: gUsers,
			Check: func(context.Context) (bool, string) {
				if e.exists(pemPath) {
					return ok("present (never rewritten)")
				}
				return need(pemPath + ": missing")
			},
			Apply: func(context.Context) error {
				cert, key, err := selfSigned(c.cn, time.Now())
				if err != nil {
					return err
				}
				kw := c.keyW
				if c.keyGroupIfPresent != "" {
					if _, found := e.Acct.Group(c.keyGroupIfPresent); found {
						kw.group = c.keyGroupIfPresent
					}
				}
				if err := e.writeFile(keyPath, key, kw); err != nil {
					return err
				}
				return e.writeFile(pemPath, cert, c.pemW)
			}})
	}

	// ---- binaries
	switch {
	case o.Doctor:
	case o.From == "" && o.Release == "":
		add(Step{ID: "binaries", Group: gBinaries, Check: skipped("no build given (--from or --release): binaries left as they are")})
	default:
		src := o.From
		if src == "" {
			src = o.Release
		}
		add(Step{ID: "binaries", Group: gBinaries,
			Check: func(context.Context) (bool, string) {
				var diff []string
				for _, b := range binariesIn(src) {
					want, err := os.ReadFile(filepath.Join(src, b))
					if err != nil {
						return need(err.Error())
					}
					if e.fileSHA256(filepath.Join(BinDir, b)) != sha256Hex(want) {
						diff = append(diff, b)
					}
				}
				if len(diff) > 0 {
					return need("differs from " + src + ": " + strings.Join(diff, " "))
				}
				return ok("same as " + src)
			},
			Apply: func(context.Context) error {
				for _, b := range binariesIn(src) {
					data, err := os.ReadFile(filepath.Join(src, b))
					if err != nil {
						return err
					}
					dst := filepath.Join(BinDir, b)
					if err := e.writeFile(dst, data, want{mode: 0o755}); err != nil {
						return err
					}
					// What landed is what was verified.
					if e.fileSHA256(dst) != sha256Hex(data) {
						return fmt.Errorf("%s: the installed file does not match its source", dst)
					}
				}
				return nil
			}})
	}

	// ---- kernel and nginx
	add(r.sysctlStep())
	add(Step{ID: "nginx.tuning", Group: gKernel,
		Check: func(context.Context) (bool, string) {
			b, err := e.readFile(managed.NginxConf)
			if err != nil {
				return need(err.Error())
			}
			if !bytes.Contains(b, []byte(TunedMarker)) {
				return need(managed.NginxConf + ": not tuned")
			}
			return ok("tuned")
		},
		Apply: func(ctx context.Context) error { return r.tuneNginx(ctx) }})
	add(Step{ID: "nginx.default-site", Group: gNginx,
		Check: func(context.Context) (bool, string) {
			if e.exists("/etc/nginx/sites-enabled/default") {
				return need("the distribution's default site is enabled")
			}
			return ok("removed")
		},
		Apply: func(context.Context) error {
			if target, err := os.Readlink(e.path("/etc/nginx/sites-enabled/default")); err == nil {
				e.recordOrWarn(func(rec *InstallRecord) {
					if rec.NginxDefaultSite == "" {
						rec.NginxDefaultSite = target
					}
				})
			}
			err := os.Remove(e.path("/etc/nginx/sites-enabled/default"))
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}})
	add(r.renderedStep("nginx.vhost", gNginx, managed.Vhost, func(ctx context.Context) error {
		if out, err := e.Cmd.Run(ctx, "nginx", "-t"); err != nil {
			return fmt.Errorf("nginx -t: %v: %s", err, strings.TrimSpace(out))
		}
		return nil
	}))

	// ---- services
	if noSystemd {
		add(Step{ID: "mariadb", Group: gMariaDB, Check: skipped(whyNoSystemd)})
	} else {
		add(r.serviceStep("mariadb", gMariaDB, []string{"mariadb"}, nil))
	}
	add(r.unitsStep(noSystemd))
	if noSystemd {
		add(Step{ID: "services", Group: gServices, Check: skipped(whyNoSystemd)})
	} else {
		add(r.servicesStep())
	}

	// ---- firewall, SFTP, logs, renewal
	if o.Container {
		add(Step{ID: "firewall", Group: gFirewall, Check: skipped("container: skipped")})
	} else {
		add(r.firewallStep())
	}
	add(Step{ID: "sftp.group", Group: gFirewall,
		Check: func(context.Context) (bool, string) {
			if _, found := e.Acct.Group("cg-sftp-off"); found {
				return ok("group cg-sftp-off exists")
			}
			return need("no group cg-sftp-off")
		},
		Apply: func(ctx context.Context) error { return e.Acct.AddSystemGroup(ctx, "cg-sftp-off") }})
	add(r.renderedStep("sshd.sftp", gFirewall, managed.SSHD, func(ctx context.Context) error {
		if out, err := e.Cmd.Run(ctx, "sshd", "-t"); err != nil {
			return fmt.Errorf("sshd -t: %v: %s", err, strings.TrimSpace(out))
		}
		if noSystemd {
			return nil
		}
		if err := e.Sd.Do(ctx, "reload", "ssh"); err != nil {
			if err := e.Sd.Do(ctx, "reload", "sshd"); err != nil {
				e.logf("reload sshd: %v", err)
			}
		}
		return nil
	}))
	add(r.renderedStep("logrotate", gLogs, managed.Logrotate, func(ctx context.Context) error {
		// Found now rather than weeks later, with the disk full.
		if _, err := e.Cmd.Run(ctx, "logrotate", "--debug", managed.Logrotate); err != nil {
			e.logf("warning: logrotate rejected the CloudGround config — check %s", managed.Logrotate)
		}
		return nil
	}))
	// Harmless without needrestart: nothing reads the file. Without the
	// package its directory is missing too; the installer records it so
	// that an uninstall removes it again while it is empty.
	nrDir := filepath.Dir(managed.Needrestart)
	nrDirW := want{mode: 0o755}
	add(Step{ID: "needrestart.dir", Group: gLogs,
		Check: func(context.Context) (bool, string) {
			if why := e.dirDiff(nrDir, nrDirW); why != "" {
				return need(why)
			}
			return ok("in place")
		},
		Apply: func(context.Context) error {
			e.recordNewDir(nrDir)
			return e.ensureDir(nrDir, nrDirW)
		}})
	add(r.renderedStep("needrestart", gLogs, managed.Needrestart, nil))
	hookDir := filepath.Dir(managed.RenewHook)
	hookDirW := want{mode: 0o755}
	add(Step{ID: "certbot.hook-dir", Group: gLogs,
		Check: func(context.Context) (bool, string) {
			if why := e.dirDiff(hookDir, hookDirW); why != "" {
				return need(why)
			}
			return ok("in place")
		},
		Apply: func(context.Context) error {
			e.recordNewDir(hookDir)
			return e.ensureDir(hookDir, hookDirW)
		}})
	add(r.renderedStep("certbot.hook", gLogs, managed.RenewHook, nil))
	for i := range s {
		id := s[i].ID
		s[i].planned = func() { r.mark(id) }
	}
	return s
}

// pinnedTool installs a phar whose SHA-256 is fixed here.
func pinnedTool(id, name, label string, pin Pin, r *run) Step {
	p := filepath.Join(BinDir, name)
	label += " " + pin.Version
	url, sum := pin.URL, pin.SHA256
	return Step{ID: id, Group: gPackages,
		Check: func(context.Context) (bool, string) {
			if got := r.e.fileSHA256(p); got == sum {
				return ok(label)
			} else if got == "" {
				return need(p + ": missing")
			}
			return need(p + ": not the pinned " + label)
		},
		Apply: func(ctx context.Context) error {
			b, err := r.download(ctx, url, sum, name)
			if err != nil {
				return err
			}
			return r.installTool(p, b)
		}}
}

// installTool writes a pinned tool, recording it when no file was there
// before: only those go with --purge-packages (SPEC §9.1).
func (r *run) installTool(p string, b []byte) error {
	fresh := !r.e.exists(p)
	if err := r.e.writeFile(p, b, want{mode: 0o755}); err != nil {
		return err
	}
	if fresh {
		r.e.recordOrWarn(func(rec *InstallRecord) { rec.Tools = addOnce(rec.Tools, p) })
	}
	return nil
}

// download fetches url and refuses it unless it hashes to sum.
func (r *run) download(ctx context.Context, url, sum, name string) ([]byte, error) {
	b, err := r.e.Net.Fetch(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", name, err)
	}
	if sha256Hex(b) != sum {
		return nil, fmt.Errorf("%s checksum mismatch", name)
	}
	return b, nil
}

// renderedStep writes one managed file from its template; after runs once
// the file is written.
func (r *run) renderedStep(id, group, path string, after func(context.Context) error) Step {
	f, found := managed.Lookup(path)
	if !found {
		panic("install: " + path + " is not in internal/install/managed")
	}
	w := want{mode: f.Mode, user: orRootName(f.Owner), group: orRootName(f.Group)}
	return Step{ID: id, Group: group,
		Check: func(ctx context.Context) (bool, string) {
			content, err := Render(f, r.vars(ctx))
			if err != nil {
				return need(err.Error())
			}
			w := w
			w.content = content
			if why := r.e.fileDiff(path, w); why != "" {
				return need(why)
			}
			return ok("up to date")
		},
		Apply: func(ctx context.Context) error {
			content, err := Render(f, r.vars(ctx))
			if err != nil {
				return err
			}
			if err := r.e.writeFile(path, content, w); err != nil {
				return err
			}
			r.mark(id)
			if after != nil {
				return after(ctx)
			}
			return nil
		}}
}

func orRootName(s string) string {
	if s == "" {
		return "root"
	}
	return s
}

// tuneNginx is SPEC §3 step 18: back up, edit, test, restore on failure.
func (r *run) tuneNginx(ctx context.Context) error {
	e := r.e
	conf := managed.NginxConf
	orig, err := e.readFile(conf)
	if err != nil {
		return err
	}
	fi, err := e.stat(conf)
	if err != nil {
		return err
	}
	uid, gid, err := e.FS.Owner(e.path(conf))
	if err != nil {
		return err
	}
	backup := conf + ".cloudground-pre"
	if err := e.copyPreserving(conf, backup, orig, fi, uid, gid); err != nil {
		return err
	}
	if err := e.replaceKeeping(conf, TuneNginxConf(orig), fi, uid, gid); err != nil {
		return err
	}
	if _, err := e.Cmd.Run(ctx, "nginx", "-t"); err == nil {
		return nil
	}
	e.logf("nginx rejected the tuned config — restoring the original")
	if err := e.FS.Rename(e.path(backup), e.path(conf)); err != nil {
		return err
	}
	if _, err := e.Cmd.Run(ctx, "nginx", "-t"); err != nil {
		return fmt.Errorf("nginx config is broken and the backup did not restore it")
	}
	return nil
}

// copyPreserving writes data at dst with src's mode, owner and times (cp -a).
func (e *Env) copyPreserving(src, dst string, data []byte, fi fs.FileInfo, uid, gid int) error {
	if err := e.replaceKeeping(dst, data, fi, uid, gid); err != nil {
		return err
	}
	return os.Chtimes(e.path(dst), fi.ModTime(), fi.ModTime())
}

// replaceKeeping replaces p with data, keeping fi's mode and the owner.
func (e *Env) replaceKeeping(p string, data []byte, fi fs.FileInfo, uid, gid int) error {
	full := e.path(p)
	tmp, err := createTemp(full)
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := e.FS.Chown(tmp, uid, gid); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, fi.Mode().Perm()); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := e.FS.Rename(tmp, full); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	e.logf("wrote %s", p)
	return nil
}

// unitsStep writes the embedded units and reloads systemd.
func (r *run) unitsStep(noSystemd bool) Step {
	e := r.e
	return Step{ID: "units", Group: gServices,
		Check: func(context.Context) (bool, string) {
			var diff []string
			for _, f := range managed.Files {
				if !strings.HasPrefix(f.Template, "units/") {
					continue
				}
				content, err := Render(f, Vars{})
				if err != nil {
					return need(err.Error())
				}
				if why := e.fileDiff(f.Path, want{content: content, mode: f.Mode, user: "root", group: "root"}); why != "" {
					diff = append(diff, why)
				}
			}
			if len(diff) > 0 {
				return need(strings.Join(diff, "; "))
			}
			return ok("up to date")
		},
		Apply: func(ctx context.Context) error {
			for _, f := range managed.Files {
				if !strings.HasPrefix(f.Template, "units/") {
					continue
				}
				content, err := Render(f, Vars{})
				if err != nil {
					return err
				}
				if err := e.writeFile(f.Path, content, want{mode: f.Mode, user: "root", group: "root"}); err != nil {
					return err
				}
			}
			r.mark("units")
			if noSystemd {
				return nil
			}
			return e.Sd.Do(ctx, "daemon-reload")
		}}
}

// serviceStep enables and starts units; then, when set, runs after.
func (r *run) serviceStep(id, group string, units []string, after func(context.Context) error) Step {
	e := r.e
	return Step{ID: id, Group: group,
		Check: func(ctx context.Context) (bool, string) {
			var off []string
			for _, u := range units {
				en, err1 := e.Sd.Enabled(ctx, u)
				ac, err2 := e.Sd.Active(ctx, u)
				if err := errors.Join(err1, err2); err != nil {
					return need("cannot ask systemd: " + err.Error())
				}
				if !en || !ac {
					off = append(off, u)
				}
			}
			if len(off) > 0 {
				return need("not enabled and running: " + strings.Join(off, " "))
			}
			return ok("enabled and running")
		},
		Apply: func(ctx context.Context) error {
			if err := e.Sd.Do(ctx, append([]string{"enable", "--now"}, units...)...); err != nil {
				return err
			}
			if after != nil {
				return after(ctx)
			}
			return nil
		}}
}

// What a running service must be told about (SPEC §3 step 23): the panel
// restarts when its binaries, units, environment or agent token changed;
// nginx reloads when its configuration or a certificate it serves changed.
var (
	panelInputs = []string{"binaries", "units", "panel.env", "token.agent"}
	nginxInputs = []string{"nginx.tuning", "nginx.default-site", "nginx.vhost", "cert.panel", "cert.panel-ip", "cert.fallback"}
)

// panelRunning reports whether the agent or the API is running now.
func (r *run) panelRunning(ctx context.Context) bool {
	for _, u := range []string{"cloudground-agent", "cloudground-api"} {
		if on, _ := r.e.Sd.Active(ctx, u); on {
			return true
		}
	}
	return false
}

// servicesStep enables and starts the panel and nginx, restarts the panel
// and reloads nginx when what they read changed, and waits for :443.
func (r *run) servicesStep() Step {
	e := r.e
	base := r.serviceStep("services", gServices, services, nil)
	restart := func(ctx context.Context) bool {
		return r.wasChanged(panelInputs...) && !r.wasChanged("restarted") && r.panelRunning(ctx)
	}
	reload := func() bool { return r.wasChanged(nginxInputs...) && !r.wasChanged("reloaded") }
	return Step{ID: "services", Group: gServices,
		Check: func(ctx context.Context) (bool, string) {
			if needed, why := base.Check(ctx); needed {
				return needed, why
			}
			if restart(ctx) {
				return need("restart the panel: " + strings.Join(r.changedOf(panelInputs), " ") + " changed")
			}
			if reload() {
				return need("reload nginx: " + strings.Join(r.changedOf(nginxInputs), " ") + " changed")
			}
			if _, why := r.panelDown(ctx); why != "" {
				return need("the panel does not answer: " + why)
			}
			return ok("enabled and running")
		},
		Apply: func(ctx context.Context) error {
			// Decided before enable --now starts anything: a panel that was
			// not running starts on the new files and needs no restart.
			restartPanel := restart(ctx)
			if err := e.Sd.Do(ctx, append([]string{"enable", "--now"}, services...)...); err != nil {
				return r.startFailed(ctx, err)
			}
			if restartPanel {
				if err := e.Sd.Do(ctx, "stop", "cloudground-api"); err != nil {
					e.logf("stop cloudground-api: %v", err)
				}
				if err := e.Sd.Do(ctx, "restart", "cloudground-api.socket"); err != nil {
					return err
				}
				if err := e.Sd.Do(ctx, "restart", "cloudground-agent", "cloudground-api"); err != nil {
					return r.startFailed(ctx, err)
				}
				r.mark("restarted")
			}
			if reload() {
				// apt started nginx before the bootstrap vhost existed, and
				// enable --now does nothing to a running master: without
				// this, :443 never binds. Every nginx change was tested
				// (nginx -t) by the step that made it.
				if err := e.Sd.Do(ctx, "reload", "nginx"); err != nil {
					if err := e.Sd.Do(ctx, "restart", "nginx"); err != nil {
						e.logf("restart nginx: %v", err)
					}
				}
				r.mark("reloaded")
			}
			listening := false
			for i := 0; i < 10 && !listening; i++ {
				if listening = e.Listening != nil && e.Listening(443); !listening {
					e.sleep(time.Second)
				}
			}
			if !listening {
				return fmt.Errorf("nginx is not listening on 443 — the panel would be unreachable")
			}
			return r.waitPanel(ctx)
		}}
}

// panelUnits are the panel's services, the agent first: the API needs it.
var panelUnits = []string{"cloudground-agent", "cloudground-api"}

// PanelWait is how long the services step waits for the panel to answer.
const PanelWait = 30 * time.Second

// panelDown says why the panel is not up ("" when it is): the unit that is
// not active, or else why /healthz, which pings the agent, did not say 200.
func (r *run) panelDown(ctx context.Context) (unit, why string) {
	for _, u := range panelUnits {
		on, err := r.e.Sd.Active(ctx, u)
		if err != nil {
			return u, "cannot ask systemd about " + u + ": " + err.Error()
		}
		if !on {
			return u, u + " is not active"
		}
	}
	if r.e.PanelHealth == nil {
		return "", ""
	}
	if err := r.e.PanelHealth(ctx); err != nil {
		return "", "/healthz: " + err.Error()
	}
	return "", ""
}

// waitPanel waits up to PanelWait for both panel services to be active and
// the API's /healthz to say 200 (SPEC §3 step 23). A unit that cannot start
// (a NAMESPACE failure restarts it for ever, never active for long) fails
// the step, with the end of the unit's journal in the error.
func (r *run) waitPanel(ctx context.Context) error {
	deadline := time.Now().Add(PanelWait)
	var unit, why string
	for i := 0; i < int(PanelWait/time.Second); i++ {
		if unit, why = r.panelDown(ctx); why == "" {
			return nil
		}
		if time.Now().After(deadline) {
			break
		}
		r.e.sleep(time.Second)
	}
	show := panelUnits
	if unit != "" {
		show = []string{unit}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "the panel did not come up within %s: %s", PanelWait, why)
	for _, u := range show {
		b.WriteString(r.journalTail(ctx, u))
	}
	return errors.New(b.String())
}

// startFailed is the error of a systemctl start that failed. When a panel
// unit is down after it, its journal follows, as waitPanel shows it: a unit
// whose start job fails (its ExecStartPre fails at step NAMESPACE, say) makes
// systemctl fail before the wait, and systemctl's own message names neither
// the cause nor the journal.
func (r *run) startFailed(ctx context.Context, err error) error {
	unit, why := r.panelDown(ctx)
	if unit == "" {
		return err
	}
	return fmt.Errorf("%w\n  %s%s", err, why, r.journalTail(ctx, unit))
}

// journalLines is how much of a unit's journal a failure shows.
const journalLines = 15

// journalTail is the end of unit's journal, one indented line each, for an
// error. It is read quietly, and a line with the setup link (a credential,
// SPEC §3 step 30) is left out.
func (r *run) journalTail(ctx context.Context, unit string) string {
	out, err := r.e.Cmd.Quiet(ctx, "journalctl", "-u", unit, "-n", strconv.Itoa(journalLines), "--no-pager", "-o", "cat")
	out = strings.TrimSpace(out)
	if err != nil || out == "" {
		return "\n  (no journal for " + unit + ")"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n  journalctl -u %s:", unit)
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "/setup/") || setupURL.MatchString(l) {
			l = "(a line with the setup link, left out)"
		}
		b.WriteString("\n    " + l)
	}
	return b.String()
}

func (r *run) changedOf(ids []string) []string {
	var out []string
	for _, id := range ids {
		if r.wasChanged(id) {
			out = append(out, id)
		}
	}
	return out
}

// sysctlStep writes the kernel settings and loads them: needed when the
// file differs, or when the running kernel's values differ from it.
func (r *run) sysctlStep() Step {
	e := r.e
	f, _ := managed.Lookup(managed.Sysctl)
	w := want{mode: f.Mode, user: "root", group: "root"}
	return Step{ID: "sysctl", Group: gKernel,
		Check: func(ctx context.Context) (bool, string) {
			content, err := Render(f, Vars{})
			if err != nil {
				return need(err.Error())
			}
			w := w
			w.content = content
			if why := e.fileDiff(managed.Sysctl, w); why != "" {
				return need(why)
			}
			if !r.o.Container {
				if diff := e.liveSysctlDiff(content); len(diff) > 0 {
					return need("the running kernel differs: " + strings.Join(diff, "; "))
				}
			}
			return ok("up to date")
		},
		Apply: func(ctx context.Context) error {
			content, err := Render(f, Vars{})
			if err != nil {
				return err
			}
			w := w
			w.content = content
			if e.fileDiff(managed.Sysctl, w) != "" {
				if err := e.writeFile(managed.Sysctl, content, w); err != nil {
					return err
				}
			}
			if r.o.Container {
				return nil
			}
			// A kernel that refuses a value must not stop the install (as
			// before); doctor then keeps reporting the difference.
			if _, err := e.Cmd.Run(ctx, "sysctl", "-p", managed.Sysctl); err != nil {
				e.logf("warning: sysctl -p: %v", err)
			}
			return nil
		}}
}

// liveSysctlDiff compares the "key = value" lines of conf with /proc/sys.
// A key the kernel does not have is not compared.
func (e *Env) liveSysctlDiff(conf []byte) []string {
	var diff []string
	for _, l := range strings.Split(string(conf), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		k, v, found := strings.Cut(l, "=")
		if !found {
			continue
		}
		k = strings.TrimSpace(k)
		raw, err := e.readFile("/proc/sys/" + strings.ReplaceAll(k, ".", "/"))
		if err != nil {
			continue
		}
		got := strings.Join(strings.Fields(string(raw)), " ")
		if want := strings.Join(strings.Fields(v), " "); got != want {
			diff = append(diff, fmt.Sprintf("%s is %s, want %s", k, got, want))
		}
	}
	return diff
}

// firewallStep opens the web, SSH and panel ports when ufw is already
// active, SSH on the ports sshd really listens on (firewallRulesFor). It
// never enables ufw (nor did install.sh): on a fresh cloud image
// ufw is inactive, and turning it on is the operator's decision (SPEC §3).
func (r *run) firewallStep() Step {
	e := r.e
	active := func(ctx context.Context) (string, bool) {
		if !e.Cmd.LookPath("ufw") {
			return "", false
		}
		out, err := e.Cmd.Run(ctx, "ufw", "status")
		return out, err == nil && ufwActive(out)
	}
	missing := func(ctx context.Context, status string) []string {
		var m []string
		for _, rule := range firewallRulesFor(ctx, e) {
			found := slices.ContainsFunc(strings.Split(status, "\n"), func(l string) bool {
				f := strings.Fields(l)
				return len(f) >= 2 && f[0] == rule && slices.Contains(f, "ALLOW")
			})
			if !found {
				m = append(m, rule)
			}
		}
		return m
	}
	return Step{ID: "firewall", Group: gFirewall,
		Check: func(ctx context.Context) (bool, string) {
			status, on := active(ctx)
			if !on {
				return ok("ufw is not active")
			}
			if m := missing(ctx, status); len(m) > 0 {
				return need("not allowed: " + strings.Join(m, " "))
			}
			return ok("ports open")
		},
		Apply: func(ctx context.Context) error {
			for _, rule := range firewallRulesFor(ctx, e) {
				if _, err := e.Cmd.Run(ctx, "ufw", "allow", rule); err != nil {
					return fmt.Errorf("ufw allow %s: %w", rule, err)
				}
			}
			return nil
		}}
}

func ufwActive(status string) bool { return strings.Contains(status, "Status: active") }

// FirewallOffRules is SPEC §3 step 31: when ufw is installed but inactive,
// the rules to allow before `ufw enable` so that SSH, the sites and the
// panel stay reachable: every port sshd listens on (`sshd -T`, else 22) in
// place of firewallRules' 22/tcp, then the others. nil when ufw is
// missing, active, or its state cannot be read.
func FirewallOffRules(ctx context.Context, e *Env) []string {
	if !e.Cmd.LookPath("ufw") {
		return nil
	}
	out, err := e.Cmd.Run(ctx, "ufw", "status")
	if err != nil || ufwActive(out) || !strings.Contains(out, "Status: inactive") {
		return nil
	}
	return firewallRulesFor(ctx, e)
}

// firewallRulesFor is firewallRules with every port sshd listens on
// (`sshd -T`) in place of its 22/tcp, which stays when sshd's configuration
// cannot be read or names no port: the rules that keep SSH, the sites and
// the panel reachable. Shared by the firewall step (ufw active) and
// FirewallOffRules (ufw inactive).
func firewallRulesFor(ctx context.Context, e *Env) []string {
	var rules []string
	// sshd -T prints the whole effective configuration: read quietly, it
	// adds nothing to the log.
	if cfg, err := e.Cmd.Quiet(ctx, "sshd", "-T"); err == nil {
		for _, l := range strings.Split(cfg, "\n") {
			f := strings.Fields(l)
			if len(f) != 2 || !strings.EqualFold(f[0], "port") {
				continue
			}
			if n, err := strconv.Atoi(f[1]); err == nil && n > 0 && n < 65536 {
				if r := strconv.Itoa(n) + "/tcp"; !slices.Contains(rules, r) {
					rules = append(rules, r)
				}
			}
		}
	}
	if len(rules) == 0 {
		rules = []string{"22/tcp"}
	}
	for _, r := range firewallRules {
		if r != "22/tcp" && !slices.Contains(rules, r) {
			rules = append(rules, r)
		}
	}
	return rules
}
