// Package functions sets a site's functions up on the server, keeps their
// code in releases and invokes them (#143, SPEC.md).
package functions

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	fn "github.com/cloudground-it/cloudground/internal/functions"
	"github.com/cloudground-it/cloudground/internal/phpfpm"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/runtimes"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Ops holds what the handlers use.
type Ops struct {
	Systemd host.Systemd
	// Open opens a site's tree; what it creates belongs to the site user.
	Open func(state.Site) (*sitefs.Root, error)
	// SystemdDir holds the units; Dir the environment files and shims
	// ("" is functions.Dir); LogRoot the sites' logs.
	SystemdDir, Dir, LogRoot string
	// Installed reports whether a runtime version is on the server.
	Installed func(runtime, version string) bool
	// NginxSocket is where an invocation reaches nginx ("" is
	// fn.InvokeSocket).
	NginxSocket string
	Now         func() time.Time
	Log         *slog.Logger
}

// Register wires the methods into the agent's RPC server.
func (o *Ops) Register(s *agentrpc.Server) {
	agentrpc.Handle(s, agentrpc.SetFunction, o.Set)
	agentrpc.Handle(s, agentrpc.DeleteFunction, func(ctx context.Context, p rpc.FunctionRef) (agentrpc.None, error) {
		return agentrpc.None{}, o.Delete(ctx, p.Site, p.Name)
	})
	agentrpc.Handle(s, agentrpc.FunctionCode, o.Code)
	agentrpc.Handle(s, agentrpc.FunctionRollback, o.Rollback)
	agentrpc.Handle(s, agentrpc.InvokeFunction, o.Invoke)
	agentrpc.Handle(s, agentrpc.FunctionLogs, o.Logs)
}

func (o *Ops) dir() string {
	if o.Dir != "" {
		return o.Dir
	}
	return fn.Dir
}

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

func (o *Ops) envPath(user, name string) string { return filepath.Join(o.dir(), user+"-"+name+".env") }

// rootOwned is r creating entries for root with the site's group.
func rootOwned(r *sitefs.Root) *sitefs.Root {
	return r.WithOwner(sitefs.Owner{UID: 0, GID: r.Owner().GID})
}

// Set is SetFunction (SPEC §2).
func (o *Ops) Set(ctx context.Context, p rpc.SetFunctionParams) (rpc.FunctionResult, error) {
	s, f := p.Site, p.Function
	if err := p.Validate(); err != nil {
		return rpc.FunctionResult{}, agentrpc.Invalid("%s", err.Error())
	}
	if !site.ValidSystemUser(s.SystemUser) {
		return rpc.FunctionResult{}, agentrpc.Invalid("invalid system user %q", s.SystemUser)
	}
	if o.Installed != nil && !o.Installed(f.Runtime, f.Version) {
		return rpc.FunctionResult{}, agentrpc.Invalid("%s %s is not installed on this server", runtimes.Label(f.Runtime), f.Version)
	}
	r, err := o.Open(s)
	if err != nil {
		return rpc.FunctionResult{}, err
	}
	defer r.Close()
	if _, err := live(r, f.Name); p.Code != "" || err != nil {
		code := p.Code
		if code == "" {
			code = fn.Starter(f.Runtime)
		}
		if err := o.newRelease(r, f, code); err != nil {
			return rpc.FunctionResult{}, err
		}
	}
	if err := o.writeShims(); err != nil {
		return rpc.FunctionResult{}, err
	}
	if err := writeFile(o.envPath(s.SystemUser, f.Name), []byte(runtimes.RenderEnv(p.Env)), 0o600); err != nil {
		return rpc.FunctionResult{}, err
	}
	if err := o.writeUnits(ctx, s, f); err != nil {
		return rpc.FunctionResult{}, err
	}
	return result(r, f.Name)
}

