// Package functions sets standalone functions up on the server: their user,
// their code in releases with its dependencies, their units, their nginx
// files and certificates, and invokes them (#143 v0.9.0, SPEC.md).
package functions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/rootfile"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	fn "github.com/cloudground-it/cloudground/internal/functions"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/runtimes"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// installLimit bounds a dependency step (SPEC §2).
const installLimit = 20 * time.Minute

// Ops holds what the handlers use.
type Ops struct {
	Systemd  host.Systemd
	Accounts host.Accounts
	Nginx    host.Nginx
	ACME     host.ACME
	// LookupOwner resolves a function's user to its uid and gid; nil is
	// the user database.
	LookupOwner func(user string) (sitefs.Owner, error)
	// OpenSite opens a site's tree (MigrateFunction).
	OpenSite func(state.Site) (*sitefs.Root, error)
	// Run runs a dependency step's transient service (ops/runtimes Run).
	Run func(ctx context.Context, svc host.Service, what string) (string, error)
	// Installed reports whether a runtime version is on the server.
	Installed func(runtime, version string) bool
	// Preflight checks that domains answer here on port 80 (ops/certs).
	Preflight func(ctx context.Context, domains []string) error
	// KillUser ends a user's processes before it is deleted; nil ends
	// none.
	KillUser func(ctx context.Context, name string) error
	// NginxMu and CertMu are the agent's nginx and certbot locks.
	NginxMu, CertMu *sync.Mutex

	// Where things are; "" is the package's default (internal/functions).
	Root, Dir, LogDir string
	// SystemdDir holds the units, SitesDir nginx's sites.
	SystemdDir, SitesDir string
	// CertLiveDir is certbot's live/; FallbackCert and FallbackKey the
	// certificate a server has before its own.
	CertLiveDir, FallbackCert, FallbackKey, ACMEWebroot string
	// NginxSocket is the internal server's socket ("" is fn.InvokeSocket).
	NginxSocket string
	Now         func() time.Time
	Log         *slog.Logger

	locks sync.Map // function name -> *sync.Mutex
}

// Register wires the methods into the agent's RPC server.
func (o *Ops) Register(s *agentrpc.Server) {
	agentrpc.Handle(s, agentrpc.SetFunction, o.Set)
	agentrpc.Handle(s, agentrpc.DeleteFunction, func(ctx context.Context, p rpc.FunctionRef) (agentrpc.None, error) {
		return agentrpc.None{}, o.Delete(ctx, p.ID, p.Name)
	})
	agentrpc.Handle(s, agentrpc.FunctionCode, o.Code)
	agentrpc.Handle(s, agentrpc.RollbackFunction, o.Rollback)
	agentrpc.Handle(s, agentrpc.InvokeFunction, o.Invoke)
	agentrpc.Handle(s, agentrpc.FunctionLogs, o.Logs)
	agentrpc.Handle(s, agentrpc.FunctionsSummary, o.Summary)
	agentrpc.Handle(s, agentrpc.RenderFunctions, o.Render)
	agentrpc.Handle(s, agentrpc.IssueFunctionsCertificate, func(ctx context.Context, p rpc.FunctionsCertParams) (agentrpc.None, error) {
		return agentrpc.None{}, o.IssueCertificate(ctx, p)
	})
	agentrpc.Handle(s, agentrpc.DeleteFunctionsCertificate, func(ctx context.Context, p rpc.FunctionsCertParams) (agentrpc.None, error) {
		return agentrpc.None{}, o.deleteCert(ctx, p.Name)
	})
	agentrpc.Handle(s, agentrpc.MigrateFunction, o.Migrate)
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (o *Ops) root() string   { return or(o.Root, fn.Root) }
func (o *Ops) dir() string    { return or(o.Dir, fn.Dir) }
func (o *Ops) logDir() string { return or(o.LogDir, fn.LogDir) }

func (o *Ops) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o *Ops) log() *slog.Logger {
	if o.Log != nil {
		return o.Log
	}
	return slog.Default()
}

