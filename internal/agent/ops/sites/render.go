package sites

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cloudground-it/cloudground/connector"
	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/rootfile"
	"github.com/cloudground-it/cloudground/internal/agent/ops/staging"
	"github.com/cloudground-it/cloudground/internal/agent/ops/wp"
	"github.com/cloudground-it/cloudground/internal/blocklist"
	"github.com/cloudground-it/cloudground/internal/engine"
	"github.com/cloudground-it/cloudground/internal/phpfpm"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
	"github.com/cloudground-it/cloudground/internal/sysprobe"
	"github.com/cloudground-it/cloudground/internal/velocity"
)

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// bcryptRe is the only password hash written to an nginx password file.
var bcryptRe = regexp.MustCompile(`^\$2[aby]\$[0-9]{2}\$[./A-Za-z0-9]{53}$`)

// nginxVersionRe finds the version in `nginx -V`.
var nginxVersionRe = regexp.MustCompile(`nginx/(\d+)\.(\d+)\.(\d+)`)

// probeBuild reads `nginx -V` once: HTTP/3 when the build has the module,
// the "http2 on;" directive from 1.25.1 (SPEC §4.1). A build cannot change
// without the agent restarting.
func (o *Ops) probeBuild(ctx context.Context) {
	o.buildOnce.Do(func() {
		info, err := o.Nginx.BuildInfo(context.WithoutCancel(ctx))
		if err != nil {
			o.log().Warn("reading the nginx build failed; rendering without HTTP/3", "err", err)
			return
		}
		o.http3 = strings.Contains(info, "http_v3_module")
		if m := nginxVersionRe.FindStringSubmatch(info); m != nil {
			v := [3]int{}
			for i := range v {
				v[i], _ = strconv.Atoi(m[i+1])
			}
			o.http2On = v[0] > 1 || (v[0] == 1 && (v[1] > 25 || (v[1] == 25 && v[2] >= 1)))
		}
	})
}

// HTTP3 reports whether nginx was built with HTTP/3, from the probe the
// renderer uses.
func (o *Ops) HTTP3(ctx context.Context) bool {
	o.probeBuild(ctx)
	return o.http3
}

// input is what the renderer gets for s (SPEC §4.1).
func (o *Ops) input(ctx context.Context, s state.Site) engine.Input {
	docRoot := filepath.Join(s.RootPath, "current")
	symlinkFrom := ""
	if s.Config.PublicRoot != "" {
		// nginx checks the links of every component after "current"
		// (docroot.go), the public root's included.
		symlinkFrom = docRoot
		docRoot = filepath.Join(docRoot, s.Config.PublicRoot)
	}
	certDir := filepath.Join(o.Paths.CertLiveDir, s.Domain)
	custom := filepath.Join(o.Paths.CustomCertDir, s.Domain)
	if _, err := os.Stat(filepath.Join(custom, "fullchain.pem")); err == nil {
		certDir = custom
	}
	in := engine.Input{
		Site:          s,
		Policy:        velocity.PolicyFor(s),
		DocRoot:       docRoot,
		SymlinkFrom:   symlinkFrom,
		CacheDir:      filepath.Join(o.Paths.CacheRoot, velocity.SanitizeCacheDirName(s.Domain)),
		LogDir:        filepath.Join(o.Paths.LogRoot, s.Domain),
		CertFullchain: filepath.Join(certDir, "fullchain.pem"),
		CertKey:       filepath.Join(certDir, "privkey.pem"),
		FallbackCert:  o.Paths.FallbackCert,
		FallbackKey:   o.Paths.FallbackKey,
		ACMEWebroot:   o.Paths.ACMEWebroot,
		AuthLogSocket: o.Paths.AuthLogSocket,
	}
	if _, err := os.Stat(in.CertFullchain); err == nil {
		in.CertAvailable = true
	}
	if isPHP(s) {
		in.PHPSocket = filepath.Join(o.Paths.PHPSocketDir, s.SystemUser, "php.sock")
	}
	if s.Config.StagingAuthHash != "" {
		in.BasicAuthFile = staging.AuthFile(o.Paths.NginxAuthDir, s.Domain)
		in.InternalKey = s.InternalKey()
	}
	for _, p := range s.Config.ProtectedPaths {
		in.ProtectedPaths = append(in.ProtectedPaths, engine.ProtectedPath{Path: p.Path, File: protectedAuthFile(o.Paths.NginxAuthDir, s.Domain, p.Path)})
	}
	if b := s.Config.Blocking; b.Any() {
		in.Blocking = &engine.Blocking{Exempt: o.localAddrs(), UserAgents: b.UserAgents,
			Countries: b.Countries, AllowCountriesOnly: b.AllowCountriesOnly}
		for _, a := range b.Addresses {
			// Checked when the site was (site.CheckBlocking).
			if p, err := blocklist.ParsePrefix(a); err == nil {
				in.Blocking.Addresses = append(in.Blocking.Addresses, p)
			}
		}
	}
	o.probeBuild(ctx)
	in.HTTP3, in.HTTP2Directive = o.http3, o.http2On
	if s.Config.Routing != "" {
		// What a custom routing block must not reach (internal/engine
		// SPEC §7.3), as this machine is now.
		in.SystemPorts, in.LocalAddrs, in.Resolve = o.systemPorts(s), o.localAddrs(), o.resolve
	}
	return in
}

