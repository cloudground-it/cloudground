package sites

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/runtimes"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Managed Node.js and Python apps (#28, SPEC §11): the app's unit instead
// of a PHP master, its environment file, its health.

func isApp(s state.Site) bool { return s.Type.IsApp() }

func (o *Ops) appUnitPath(s state.Site) string {
	return filepath.Join(o.Paths.SystemdDir, runtimes.UnitName(s.SystemUser))
}

func (o *Ops) appEnvDir() string {
	if o.Paths.AppEnvDir != "" {
		return o.Paths.AppEnvDir
	}
	return runtimes.EnvDir
}

func (o *Ops) appEnvPath(s state.Site) string {
	return filepath.Join(o.appEnvDir(), s.SystemUser+".env")
}

// appStarter is the first release's app (SPEC §11.1): it answers every path
// with a page naming the site and the runtime.
func appStarter(s state.Site) map[string]string {
	name := strings.NewReplacer("<", "", ">", "", "&", "", `"`, "", "'", "").Replace(s.Domain)
	if s.Type == state.SiteNode {
		return map[string]string{"server.js": `// Written by CloudGround: deploy your app to replace it.
const http = require("http");
const server = http.createServer((req, res) => {
  res.writeHead(200, { "Content-Type": "text/html; charset=utf-8" });
  res.end("<!doctype html><title>` + name + `</title><h1>` + name + `</h1><p>This site is ready on CloudGround. It runs Node.js " + process.version + ". Deploy your app to replace this page.</p>\n");
});
server.listen(process.env.PORT);
`}
	}
	return map[string]string{
		"app.py": `# Written by CloudGround: deploy your app to replace it.
import sys


def app(environ, start_response):
    body = ("<!doctype html><title>` + name + `</title><h1>` + name + `</h1>"
            "<p>This site is ready on CloudGround. It runs Python %s. "
            "Deploy your app to replace this page.</p>\n" % sys.version.split()[0]).encode()
    start_response("200 OK", [("Content-Type", "text/html; charset=utf-8"), ("Content-Length", str(len(body)))])
    return [body]
`,
		"requirements.txt": "gunicorn\n",
	}
}

// bootstrapApp writes the starter app into the first release and installs
// its dependencies.
func (o *Ops) bootstrapApp(ctx context.Context, r *sitefs.Root, s state.Site) error {
	for name, content := range appStarter(s) {
		if err := r.WriteFile(firstRelease+"/"+name, []byte(content), 0o644); err != nil {
			return err
		}
	}
	if o.PrepareApp == nil {
		return nil
	}
	_, err := o.PrepareApp(ctx, s, filepath.Join(s.RootPath, firstRelease))
	return err
}

// renderApp writes the app's unit and (re)starts it (SPEC §11.1).
func (o *Ops) renderApp(ctx context.Context, s state.Site, tx *configTx, onlyIfChanged bool) error {
	name, unit, err := runtimes.RenderUnit(s)
	if err != nil {
		return err
	}
	changed, err := tx.write(o.appUnitPath(s), unit, 0o644)
	if err != nil {
		return err
	}
	if changed {
		if err := o.Systemd.DaemonReload(ctx); err != nil {
			return err
		}
	}
	if err := o.Systemd.Enable(ctx, name); err != nil {
		return err
	}
	if onlyIfChanged && !changed {
		return o.Systemd.Start(ctx, name)
	}
	return o.Systemd.Restart(ctx, name)
}