// lock serialises the changes to one function's files and units.
func (o *Ops) lock(name string) func() {
	m, _ := o.locks.LoadOrStore(name, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (o *Ops) envPath(id int64) string { return filepath.Join(o.dir(), filepath.Base(fn.EnvFile(id))) }
func (o *Ops) accessLog(name string) string {
	return filepath.Join(o.logDir(), filepath.Base(fn.AccessLog(name)))
}
func (o *Ops) outputLog(name string) string {
	return filepath.Join(o.logDir(), filepath.Base(fn.OutputLog(name)))
}

func (o *Ops) owner(user string) (sitefs.Owner, error) {
	if o.LookupOwner != nil {
		return o.LookupOwner(user)
	}
	return sitefs.LookupOwner(user)
}

// rootOwned is r creating entries for root with the function's group.
func rootOwned(r *sitefs.Root) *sitefs.Root {
	return r.WithOwner(sitefs.Owner{UID: 0, GID: r.Owner().GID})
}

// ensureUser makes the function's user when it is missing (SPEC §2 step 2).
func (o *Ops) ensureUser(ctx context.Context, id int64) (sitefs.Owner, error) {
	u := fn.User(id)
	exists, err := o.Accounts.UserExists(ctx, u)
	if err != nil {
		return sitefs.Owner{}, err
	}
	if !exists {
		if err := o.Accounts.CreateUser(ctx, host.UserSpec{Name: u, System: true, Home: "/nonexistent", Shell: "/usr/sbin/nologin"}); err != nil {
			return sitefs.Owner{}, fmt.Errorf("create the function's user: %w", err)
		}
	}
	return o.owner(u)
}

// openTree makes the function's directories as SPEC §2 says and opens its
// own, creating for the function's user.
func (o *Ops) openTree(name string, owner sitefs.Owner) (*sitefs.Root, error) {
	if err := rootfile.MkdirAllMode(o.root(), 0o711); err != nil {
		return nil, err
	}
	if err := os.Chmod(o.root(), 0o711); err != nil {
		return nil, err
	}
	top, err := sitefs.Open(o.root(), sitefs.Owner{UID: 0, GID: owner.GID})
	if err != nil {
		return nil, err
	}
	defer top.Close()
	if err := top.Mkdir(name, 0o750); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	if err := top.Chown(name, 0, owner.GID); err != nil {
		return nil, err
	}
	if err := top.Chmod(name, 0o750); err != nil {
		return nil, err
	}
	sub, err := top.Sub(name)
	if err != nil {
		return nil, err
	}
	r := sub.WithOwner(owner)
	if err := rootOwned(r).MkdirAll("releases", 0o750); err != nil {
		r.Close()
		return nil, err
	}
	if err := r.Chown("releases", 0, owner.GID); err != nil {
		r.Close()
		return nil, err
	}
	if err := r.Chmod("releases", 0o750); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}

// openExisting opens a function's directory to read it.
func (o *Ops) openExisting(name string) (*sitefs.Root, error) {
	if !fn.ValidName(name) {
		return nil, agentrpc.Invalid("invalid function %q", name)
	}
	return sitefs.Open(filepath.Join(o.root(), name), sitefs.Owner{})
}

// Set is SetFunction (SPEC §2).
func (o *Ops) Set(ctx context.Context, p rpc.SetFunctionParams) (rpc.FunctionResult, error) {
	f := p.Function
	if err := p.Validate(); err != nil {
		return rpc.FunctionResult{}, agentrpc.Invalid("%s", err.Error())
	}
	if o.Installed != nil && !o.Installed(f.Runtime, f.Version) {
		return rpc.FunctionResult{}, agentrpc.Invalid("%s %s is not installed on this server", runtimes.Label(f.Runtime), f.Version)
	}
	defer o.lock(f.Name)()
	owner, err := o.ensureUser(ctx, f.ID)
	if err != nil {
		return rpc.FunctionResult{}, err
	}
	r, err := o.openTree(f.Name, owner)
	if err != nil {
		return rpc.FunctionResult{}, err
	}
	defer r.Close()
	var out string
	if _, err := live(r); p.Files != nil || err != nil {
		files := p.Files
		if files == nil {
			files = fn.Starter(f.Runtime, fn.TemplateHTTP)
		}
		if out, err = o.newRelease(ctx, r, f, files); err != nil {
			return rpc.FunctionResult{Output: out}, err
		}
	}
	if err := o.install(ctx, f, p.Env); err != nil {
		return rpc.FunctionResult{}, err
	}
	res, err := result(r)
	res.Output = out
	return res, err
}

// install writes the shims, the environment, the log directory, the slice
// and the units (SPEC §2 steps 5–6).
func (o *Ops) install(ctx context.Context, f state.Function, env map[string]string) error {
	if err := o.writeShims(); err != nil {
		return err
	}
	if err := rootfile.MkdirAllMode(o.dir(), 0o755); err != nil {
		return err
	}
	if err := rootfile.Replace(o.envPath(f.ID), []byte(runtimes.RenderEnv(env)), 0o600); err != nil {
		return err
	}
	if err := rootfile.MkdirAllMode(o.logDir(), 0o750); err != nil {
		return err
	}
	return o.writeUnits(ctx, f)
}

var runs atomic.Uint64

// installService is a release's dependency step (SPEC §2).
func (o *Ops) installService(f state.Function, release string) host.Service {
	return host.Service{
		Unit:        fmt.Sprintf("cg-fn-prep-%d-%d-%d.service", f.ID, time.Now().UnixNano(), runs.Add(1)),
		Description: "CloudGround: install the dependencies of function " + f.Name,
		User:        fn.User(f.ID), Slice: fn.Slice(f.ID),
		Dir:        path.Join(o.root(), f.Name, "releases", release),
		Env:        fn.PrepareEnv(f.Runtime, f.Version),
		Props:      append(fn.InstallSandbox(f.Name, release), host.SandboxedProps...),
		MaxRuntime: installLimit, Pipe: true,
		Argv: []string{"/bin/sh", "-c", "exec 2>&1\n" + fn.PrepareScript(f.Runtime, f.Version)},
	}
}

// newRelease writes files as a new release, installs its dependencies and
// moves current to it; a failure removes the release (SPEC §2 step 4).
func (o *Ops) newRelease(ctx context.Context, r *sitefs.Root, f state.Function, files map[string]string) (string, error) {
	id := site.NewReleaseID(o.now())
	rel := "releases/" + id
	if err := r.Mkdir(rel, 0o750); err != nil {
		return "", err
	}
	drop := func() {
		if err := rootOwned(r).RemoveAll(rel); err != nil {
			o.log().Warn("could not remove a failed release", "function", f.Name, "release", id, "err", err)
		}
	}
	paths := fn.SortedPaths(files)
	for _, p := range paths {
		if !fn.ValidPath(p) {
			drop()
			return "", agentrpc.Invalid("%q is not a file path a function may have", p)
		}
		if d := path.Dir(p); d != "." {
			if err := r.MkdirAll(rel+"/"+d, 0o750); err != nil {
				drop()
				return "", err
			}
		}
		content := files[p]
		if err := r.Replace(rel+"/"+p, 0o640, func(w *os.File) error {
			_, err := w.WriteString(content)
			return err
		}); err != nil {
			drop()
			return "", err
		}
	}
	manifest, _ := json.Marshal(paths)
	if err := rootOwned(r).Replace(rel+"/"+fn.ManifestFile, 0o644, func(w *os.File) error {
		_, err := w.Write(manifest)
		return err
	}); err != nil {
		drop()
		return "", err
	}
	var out string
	if fn.NeedsInstall(f.Runtime, files) && o.Run != nil {
		var err error
		if out, err = o.Run(ctx, o.installService(f, id), "the function's dependencies"); err != nil {
			drop()
			return out, err
		}
	}
	if err := rootOwned(r).Symlink(rel, "current"); err != nil {
		drop()
		return out, err
	}
	return out, prune(r, id)
}

// releases are a function's releases, newest first.
func releases(r *sitefs.Root) []string {
	entries, err := r.ReadDir("releases")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && site.ValidReleaseID(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Slice(out, func(i, j int) bool { return site.ReleaseBefore(out[j], out[i]) })
	return out
}

// prune keeps the newest KeepReleases, and the live one.
func prune(r *sitefs.Root, liveID string) error {
	for i, id := range releases(r) {
		if i < fn.KeepReleases || id == liveID {
			continue
		}
		if err := rootOwned(r).RemoveAll("releases/" + id); err != nil {
			return err
		}
	}
	return nil
}

var currentRe = regexp.MustCompile(`^releases/([0-9A-Za-z.-]+)$`)

// live is the release current names; it must be a real directory.
func live(r *sitefs.Root) (string, error) {
	target, err := r.Readlink("current")
	if err != nil {
		return "", err
	}
	m := currentRe.FindStringSubmatch(target)
	if m == nil || !site.ValidReleaseID(m[1]) {
		return "", fmt.Errorf("current leads nowhere a release can be")
	}
	fi, err := r.Lstat("releases/" + m[1])
	if err != nil || !fi.IsDir() {
		return "", fmt.Errorf("the live release is missing")
	}
	return m[1], nil
}

func result(r *sitefs.Root) (rpc.FunctionResult, error) {
	id, err := live(r)
	if err != nil {
		return rpc.FunctionResult{}, err
	}
	return rpc.FunctionResult{Release: id, Releases: releases(r)}, nil
}

// writeShims writes the two shims when they differ.
func (o *Ops) writeShims() error {
	dir := filepath.Join(o.dir(), "shim")
	for _, d := range []string{o.dir(), dir} {
		if err := rootfile.MkdirAllMode(d, 0o755); err != nil {
			return err
		}
		if err := os.Chmod(d, 0o755); err != nil {
			return err
		}
	}
	for name, content := range map[string]string{"fn.mjs": fn.ShimNode, "fn.py": fn.ShimPython} {
		p := filepath.Join(dir, name)
		if old, err := rootfile.ReadNoFollow(p); err == nil && string(old) == content {
			continue
		}
		if err := rootfile.Replace(p, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// writeUnits writes the parent slice, the socket and the service template
// and has systemd take them; a failure puts the old files back.
func (o *Ops) writeUnits(ctx context.Context, f state.Function) error {
	socket := fn.SocketUnit(f.ID)
	files := map[string]string{
		filepath.Join(o.SystemdDir, fn.ParentSlice):           fn.RenderParentSlice(),
		filepath.Join(o.SystemdDir, socket):                   fn.RenderSocket(f),
		filepath.Join(o.SystemdDir, fn.ServiceTemplate(f.ID)): fn.RenderService(f),
	}
	prior := map[string][]byte{}
	changed, socketChanged := false, false
	for p, content := range files {
		old, err := rootfile.ReadNoFollow(p)
		if err == nil {
			prior[p] = old
		}
		if err == nil && string(old) == content {
			continue
		}
		if err := rootfile.Replace(p, []byte(content), 0o644); err != nil {
			return err
		}
		changed = true
		socketChanged = socketChanged || filepath.Base(p) == socket
	}
	restore := func(cause error) error {
		for p := range files {
			if old, ok := prior[p]; ok {
				_ = rootfile.Replace(p, old, 0o644)
			} else {
				_ = os.Remove(p)
			}
		}
		return errors.Join(cause, o.Systemd.DaemonReload(context.WithoutCancel(ctx)))
	}
	if changed {
		if err := o.Systemd.DaemonReload(ctx); err != nil {
			return restore(err)
		}
	}
	if err := o.Systemd.Enable(ctx, socket); err != nil {
		return restore(err)
	}
	start := o.Systemd.Start
	if socketChanged {
		start = o.Systemd.Restart
	}
	if err := start(ctx, socket); err != nil {
		return restore(err)
	}
	return nil
}

// Delete is DeleteFunction (SPEC §3): every step, whatever failed before.
func (o *Ops) Delete(ctx context.Context, id int64, name string) error {
	if !fn.ValidName(name) || id <= 0 {
		return agentrpc.Invalid("invalid function %q", name)
	}
	defer o.lock(name)()
	errs := []error{o.removeUnits(ctx, id)}
	if err := o.Systemd.Stop(ctx, fn.Slice(id)); err != nil && !errors.Is(err, host.ErrNoSuchUnit) {
		errs = append(errs, err)
	}
	if top, err := sitefs.Open(o.root(), sitefs.Owner{}); err == nil {
		errs = append(errs, top.RemoveAll(name))
		top.Close()
	} else if !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, err)
	}
	for _, p := range []string{o.accessLog(name), o.outputLog(name)} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	errs = append(errs, o.deleteUser(ctx, fn.User(id)), o.deleteCert(ctx, fn.FunctionLineage(id)))
	return errors.Join(errs...)
}

// removeUnits stops and removes a function's units and environment file.
func (o *Ops) removeUnits(ctx context.Context, id int64) error {
	socket := fn.SocketUnit(id)
	return o.removeUnitFiles(ctx, socket, fn.ServiceTemplate(id), fn.UnitBase(id)+"@*.service", o.envPath(id))
}

// removeUnitFiles stops a socket and its instances and removes the two unit
// files and the environment file.
func (o *Ops) removeUnitFiles(ctx context.Context, socket, template, instances, env string) error {
	var errs []error
	sockPath := filepath.Join(o.SystemdDir, socket)
	if _, err := os.Lstat(sockPath); err == nil {
		if err := o.Systemd.Disable(ctx, socket); err != nil && !errors.Is(err, host.ErrNoSuchUnit) {
			errs = append(errs, err)
		}
		if err := o.Systemd.Stop(ctx, socket); err != nil && !errors.Is(err, host.ErrNoSuchUnit) {
			errs = append(errs, err)
		}
	}
	if units, err := o.Systemd.ListUnits(ctx, instances); err == nil {
		for _, u := range units {
			if err := o.Systemd.Stop(ctx, u.Name); err != nil && !errors.Is(err, host.ErrNoSuchUnit) {
				errs = append(errs, err)
			}
		}
	}
	removed := false
	for _, p := range []string{sockPath, filepath.Join(o.SystemdDir, template)} {
		err := os.Remove(p)
		switch {
		case err == nil:
			removed = true
		case !errors.Is(err, fs.ErrNotExist):
			errs = append(errs, err)
		}
	}
	if err := os.Remove(env); err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, err)
	}
	if removed {
		if err := o.Systemd.DaemonReload(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// deleteUser removes a function's user and group, its processes ended
// first (as a site user's, ops/sites SPEC §6.8).
func (o *Ops) deleteUser(ctx context.Context, name string) error {
	if !fn.ValidUser(name) {
		return nil
	}
	exists, err := o.Accounts.UserExists(ctx, name)
	if err != nil || !exists {
		return err
	}
	if err := o.Accounts.ExpireUser(ctx, name, true); err != nil && !errors.Is(err, host.ErrNoSuchUser) {
		return err
	}
	if o.KillUser != nil {
		if err := o.KillUser(ctx, name); err != nil {
			return fmt.Errorf("end the processes of %s: %w", name, err)
		}
	}
	if err := o.Accounts.DeleteUser(ctx, name, false); err != nil && !errors.Is(err, host.ErrNoSuchUser) {
		return err
	}
	if err := o.Accounts.DeleteGroup(ctx, name); err != nil && !errors.Is(err, host.ErrNoSuchGroup) {
		return fmt.Errorf("remove the group %s: %w", name, err)
	}
	return nil
}

// RemoveLegacySite removes every v0.8.x function unit of a site's user and
// their environment files (ops/sites SPEC §6: a site deletion).
func (o *Ops) RemoveLegacySite(ctx context.Context, user string) error {
	if !site.ValidSystemUser(user) {
		return nil
	}
	var errs []error
	sockets, _ := filepath.Glob(filepath.Join(o.SystemdDir, fn.LegacyUnitBase(user, "")+"*.socket"))
	for _, p := range sockets {
		name := filepath.Base(p)
		base := name[:len(name)-len(".socket")]
		errs = append(errs, o.removeUnitFiles(ctx, name, base+"@.service", base+"@*.service", filepath.Join(o.dir(), base[len("cloudground-fn-"):]+".env")))
	}
	envs, _ := filepath.Glob(filepath.Join(o.dir(), user+"-*.env"))
	for _, p := range envs {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Code is FunctionCode (SPEC §4).
func (o *Ops) Code(_ context.Context, p rpc.FunctionRef) (rpc.FunctionCodeResult, error) {
	r, err := o.openExisting(p.Name)
	if err != nil {
		return rpc.FunctionCodeResult{}, agentrpc.NotFound("function %s has no code: %v", p.Name, err)
	}
	defer r.Close()
	id, err := live(r)
	if err != nil {
		return rpc.FunctionCodeResult{}, agentrpc.NotFound("function %s has no code: %v", p.Name, err)
	}
	rel := "releases/" + id + "/"
	var paths []string
	if b, err := r.ReadFile(rel+fn.ManifestFile, 64<<10); err == nil {
		_ = json.Unmarshal(b, &paths)
	} else {
		paths = []string{"index.js", "main.py"} // a release from before v0.9.0
	}
	files := map[string]string{}
	total := 0
	for _, f := range paths {
		if !fn.ValidPath(f) || len(files) >= fn.MaxFiles {
			continue
		}
		b, err := r.ReadFile(rel+f, fn.MaxCode)
		if err != nil {
			continue
		}
		if total += len(b); total > fn.MaxCode {
			break
		}
		files[f] = string(b)
	}
	return rpc.FunctionCodeResult{Release: id, Files: files, Releases: releases(r)}, nil
}

// Rollback is RollbackFunction (SPEC §4).
func (o *Ops) Rollback(_ context.Context, p rpc.FunctionRollbackParams) (rpc.FunctionResult, error) {
	defer o.lock(p.Name)()
	r, err := o.openExisting(p.Name)
	if err != nil {
		return rpc.FunctionResult{}, agentrpc.NotFound("function %s has no code", p.Name)
	}
	defer r.Close()
	gid := r.Owner().GID
	if u, err := o.owner(fn.User(p.ID)); err == nil {
		gid = u.GID
	}
	r = r.WithOwner(sitefs.Owner{UID: 0, GID: gid})
	cur, err := live(r)
	if err != nil {
		return rpc.FunctionResult{}, agentrpc.NotFound("function %s has no code", p.Name)
	}
	all := releases(r)
	target := p.Release
	if target == "" {
		i := slices.Index(all, cur)
		if i < 0 || i+1 >= len(all) {
			return rpc.FunctionResult{}, agentrpc.Conflict("no release older than %s", cur)
		}
		target = all[i+1]
	} else if !slices.Contains(all, target) {
		return rpc.FunctionResult{}, agentrpc.NotFound("release %s not found", target)
	}
	if err := r.Symlink("releases/"+target, "current"); err != nil {
		return rpc.FunctionResult{}, err
	}
	return result(r)
}