// configTx remembers what each file it wrote was before, so a failed render
// can put every one of them back.
type configTx struct {
	prior map[string]*priorFile // nil: the file did not exist
	files []rpc.ManagedFile
}

// priorFile is a file as it was: content, mode and owner. A restore that
// lost the mode would leave a staging vhost's key, or a pool's settings,
// readable by everyone.
type priorFile struct {
	content  []byte
	mode     os.FileMode
	uid, gid int
}

// write replaces path with content (temp file and rename, mode mode) and
// reports whether the content changed.
func (tx *configTx) write(path, content string, mode os.FileMode) (bool, error) {
	old, readErr := os.ReadFile(path)
	if _, seen := tx.prior[path]; !seen {
		tx.prior[path] = nil
		if fi, err := os.Lstat(path); readErr == nil && err == nil {
			p := &priorFile{content: old, mode: fi.Mode().Perm()}
			if st, ok := fi.Sys().(*syscall.Stat_t); ok {
				p.uid, p.gid = int(st.Uid), int(st.Gid)
			}
			tx.prior[path] = p
		}
	}
	if err := writeFile(path, []byte(content), mode); err != nil {
		return false, err
	}
	sum := sha256.Sum256([]byte(content))
	tx.files = append(tx.files, rpc.ManagedFile{Path: path, SHA256: hex.EncodeToString(sum[:])})
	return readErr != nil || string(old) != content, nil
}

// remove deletes path, remembering it for restore, and reports whether
// there was a file.
func (tx *configTx) remove(path string) (bool, error) {
	old, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, seen := tx.prior[path]; !seen {
		p := &priorFile{content: old, mode: 0o644}
		if fi, err := os.Lstat(path); err == nil {
			p.mode = fi.Mode().Perm()
			if st, ok := fi.Sys().(*syscall.Stat_t); ok {
				p.uid, p.gid = int(st.Uid), int(st.Gid)
			}
		}
		tx.prior[path] = p
	}
	return true, os.Remove(path)
}

// existed reports whether path was there before the render.
func (tx *configTx) existed(path string) bool {
	return tx.prior[path] != nil
}

// restore puts every file back as it was.
func (tx *configTx) restore() error {
	var errs []error
	for path, p := range tx.prior {
		if p == nil {
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, fmt.Errorf("remove %s: %w", path, err))
			}
			continue
		}
		if err := writeFile(path, p.content, p.mode); err != nil {
			errs = append(errs, fmt.Errorf("restore %s: %w", path, err))
		} else if err := os.Lchown(path, p.uid, p.gid); err != nil && os.Geteuid() == 0 {
			errs = append(errs, fmt.Errorf("restore the owner of %s: %w", path, err))
		}
	}
	return errors.Join(errs...)
}