// rollbackApp puts the unit back after a failed render.
func (o *Ops) rollbackApp(ctx context.Context, s state.Site, tx *configTx) error {
	errs := []error{tx.restore(), o.Systemd.DaemonReload(ctx)}
	name := runtimes.UnitName(s.SystemUser)
	if tx.existed(o.appUnitPath(s)) {
		errs = append(errs, o.Systemd.Restart(ctx, name))
	} else if err := o.Systemd.Stop(ctx, name); err != nil && !errors.Is(err, host.ErrNoSuchUnit) {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// removeApp stops and removes the app's unit and environment file.
func (o *Ops) removeApp(ctx context.Context, s state.Site) error {
	name := runtimes.UnitName(s.SystemUser)
	var errs []error
	if _, err := os.Stat(o.appUnitPath(s)); err == nil {
		for _, f := range []func(context.Context, ...string) error{o.Systemd.Disable} {
			if err := f(ctx, name); err != nil && !errors.Is(err, host.ErrNoSuchUnit) {
				errs = append(errs, err)
			}
		}
		if err := o.Systemd.Stop(ctx, name); err != nil && !errors.Is(err, host.ErrNoSuchUnit) {
			errs = append(errs, err)
		}
	}
	for _, p := range []string{o.appUnitPath(s), o.appEnvPath(s)} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	if err := o.Systemd.ResetFailed(ctx, name); err != nil && !errors.Is(err, host.ErrNoSuchUnit) {
		o.log().Warn("resetting the app's failed state", "unit", name, "err", err)
	}
	return errors.Join(errs...)
}

// restartApp restarts the app on what current is now; nothing when its
// unit is not there yet.
func (o *Ops) restartApp(ctx context.Context, s state.Site) error {
	if !site.ValidSystemUser(s.SystemUser) {
		return nil
	}
	if _, err := os.Stat(o.appUnitPath(s)); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return o.Systemd.Restart(ctx, runtimes.UnitName(s.SystemUser))
}

// SetAppEnv writes the app's environment file (SPEC §11.2).
func (o *Ops) SetAppEnv(ctx context.Context, p rpc.AppEnvParams) (agentrpc.None, error) {
	s := p.Site
	if !isApp(s) {
		return agentrpc.None{}, agentrpc.Invalid("not a Node.js or Python site")
	}
	if err := runtimes.CheckEnv(p.Env); err != nil {
		return agentrpc.None{}, agentrpc.Invalid("%s", err.Error())
	}
	if err := os.MkdirAll(o.appEnvDir(), 0o700); err != nil {
		return agentrpc.None{}, err
	}
	if err := os.Chmod(o.appEnvDir(), 0o700); err != nil {
		return agentrpc.None{}, err
	}
	content := runtimes.RenderEnv(p.Env)
	old, _ := os.ReadFile(o.appEnvPath(s))
	if string(old) == content {
		return agentrpc.None{}, nil
	}
	if err := writeFile(o.appEnvPath(s), []byte(content), 0o600); err != nil {
		return agentrpc.None{}, err
	}
	return agentrpc.None{}, o.restartApp(ctx, s)
}

// AppStatus is the unit's state and the health probe (SPEC §11.3).
func (o *Ops) AppStatus(ctx context.Context, p rpc.SiteRef) (rpc.AppStatusResult, error) {
	s := p.Site
	if !isApp(s) {
		return rpc.AppStatusResult{}, agentrpc.Invalid("not a Node.js or Python site")
	}
	name := runtimes.UnitName(s.SystemUser)
	props, err := o.Systemd.UnitProps(ctx, name)
	if err != nil {
		return rpc.AppStatusResult{}, err
	}
	res := rpc.AppStatusResult{Active: props.ActiveState == "active", State: props.ActiveState + "/" + props.SubState}
	if !props.Exists() {
		res.State = "not-found"
	}
	if !props.ActiveEnter.IsZero() && res.Active {
		t := props.ActiveEnter.UTC()
		res.Since = &t
	}
	if o.Exec != nil {
		if r, err := o.Exec.Run(ctx, host.Cmd{Name: "systemctl", Args: []string{"show", "--property=NRestarts", "--property=ExecMainStatus", name}}); err == nil {
			for _, line := range strings.Split(string(r.Stdout), "\n") {
				k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
				n, _ := strconv.Atoi(v)
				switch k {
				case "NRestarts":
					res.Restarts = n
				case "ExecMainStatus":
					res.LastExit = n
				}
			}
		}
	}
	if res.Active {
		res.Health = o.probe(ctx, s)
	}
	return res, nil
}

// probe asks the app for its health path, the way nginx reaches it.
func (o *Ops) probe(ctx context.Context, s state.Site) rpc.AppHealth {
	path := s.Config.App.HealthPath
	if path == "" {
		path = "/"
	}
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		if s.Config.App.Port != 0 {
			return d.DialContext(ctx, "tcp", "127.0.0.1:"+strconv.Itoa(s.Config.App.Port))
		}
		return d.DialContext(ctx, "unix", o.appSocket(s))
	}
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DialContext: dial, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+s.Domain+path, nil)
	if err != nil {
		return rpc.AppHealth{Error: err.Error()}
	}
	req.Header.Set("User-Agent", "CloudGround-Health")
	req.Header.Set("X-Forwarded-Proto", "https")
	start := time.Now()
	resp, err := c.Do(req)
	h := rpc.AppHealth{MS: time.Since(start).Milliseconds()}
	if err != nil {
		h.Error = probeError(err)
		return h
	}
	resp.Body.Close()
	h.Status = resp.StatusCode
	return h
}

func probeError(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "connection refused"), strings.Contains(msg, "no such file"):
		return "the app is not listening"
	case strings.Contains(msg, "Timeout"), strings.Contains(msg, "deadline"):
		return "no answer in 5 seconds"
	}
	return msg
}

// appSocket is the socket the probe dials; tests move it.
func (o *Ops) appSocket(s state.Site) string {
	if o.AppSocket != nil {
		return o.AppSocket(s)
	}
	return runtimes.SocketPath(s.SystemUser)
}

// AppControl starts, stops or restarts the app (SPEC §11.3).
func (o *Ops) AppControl(ctx context.Context, p rpc.AppControlParams) (rpc.AppStatusResult, error) {
	s := p.Site
	if !isApp(s) {
		return rpc.AppStatusResult{}, agentrpc.Invalid("not a Node.js or Python site")
	}
	name := runtimes.UnitName(s.SystemUser)
	var err error
	switch p.Action {
	case "start":
		err = o.Systemd.Start(ctx, name)
	case "stop":
		err = o.Systemd.Stop(ctx, name)
	case "restart":
		err = o.Systemd.Restart(ctx, name)
	default:
		return rpc.AppStatusResult{}, agentrpc.Invalid("unknown action %q", p.Action)
	}
	if err != nil {
		return rpc.AppStatusResult{}, fmt.Errorf("%s the app: %w", p.Action, err)
	}
	if p.Action != "stop" {
		// A moment for the app to bind its socket before the probe.
		o.waitListening(ctx, s, 10*time.Second)
	}
	return o.AppStatus(ctx, rpc.SiteRef{Site: s})
}

// waitListening waits until the app accepts a connection, at most max.
func (o *Ops) waitListening(ctx context.Context, s state.Site, max time.Duration) {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		network, addr := "unix", o.appSocket(s)
		if s.Config.App.Port != 0 {
			network, addr = "tcp", "127.0.0.1:"+strconv.Itoa(s.Config.App.Port)
		}
		if c, err := net.DialTimeout(network, addr, time.Second); err == nil {
			c.Close()
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}