// newRelease writes code as a new release and moves current to it.
func (o *Ops) newRelease(r *sitefs.Root, f state.SiteFunction, code string) error {
	base := fn.CodeDir(f.Name)
	root := rootOwned(r)
	for _, d := range []string{"functions", base, base + "/releases"} {
		if err := root.MkdirAll(d, 0o750); err != nil {
			return err
		}
		if err := root.Chmod(d, 0o750); err != nil {
			return err
		}
	}
	id := site.NewReleaseID(o.now())
	dir := base + "/releases/" + id
	if err := r.Mkdir(dir, 0o750); err != nil {
		return err
	}
	if err := r.WriteFile(dir+"/"+fn.File(f.Runtime), []byte(code), 0o640); err != nil {
		return err
	}
	if err := root.Symlink("releases/"+id, base+"/current"); err != nil {
		return err
	}
	return prune(r, f.Name, id)
}

// releases are a function's releases, newest first.
func releases(r *sitefs.Root, name string) []string {
	entries, err := r.ReadDir(fn.CodeDir(name) + "/releases")
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
func prune(r *sitefs.Root, name, liveID string) error {
	all := releases(r, name)
	for i, id := range all {
		if i < fn.KeepReleases || id == liveID {
			continue
		}
		if err := rootOwned(r).RemoveAll(fn.CodeDir(name) + "/releases/" + id); err != nil {
			return err
		}
	}
	return nil
}

var currentRe = regexp.MustCompile(`^releases/([0-9A-Za-z.-]+)$`)

// live is the release current names; it must be a real directory.
func live(r *sitefs.Root, name string) (string, error) {
	target, err := r.Readlink(fn.CodeDir(name) + "/current")
	if err != nil {
		return "", err
	}
	m := currentRe.FindStringSubmatch(target)
	if m == nil || !site.ValidReleaseID(m[1]) {
		return "", fmt.Errorf("current leads nowhere a release can be")
	}
	fi, err := r.Lstat(fn.CodeDir(name) + "/releases/" + m[1])
	if err != nil || !fi.IsDir() {
		return "", fmt.Errorf("the live release is missing")
	}
	return m[1], nil
}

func result(r *sitefs.Root, name string) (rpc.FunctionResult, error) {
	id, err := live(r, name)
	if err != nil {
		return rpc.FunctionResult{}, err
	}
	return rpc.FunctionResult{Release: id, Releases: releases(r, name)}, nil
}

// writeShims writes the two shims when they differ (SPEC §2 step 3).
func (o *Ops) writeShims() error {
	dir := filepath.Join(o.dir(), "shim")
	for _, d := range []string{o.dir(), dir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
		if err := os.Chmod(d, 0o755); err != nil {
			return err
		}
	}
	for name, content := range map[string]string{"fn.mjs": fn.ShimNode, "fn.py": fn.ShimPython} {
		p := filepath.Join(dir, name)
		if old, err := os.ReadFile(p); err == nil && string(old) == content {
			continue
		}
		if err := writeFile(p, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// writeUnits writes the socket and the service template and has systemd
// take them; a failure puts the old files back.
func (o *Ops) writeUnits(ctx context.Context, s state.Site, f state.SiteFunction) error {
	files := map[string]string{
		filepath.Join(o.SystemdDir, fn.SocketUnit(s.SystemUser, f.Name)):      fn.RenderSocket(s, f),
		filepath.Join(o.SystemdDir, fn.ServiceTemplate(s.SystemUser, f.Name)): fn.RenderService(s, f),
	}
	prior := map[string][]byte{}
	changed := false
	for path, content := range files {
		old, err := os.ReadFile(path)
		if err == nil {
			prior[path] = old
		}
		if err == nil && string(old) == content {
			continue
		}
		if err := writeFile(path, []byte(content), 0o644); err != nil {
			return err
		}
		changed = true
	}
	socket := fn.SocketUnit(s.SystemUser, f.Name)
	restore := func(cause error) error {
		for path := range files {
			if old, ok := prior[path]; ok {
				_ = writeFile(path, old, 0o644)
			} else {
				_ = os.Remove(path)
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
	if changed {
		start = o.Systemd.Restart
	}
	if err := start(ctx, socket); err != nil {
		return restore(err)
	}
	return nil
}

// Delete is DeleteFunction (SPEC §3).
func (o *Ops) Delete(ctx context.Context, s state.Site, name string) error {
	if !fn.ValidName(name) || !site.ValidSystemUser(s.SystemUser) {
		return agentrpc.Invalid("invalid function %q", name)
	}
	errs := []error{o.removeUnits(ctx, s.SystemUser, fn.UnitBase(s.SystemUser, name))}
	if r, err := o.Open(s); err == nil {
		if err := rootOwned(r).RemoveAll(fn.CodeDir(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
		r.Close()
	}
	return errors.Join(errs...)
}

// RemoveSite removes every function of a site's user (ops/sites SPEC §11.1).
func (o *Ops) RemoveSite(ctx context.Context, user string) error {
	if !site.ValidSystemUser(user) {
		return nil
	}
	return o.removeUnits(ctx, user, fn.UnitBase(user, ""))
}

// removeUnits stops and removes the units whose names start with prefix,
// and their environment files.
func (o *Ops) removeUnits(ctx context.Context, user, prefix string) error {
	var errs []error
	sockets, _ := filepath.Glob(filepath.Join(o.SystemdDir, prefix+"*.socket"))
	for _, p := range sockets {
		unit := filepath.Base(p)
		for _, f := range []func(context.Context, ...string) error{o.Systemd.Disable} {
			if err := f(ctx, unit); err != nil && !errors.Is(err, host.ErrNoSuchUnit) {
				errs = append(errs, err)
			}
		}
		if err := o.Systemd.Stop(ctx, unit); err != nil && !errors.Is(err, host.ErrNoSuchUnit) {
			errs = append(errs, err)
		}
	}
	if units, err := o.Systemd.ListUnits(ctx, prefix+"*@*.service"); err == nil {
		for _, u := range units {
			if err := o.Systemd.Stop(ctx, u.Name); err != nil && !errors.Is(err, host.ErrNoSuchUnit) {
				errs = append(errs, err)
			}
		}
	}
	templates, _ := filepath.Glob(filepath.Join(o.SystemdDir, prefix+"*@.service"))
	removed := false
	for _, p := range append(sockets, templates...) {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		} else {
			removed = true
		}
	}
	envPrefix := strings.TrimPrefix(prefix, "cloudground-fn-")
	envs, _ := filepath.Glob(filepath.Join(o.dir(), envPrefix+"*.env"))
	for _, p := range envs {
		if strings.HasPrefix(filepath.Base(p), user+"-") {
			if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}
	if removed {
		if err := o.Systemd.DaemonReload(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Code is FunctionCode (SPEC §4).
func (o *Ops) Code(_ context.Context, p rpc.FunctionRef) (rpc.FunctionCodeResult, error) {
	r, err := o.Open(p.Site)
	if err != nil {
		return rpc.FunctionCodeResult{}, err
	}
	defer r.Close()
	id, err := live(r, p.Name)
	if err != nil {
		return rpc.FunctionCodeResult{}, agentrpc.NotFound("function %s has no code: %v", p.Name, err)
	}
	var code []byte
	for _, file := range []string{"index.js", "main.py"} {
		if b, err := r.ReadFile(fn.CodeDir(p.Name)+"/releases/"+id+"/"+file, fn.MaxCode); err == nil {
			code = b
			break
		}
	}
	return rpc.FunctionCodeResult{Release: id, Code: string(code), Releases: releases(r, p.Name)}, nil
}

// Rollback is FunctionRollback (SPEC §4).
func (o *Ops) Rollback(_ context.Context, p rpc.FunctionRollbackParams) (rpc.FunctionResult, error) {
	r, err := o.Open(p.Site)
	if err != nil {
		return rpc.FunctionResult{}, err
	}
	defer r.Close()
	cur, err := live(r, p.Name)
	if err != nil {
		return rpc.FunctionResult{}, agentrpc.NotFound("function %s has no code", p.Name)
	}
	all := releases(r, p.Name)
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
	if err := rootOwned(r).Symlink("releases/"+target, fn.CodeDir(p.Name)+"/current"); err != nil {
		return rpc.FunctionResult{}, err
	}
	return result(r, p.Name)
}

// Invoke is InvokeFunction (SPEC §5): through nginx, as a visitor's.
func (o *Ops) Invoke(ctx context.Context, p rpc.InvokeFunctionParams) (rpc.InvokeResult, error) {
	sock := o.NginxSocket
	if sock == "" {
		sock = fn.InvokeSocket
	}
	timeout := time.Duration(max(p.TimeoutSec, 1)+10) * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// The site's own 443 server, on the unix socket only root reaches
	// (internal/engine SPEC §4.13): no TLS on a hop that never leaves the
	// machine, and so no certificate to take on trust.
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
		DisableKeepAlives: true,
	}
	c := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+p.Site.Domain+"/.functions/"+p.Name, strings.NewReader(p.Body))
	if err != nil {
		return rpc.InvokeResult{}, err
	}
	req.Header.Set("User-Agent", "CloudGround-Functions/"+p.Source)
	req.Header.Set("X-CloudGround-Source", p.Source)
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := c.Do(req)
	res := rpc.InvokeResult{MS: time.Since(start).Milliseconds()}
	if err != nil {
		res.Error = err.Error()
		return res, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	res.Status, res.Body = resp.StatusCode, string(body)
	return res, nil
}

// accessLine is the cloudground log format, as far as an invocation needs.
var accessLine = regexp.MustCompile(`^\S+ - \S+ \[([^\]]+)\] "(\S+) (\S+) [^"]*" (\d{3}) \d+ ([0-9.]+) \S+ "([^"]*)"`)

// Logs is FunctionLogs (SPEC §6).
func (o *Ops) Logs(_ context.Context, p rpc.FunctionRef) (rpc.FunctionLogsResult, error) {
	res := rpc.FunctionLogsResult{Invocations: []rpc.Invocation{}, Output: []string{}}
	logRoot := o.LogRoot
	if logRoot == "" {
		logRoot = phpfpm.LogRoot
	}
	prefix := "/.functions/" + p.Name
	for _, line := range tailLines(filepath.Join(logRoot, p.Site.Domain, "access.log"), 2<<20, 0) {
		m := accessLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		path, _, _ := strings.Cut(m[3], "?")
		if path != prefix && !strings.HasPrefix(path, prefix+"/") {
			continue
		}
		status, _ := strconv.Atoi(m[4])
		secs, _ := strconv.ParseFloat(m[5], 64)
		src := "http"
		if rest, ok := strings.CutPrefix(m[6], "CloudGround-Functions/"); ok && (rest == "schedule" || rest == "test") {
			src = rest // the agent's own run (SPEC §5)
		}
		at := m[1]
		if t, err := time.Parse("02/Jan/2006:15:04:05 -0700", m[1]); err == nil {
			at = t.UTC().Format(time.RFC3339)
		}
		res.Invocations = append(res.Invocations, rpc.Invocation{Time: at, Method: m[2], Status: status, MS: int64(secs * 1000), Source: src})
	}
	slices.Reverse(res.Invocations)
	if len(res.Invocations) > 100 {
		res.Invocations = res.Invocations[:100]
	}
	res.Output = tailLines(filepath.Join(logRoot, p.Site.Domain, "fn-"+p.Name+".log"), 256<<10, 200)
	if res.Output == nil {
		res.Output = []string{}
	}
	return res, nil
}

// tailLines is a file's last lines within its last max bytes (at most n
// lines when n > 0).
func tailLines(path string, max int64, n int) []string {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return nil
	}
	off := max64(0, fi.Size()-max)
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(f, max))
	if off > 0 {
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:]
		}
	}
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	if n > 0 && len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// writeFile writes path atomically with mode.
func writeFile(path string, content []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
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
