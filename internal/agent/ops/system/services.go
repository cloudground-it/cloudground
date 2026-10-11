package system

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

// managed is every service the panel may show or restart, in display order.
var managed = []struct {
	name, unit string
	listed     bool     // a row on the services page
	version    []string // argv that prints the program's version
	// optional: the installer does not install it, so a server without its
	// unit shows no row (Redis: no longer installed, R-6.1; an upgraded
	// server keeps it, switched off).
	optional bool
}{
	{"nginx", "nginx", true, []string{"nginx", "-v"}, false},
	{"mariadb", "mariadb", true, []string{"mariadbd", "--version"}, false},
	{"redis", "redis-server", true, []string{"redis-server", "--version"}, true},
	{"cloudground-api", "cloudground-api", false, nil, false},
}

// Services is the managed services, in display order, for Capabilities:
// RestartService accepts every one of them, and the Listed ones are the
// rows of the services page.
func Services() []rpc.ServiceCapability {
	out := make([]rpc.ServiceCapability, 0, len(managed))
	for _, m := range managed {
		out = append(out, rpc.ServiceCapability{Name: m.name, Unit: m.unit, Listed: m.listed, Restartable: true})
	}
	return out
}

// sitePHPPattern matches every site's own PHP-FPM master.
const sitePHPPattern = "cloudground-php-*.service"

// restartTimeout bounds a restart job: MariaDB can take a while to come back.
const restartTimeout = 3 * time.Minute

// RestartService restarts one managed service and returns once systemd's job
// has finished.
func (o *Ops) RestartService(ctx context.Context, p rpc.ServiceParams) (map[string]string, error) {
	for _, m := range managed {
		if m.name != p.Name {
			continue
		}
		ctx, cancel := context.WithTimeout(ctx, restartTimeout)
		defer cancel()
		// A restart of a stopped unit starts it. A service switched off on
		// purpose (Redis until a site needs it) stays off: started, it
		// would listen on loopback for every site's PHP and shell.
		p, err := o.Systemd.UnitProps(ctx, m.unit)
		if err != nil {
			return nil, fmt.Errorf("read %s status: %w", m.unit, err)
		}
		// systemd knows no such unit: the package is not installed, and a
		// restart job would only fail (it answered 502).
		if !p.Exists() {
			return nil, agentrpc.Conflict("%s is not installed on this server", m.name)
		}
		if p.ActiveState != "active" && p.UnitFileState == "disabled" {
			return nil, agentrpc.Conflict("%s is switched off on this server; a restart would start it", m.name)
		}
		// Past the adapter's default ceiling (2 minutes) on purpose.
		if err := o.Systemd.Restart(host.WithJobTimeout(ctx, restartTimeout), m.unit); err != nil {
			return nil, err
		}
		return map[string]string{"restarted": m.unit}, nil
	}
	return nil, agentrpc.Invalid("unknown service %q", p.Name)
}

// ServiceStatus is the services page (SPEC.md §2): one row per managed
// service and one for the sites' PHP masters.
func (o *Ops) ServiceStatus(ctx context.Context) (rpc.ServiceStatusResult, error) {
	var res rpc.ServiceStatusResult
	for _, m := range managed {
		if !m.listed {
			continue
		}
		p, err := o.Systemd.UnitProps(ctx, m.unit)
		if err != nil {
			return rpc.ServiceStatusResult{}, fmt.Errorf("read %s status: %w", m.unit, err)
		}
		if m.optional && !p.Exists() {
			continue
		}
		row := rpc.ServiceInfo{
			Name:    m.name,
			Unit:    m.unit,
			Active:  p.ActiveState == "active",
			Enabled: p.UnitFileState == "enabled",
			Detail:  p.ActiveState,
		}
		row.State = stateOf(p)
		if row.Active {
			if p.MemoryKnown {
				row.MemoryBytes = int64(p.MemoryCurrent)
			}
			row.Since = p.ActiveEnter
		}
		row.Version = o.version(ctx, m.name, m.version, p.ActiveEnter)
		res.Services = append(res.Services, row)
	}
	php, err := o.sitePHP(ctx)
	if err != nil {
		return rpc.ServiceStatusResult{}, err
	}
	res.Services = append(res.Services, php)
	return res, nil
}

// stateOf tells a service switched off on purpose from one that should run.
func stateOf(p host.UnitProps) string {
	switch {
	case p.ActiveState == "active":
		return "running"
	case p.ActiveState == "failed":
		return "failed"
	case p.UnitFileState != "enabled":
		return "disabled"
	}
	return "stopped"
}

// sitePHP summarises the sites' PHP-FPM masters in one row. It only reads:
// a deleted site's unit that systemd still remembers is skipped, not reset.
func (o *Ops) sitePHP(ctx context.Context) (rpc.ServiceInfo, error) {
	units, err := o.Systemd.ListUnits(ctx, sitePHPPattern)
	if err != nil {
		return rpc.ServiceInfo{}, fmt.Errorf("list site PHP units: %w", err)
	}
	var up int
	var down []string
	for _, u := range units {
		switch {
		case u.LoadState == "not-found":
		case u.ActiveState == "active":
			up++
		default:
			user := strings.TrimSuffix(strings.TrimPrefix(u.Name, "cloudground-php-"), ".service")
			down = append(down, user+" "+u.ActiveState)
		}
	}
	row := rpc.ServiceInfo{
		Name: "site-php", Unit: "cloudground-php-*",
		State: "running", Active: true, Enabled: true,
		Detail: fmt.Sprintf("%d running", up),
	}
	if len(down) > 0 {
		row.State, row.Active = "failed", false
		row.Detail += fmt.Sprintf(", %d not running: %s", len(down), strings.Join(down, ", "))
	}
	return row, nil
}

// versionKey ties a cached version to the service's start: an upgrade
// restarts the service, and so asks again.
type versionKey struct {
	name    string
	started time.Time
}

var versionRe = regexp.MustCompile(`\d+\.\d+(\.\d+)?`)

// version asks the program for its version once per start of the service,
// failures included: a missing redis is not re-probed on every poll.
func (o *Ops) version(ctx context.Context, name string, argv []string, started time.Time) string {
	if len(argv) == 0 {
		return ""
	}
	key := versionKey{name, started}
	o.mu.Lock()
	v, ok := o.versions[key]
	o.mu.Unlock()
	if ok {
		return v
	}
	res, err := o.Exec.Run(ctx, host.Cmd{Name: argv[0], Args: argv[1:], Timeout: 10 * time.Second})
	if err == nil {
		// nginx -v writes to stderr.
		v = versionRe.FindString(string(res.Stdout) + string(res.Stderr))
	} else if ctx.Err() != nil {
		return "" // the caller gave up: try again next time
	}
	o.mu.Lock()
	if o.versions == nil {
		o.versions = map[versionKey]string{}
	}
	for k := range o.versions {
		if k.name == name {
			delete(o.versions, k)
		}
	}
	o.versions[key] = v
	o.mu.Unlock()
	return v
}