// writeFile writes path atomically: a temp file in its directory, renamed
// over it. The directory, and any parent, is created 0755 if missing,
// whatever the agent's umask (SPEC §4.2); one that exists is left alone.
func writeFile(path string, content []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := rootfile.MkdirAllMode(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".cloudground-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(content)
	if err == nil {
		err = f.Chmod(mode)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// Render writes a site's configuration and loads it (SPEC §4).
func (o *Ops) Render(ctx context.Context, s state.Site) ([]rpc.ManagedFile, error) {
	return o.render(ctx, s, false)
}

// render writes the PHP master's files and loads them, then nginx's. The
// caller holds the site's lock, which covers everything of the site's own;
// the nginx lock is taken only around nginx's files and reload (shared with
// every other site), never across a systemd job: a PHP master's restart
// used to be waited for under it, and one lost job result stalled every
// render and deletion on the machine.
func (o *Ops) render(ctx context.Context, s state.Site, onlyIfChanged bool) ([]rpc.ManagedFile, error) {
	if err := o.checkDocRoot(s); err != nil {
		return nil, err
	}
	in := o.input(ctx, s)
	if in.Blocking != nil && len(in.Blocking.Countries) > 0 {
		// The countries' addresses, from DB-IP's data (ops/blocklist §5).
		if o.CountryPrefixes == nil {
			return nil, fmt.Errorf("blocking countries needs the country data, which this agent cannot read")
		}
		pfx, err := o.CountryPrefixes(ctx, in.Blocking.Countries)
		if err != nil {
			return nil, err
		}
		in.Blocking.CountryPrefixes = pfx
	}
	nginxFiles, err := o.Renderer.SiteFiles(in)
	if err != nil {
		return nil, err
	}
	for p, c := range o.Renderer.GlobalFiles() {
		nginxFiles[p] = c
	}
	if in.BasicAuthFile != "" {
		if !bcryptRe.MatchString(s.Config.StagingAuthHash) {
			return nil, fmt.Errorf("invalid staging password hash")
		}
		nginxFiles[in.BasicAuthFile] = "staging:" + s.Config.StagingAuthHash + "\n"
	}
	for i, p := range s.Config.ProtectedPaths {
		if !bcryptRe.MatchString(p.PasswordHash) || !site.ValidBasicAuthUser(p.Username) {
			return nil, fmt.Errorf("invalid credentials for the protected path %s", p.Path)
		}
		nginxFiles[in.ProtectedPaths[i].File] = p.Username + ":" + p.PasswordHash + "\n"
	}
	if err := o.prepareDirs(s, in); err != nil {
		return nil, err
	}

	// The site's own files: under the site's lock only.
	phpTx := &configTx{prior: map[string]*priorFile{}}
	fail := func(cause error) ([]rpc.ManagedFile, error) {
		if err := o.rollbackPHP(context.WithoutCancel(ctx), s, phpTx); err != nil {
			return nil, fmt.Errorf("%w; rolling the configuration back also failed: %v", cause, err)
		}
		return nil, cause
	}
	// The slice holds every process of the site, PHP or not (its cron and
	// shell too), so every site has its limits (SPEC §3.1).
	if err := o.renderSlice(ctx, s, phpTx); err != nil {
		return fail(err)
	}
	if isPHP(s) {
		if err := o.renderPHP(ctx, s, phpTx, onlyIfChanged); err != nil {
			return fail(err)
		}
	}
	nginxTx := &configTx{prior: map[string]*priorFile{}}
	if err := o.renderNginx(ctx, in, nginxFiles, nginxTx, onlyIfChanged); err != nil {
		return fail(err)
	}

	// The password files of paths no longer protected: the vhost that
	// named them is gone.
	o.removeStaleAuthFiles(s, in)

	files := append(phpTx.files, nginxTx.files...)
	if isWordPress(s) {
		sum := sha256.Sum256([]byte(connector.PHP))
		files = append(files, rpc.ManagedFile{
			Path:   filepath.Join(s.RootPath, "current", wp.ConnectorPlugin),
			SHA256: hex.EncodeToString(sum[:]),
		})
	}
	return files, nil
}

// renderNginx writes nginx's files and reloads it, under the nginx lock. A
// failure puts every file back and reloads nginx before the lock is let go,
// so no other render sees, or keeps, this one's half.
func (o *Ops) renderNginx(ctx context.Context, in engine.Input, nginxFiles map[string]string, tx *configTx, onlyIfChanged bool) error {
	o.NginxMu.Lock()
	defer o.NginxMu.Unlock()
	err := o.writeNginx(ctx, in, nginxFiles, tx, onlyIfChanged)
	if err == nil {
		return nil
	}
	rctx := context.WithoutCancel(ctx)
	if rerr := errors.Join(tx.restore(), o.reloadNginx(rctx)); rerr != nil {
		return fmt.Errorf("%w; rolling nginx back also failed: %v", err, rerr)
	}
	return err
}

// writeNginx writes the files and reloads nginx if one changed (or always,
// without onlyIfChanged). The caller holds NginxMu.
func (o *Ops) writeNginx(ctx context.Context, in engine.Input, nginxFiles map[string]string, tx *configTx, onlyIfChanged bool) error {
	nginxChanged := false
	for path, content := range nginxFiles {
		mode := os.FileMode(0o644)
		auth := isAuthFile(in, path)
		switch {
		case auth:
			mode = 0o640 // the password hash is nginx's business alone
		case in.InternalKey != "" && strings.Contains(content, in.InternalKey):
			mode = 0o600 // the key that passes the password: nginx's master only
		}
		changed, err := tx.write(path, content, mode)
		if err != nil {
			return err
		}
		nginxChanged = nginxChanged || changed
		if auth {
			if err := shareWithNginx(path); err != nil {
				return err
			}
		}
	}
	// Rules taken away: their file goes before the reload, so nginx does
	// not keep loading them (SPEC §4.6).
	if in.Blocking == nil {
		removed, err := tx.remove(o.blockingPath(in.Site))
		if err != nil {
			return err
		}
		nginxChanged = nginxChanged || removed
	}
	if !onlyIfChanged || nginxChanged {
		return o.reloadNginx(ctx)
	}
	return nil
}

// protectedAuthFile is the password file of one protected path (SPEC §4.6):
// named after the path's digest, so a path never becomes a file name.
func protectedAuthFile(dir, domain, path string) string {
	sum := sha256.Sum256([]byte(path))
	return filepath.Join(dir, domain+".path-"+hex.EncodeToString(sum[:6])+".htpasswd")
}

// isAuthFile reports whether path is one of in's password files.
func isAuthFile(in engine.Input, path string) bool {
	if path == in.BasicAuthFile {
		return true
	}
	return slices.ContainsFunc(in.ProtectedPaths, func(p engine.ProtectedPath) bool { return p.File == path })
}

// removeStaleAuthFiles removes the site's protected-path password files the
// render did not write (SPEC §4.6).
func (o *Ops) removeStaleAuthFiles(s state.Site, in engine.Input) {
	if o.Paths.NginxAuthDir == "" {
		return
	}
	old, _ := filepath.Glob(filepath.Join(o.Paths.NginxAuthDir, s.Domain+".path-*.htpasswd"))
	for _, f := range old {
		if !isAuthFile(in, f) {
			if err := os.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
				o.log().Warn("could not remove a stale password file", "file", f, "err", err)
			}
		}
	}
}

// prepareDirs creates the page-cache and log directories and, for a PHP
// site, its error log (SPEC §4.2).
func (o *Ops) prepareDirs(s state.Site, in engine.Input) error {
	// The roots above a site's cache and log directories are traversed by
	// nginx's workers and the site's user: 0755 when the agent makes them,
	// never what its UMask=0027 would leave (0750, or 0700 for the cache).
	if in.Policy.Enabled {
		if err := rootfile.MkdirAllMode(filepath.Dir(in.CacheDir), 0o755); err != nil {
			return err
		}
		if err := os.MkdirAll(in.CacheDir, 0o700); err != nil {
			return err
		}
	}
	if err := rootfile.MkdirAllMode(filepath.Dir(in.LogDir), 0o755); err != nil {
		return err
	}
	if err := rootfile.MkdirAllMode(in.LogDir, 0o750); err != nil {
		return err
	}
	if owner, err := o.Owner(s.SystemUser); err == nil {
		if err := os.Lchown(in.LogDir, 0, owner.GID); err != nil {
			return err
		}
	}
	if err := os.Chmod(in.LogDir, 0o750); err != nil {
		return err
	}
	if in.PHPSocket == "" {
		return nil
	}
	owner, err := o.Owner(s.SystemUser)
	if err != nil {
		return err
	}
	path := filepath.Join(in.LogDir, "php-error.log")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW, 0o640)
	if err != nil {
		return fmt.Errorf("prepare %s: %w", path, err)
	}
	defer f.Close()
	return f.Chown(owner.UID, owner.GID)
}

// shareWithNginx makes a password file and its directory root:www-data,
// the directory 0750: nginx's workers read it, nobody else.
func shareWithNginx(path string) error {
	g, err := user.LookupGroup("www-data")
	if err != nil {
		return nil // no www-data here (a test machine): root alone reads it
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.Lchown(dir, 0, gid); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o750); err != nil {
		return err
	}
	return os.Lchown(path, 0, gid)
}

// renderPHP writes the site's pool, unit and slice drop-in and loads them
// (SPEC §4.4).
func (o *Ops) renderPHP(ctx context.Context, s state.Site, tx *configTx, onlyIfChanged bool) error {
	_, pool, err := phpfpm.RenderPool(s, o.budget(s))
	if err != nil {
		return err
	}
	unitName, unit, err := phpfpm.RenderUnit(s, o.Paths.PHPBinDir)
	if err != nil {
		return err
	}
	// The master reads its pool as the site's user, and so does the
	// configuration test: the directory is 0755, set every time (the agent's
	// UMask=0027 made it 0750 on a fresh server, and every PHP site failed
	// its FPM test with "Permission denied"; SPEC §4.4).
	if err := rootfile.MkdirMode(o.Paths.PHPMasterDir, 0o755); err != nil {
		return err
	}
	poolChanged, err := tx.write(o.poolPath(s), pool, 0o640)
	if err != nil {
		return err
	}
	if owner, err := o.Owner(s.SystemUser); err == nil {
		if err := os.Lchown(o.poolPath(s), 0, owner.GID); err != nil {
			return err
		}
	}
	unitChanged, err := tx.write(filepath.Join(o.Paths.SystemdDir, unitName), unit, 0o644)
	if err != nil {
		return err
	}
	dropinChanged, err := tx.write(o.dropinPath(s), sliceDropin(s), 0o644)
	if err != nil {
		return err
	}
	restart := unitChanged || dropinChanged
	if restart {
		if err := o.Systemd.DaemonReload(ctx); err != nil {
			return err
		}
	}
	if onlyIfChanged && !restart && !poolChanged {
		// Nothing the master reads changed: a reload would only empty its
		// OPcache. Start it if it is down.
		if err := o.Systemd.Enable(ctx, unitName); err != nil {
			return err
		}
		return o.Systemd.Start(ctx, unitName)
	}
	if err := o.testPHP(ctx, s); err != nil {
		return err
	}
	return o.startPHP(ctx, s, restart)
}

// slicePath is the site's slice unit.
func (o *Ops) slicePath(s state.Site) string { return site.SlicePath(o.Paths.SystemdDir, s.SystemUser) }

// renderSlice writes the site's slice unit, its accounting and limits, and
// has systemd load it when it changed: a running slice takes new limits on
// the reload, with nothing in it restarted (SPEC §3.1).
func (o *Ops) renderSlice(ctx context.Context, s state.Site, tx *configTx) error {
	unit, err := site.SliceUnit(s, site.EffectiveLimits(s.Config.Resources, o.memTotalMB()))
	if err != nil {
		return err
	}
	changed, err := tx.write(o.slicePath(s), unit, 0o644)
	if err != nil || !changed {
		return err
	}
	return o.Systemd.DaemonReload(ctx)
}

// sliceDropin puts the site's PHP master in the site's slice.
func sliceDropin(s state.Site) string {
	return "# Managed by CloudGround — do not edit. Changes are detected as drift.\n" +
		"[Service]\nSlice=" + site.Slice(s.SystemUser) + "\n"
}

// rollbackPHP restores the PHP master's files a failed render wrote and
// reloads what read them (SPEC §4.6); nginx's are restored by renderNginx.
func (o *Ops) rollbackPHP(ctx context.Context, s state.Site, tx *configTx) error {
	errs := []error{tx.restore()}
	if !tx.existed(o.dropinPath(s)) {
		_ = os.Remove(o.dropinDir(s)) // only if empty: it was made for the drop-in
	}
	if !isPHP(s) || s.PHPVersion == "" {
		// The slice unit may have been written: the old limits come back
		// with the reload.
		if _, wrote := tx.prior[o.slicePath(s)]; wrote {
			if err := o.Systemd.DaemonReload(ctx); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if isPHP(s) && s.PHPVersion != "" {
		if err := o.Systemd.DaemonReload(ctx); err != nil {
			errs = append(errs, err)
		}
		// A master is restarted on the restored files only if it had them
		// before. A first render has nothing to go back to: the master it
		// may have started is stopped.
		if tx.existed(o.unitPath(s)) {
			if err := o.startPHP(ctx, s, true); err != nil {
				errs = append(errs, err)
			}
		} else if err := o.Systemd.Stop(ctx, phpfpm.UnitName(s.SystemUser)); err != nil && !errors.Is(err, host.ErrNoSuchUnit) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// budget is the memory the site's PHP workers are sized against (SPEC §4.4).
func (o *Ops) budget(s state.Site) int {
	if mb := s.Config.Resources.MemoryHighMB; mb > 0 {
		return mb
	}
	total := o.memTotalMB()
	if total <= 0 {
		return 0
	}
	sites := provisionedSites(o.Paths.SitesRoot)
	mb, floored := shareOf(total, sites)
	if floored {
		o.log().Warn("site memory budget floored: this server is over-subscribed",
			"sites", sites, "mem_total_mb", total, "budget_mb", mb)
	}
	return mb
}

func (o *Ops) memTotalMB() int64 {
	if o.MemTotalMB != nil {
		return o.MemTotalMB()
	}
	f, _ := sysprobe.Probe(o.Paths.SitesRoot)
	return f.MemTotalMB
}

// minBudgetMB is the smallest budget that still gives a site the two
// workers phpfpm.SizeWorkers insists on.
const minBudgetMB = 160

// shareOf divides a quarter of the machine between the sites; floored says
// the share fell under minBudgetMB and was raised to it.
func shareOf(totalMB int64, sites int) (int, bool) {
	share := totalMB / 4 / int64(max(sites, 1))
	if share < minBudgetMB {
		return minBudgetMB, true
	}
	return int(share), false
}

// provisionedSites counts the trees with a releases/ directory, at least 1.
func provisionedSites(sitesRoot string) int {
	ents, err := os.ReadDir(sitesRoot)
	n := 0
	if err == nil {
		for _, e := range ents {
			if fi, err := os.Stat(filepath.Join(sitesRoot, e.Name(), "releases")); e.IsDir() && err == nil && fi.IsDir() {
				n++
			}
		}
	}
	return max(n, 1)
}

// fpmTests numbers the configuration tests' units.
var fpmTests atomic.Uint64

// FPMTestService is the transient service the configuration test runs in
// (SPEC §4 step 4): the site's user, slice and sandbox (phpfpm.Sandbox),
// as the master itself runs, never the agent's own mount namespace.
func (o *Ops) FPMTestService(s state.Site) host.Service {
	return host.Service{
		Unit:        fmt.Sprintf("cg-fpm-test-%s-%d-%d.service", s.SystemUser, time.Now().UnixNano(), fpmTests.Add(1)),
		Description: "php-fpm configuration test for " + s.Domain,
		User:        s.SystemUser, Slice: site.Slice(s.SystemUser), Dir: "/",
		Props: phpfpm.Sandbox(s), MaxRuntime: time.Minute, Pipe: true,
		Argv: []string{o.fpmBinary(s.PHPVersion), "-t", "--fpm-config", o.poolPath(s)},
	}
}

// testPHP checks the site's master configuration as the site user, in the
// site's sandbox.
func (o *Ops) testPHP(ctx context.Context, s state.Site) error {
	if !site.ValidPHPVersion(s.PHPVersion) {
		return fmt.Errorf("unsupported php version %q", s.PHPVersion)
	}
	svc := o.FPMTestService(s)
	res, err := o.Exec.Run(ctx, host.Cmd{Name: "systemd-run", Args: host.ServiceArgs(svc),
		Env: []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"}})
	if err != nil && (ctx.Err() != nil || errors.Is(err, host.ErrStdoutTooLarge)) {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		_, _ = o.Exec.Run(sctx, host.Cmd{Name: "systemctl", Args: []string{"stop", svc.Unit}})
		cancel()
	}
	if err != nil {
		out := strings.TrimSpace(string(res.Stderr) + "\n" + string(res.Stdout))
		return fmt.Errorf("php-fpm configuration test for %s failed: %s: %w", s.Domain, out, err)
	}
	return nil
}

// startPHP enables the site's master and restarts it, or reloads it
// (starting it if it is down).
func (o *Ops) startPHP(ctx context.Context, s state.Site, restart bool) error {
	unit := phpfpm.UnitName(s.SystemUser)
	if err := o.Systemd.Enable(ctx, unit); err != nil {
		return err
	}
	if restart {
		return o.Systemd.Restart(ctx, unit)
	}
	return o.Systemd.ReloadOrRestart(ctx, unit)
}

// ReloadPHP makes the site's master pick up new code. A site without PHP,
// or whose master is not there yet, has nothing to reload.
func (o *Ops) ReloadPHP(ctx context.Context, s state.Site) error {
	if s.PHPVersion == "" || !site.ValidSystemUser(s.SystemUser) {
		return nil
	}
	if _, err := os.Stat(o.unitPath(s)); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return o.Systemd.ReloadOrRestart(ctx, phpfpm.UnitName(s.SystemUser))
}

// RestartPHP replaces the site's master: a fresh mount of its root, empty
// OPcache and APCu.
func (o *Ops) RestartPHP(ctx context.Context, s state.Site) error {
	if s.PHPVersion == "" || !site.ValidSystemUser(s.SystemUser) {
		return nil
	}
	return o.Systemd.Restart(ctx, phpfpm.UnitName(s.SystemUser))
}

// ReloadNginx tests and reloads nginx under the nginx lock.
func (o *Ops) ReloadNginx(ctx context.Context) error {
	o.NginxMu.Lock()
	defer o.NginxMu.Unlock()
	return o.reloadNginx(ctx)
}

// reloadNginx tests the configuration and reloads it, returning once a new
// worker generation runs. The caller holds NginxMu.
func (o *Ops) reloadNginx(ctx context.Context) error {
	if err := o.Nginx.Test(ctx); err != nil {
		return err
	}
	return o.Nginx.Reload(ctx)
}
