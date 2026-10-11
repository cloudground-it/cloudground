package sites

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/cron"
	"github.com/cloudground-it/cloudground/internal/agent/ops/sftp"
	"github.com/cloudground-it/cloudground/internal/agent/ops/wp"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/engine/nginx"
	"github.com/cloudground-it/cloudground/internal/phpfpm"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// fakeDBs is an in-memory database server with the agent's record of whose
// each database is.
type fakeDBs struct {
	mu        sync.Mutex
	dbs       map[string]string // name -> user
	owner     map[string]int64  // name -> site
	createErr error
}

func (f *fakeDBs) ProvisionSite(_ context.Context, s state.Site, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return f.createErr
	}
	f.dbs[s.Config.Database.Name] = s.Config.Database.User
	if f.owner == nil {
		f.owner = map[string]int64{}
	}
	f.owner[s.Config.Database.Name] = s.ID
	return nil
}

func (f *fakeDBs) DropSite(_ context.Context, siteID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for name, id := range f.owner {
		if id == siteID {
			delete(f.dbs, name)
			delete(f.owner, name)
		}
	}
	return nil
}

func (f *fakeDBs) Exists(_ context.Context, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.dbs[name]
	return ok, nil
}

func (f *fakeDBs) has(name string) bool { ok, _ := f.Exists(context.Background(), name); return ok }

// fakeCerts records the domains whose certificates were removed.
type fakeCerts struct {
	mu      sync.Mutex
	removed []string
}

func (f *fakeCerts) RemoveSite(_ context.Context, domain string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, domain)
	return nil
}

// fakeWP answers wp-cli and records every invocation.
type fakeWP struct {
	mu    sync.Mutex
	calls []host.Invocation
	// fail makes the invocation whose arguments start with it fail.
	fail string
	// onRun runs for a successful invocation (wp-cli's side effects).
	onRun func(inv host.Invocation)
}

func (f *fakeWP) Run(_ context.Context, inv host.Invocation) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, inv)
	fail, on := f.fail, f.onRun
	f.mu.Unlock()
	if fail != "" && strings.HasPrefix(strings.Join(inv.Args, " "), fail) {
		return nil, &host.ExitError{Name: "runuser", Code: 1}
	}
	if on != nil {
		on(inv)
	}
	return nil, nil
}

func (f *fakeWP) lines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, strings.Join(c.Args, " "))
	}
	return out
}

// env is an Ops over fakes, with every path in a temporary directory.
type env struct {
	*Ops
	root     string
	exec     *host.FakeExec
	systemd  *host.FakeSystemd
	accounts *host.FakeAccounts
	nginx    *host.FakeNginx
	dbs      *fakeDBs
	certs    *fakeCerts
	wpcli    *fakeWP
	killedMu sync.Mutex
	killed   []string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	dir := func(parts ...string) string {
		p := filepath.Join(append([]string{root}, parts...)...)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	binDir := dir("bin")
	if err := os.WriteFile(filepath.Join(binDir, "panel-agent"), []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	paths := Paths{
		SitesRoot: dir("srv"), CacheRoot: dir("cache"), LogRoot: dir("logs"), ACMEWebroot: dir("acme"),
		FallbackCert: filepath.Join(root, "certs", "fallback.pem"), FallbackKey: filepath.Join(root, "certs", "fallback.key"),
		CertLiveDir: dir("letsencrypt"), CustomCertDir: dir("custom-certs"),
		NginxSites: dir("nginx", "sites-enabled"), NginxConfDir: dir("nginx", "conf.d"), NginxAuthDir: dir("nginx-auth"),
		PHPBinDir: dir("sbin"), PHPSocketDir: dir("phpsock"), PHPMasterDir: dir("php-masters"), SystemdDir: dir("systemd"),
	}
	e := &env{
		root:     root,
		exec:     &host.FakeExec{},
		systemd:  host.NewFakeSystemd(),
		accounts: host.NewFakeAccounts(),
		nginx:    &host.FakeNginx{Build: "nginx version: nginx/1.24.0\nconfigure arguments: --with-http_ssl_module"},
		dbs:      &fakeDBs{dbs: map[string]string{}},
		certs:    &fakeCerts{},
		wpcli:    &fakeWP{},
	}
	e.systemd.UnitDir = paths.SystemdDir
	ctx := context.Background()
	if err := e.accounts.CreateUser(ctx, host.UserSpec{Name: "www-data", System: true}); err != nil {
		t.Fatal(err)
	}
	owner := func(string) (sitefs.Owner, error) { return sitefs.Owner{UID: os.Getuid(), GID: os.Getgid()}, nil }
	open := func(s state.Site) (*sitefs.Root, error) {
		o, _ := owner(s.SystemUser)
		return sitefs.Open(s.RootPath, o)
	}
	e.Ops = &Ops{
		Paths:    paths,
		Renderer: nginx.Renderer{SitesDir: paths.NginxSites, ConfDir: paths.NginxConfDir},
		Accounts: e.accounts,
		Systemd:  e.systemd,
		Nginx:    e.nginx,
		Exec:     e.exec,
		Open:     open,
		Owner:    owner,
		DB:       e.dbs,
		SFTP:     &sftp.Ops{Accounts: e.accounts},
		Certs:    e.certs,
		Cron: &cron.Ops{Exec: e.exec, Systemd: e.systemd, CrontabDir: dir("crontabs"),
			CronDir: dir("cron.d"), SpecDir: dir("cron-spec"), BinDir: binDir},
		NginxMu: &sync.Mutex{},
		// A system service on 7000, none of this machine's addresses, no DNS.
		SystemPorts: func() []int { return []int{7000} },
		LocalAddrs:  func() []netip.Addr { return nil },
		Resolve:     func(string) ([]netip.Addr, error) { return nil, fmt.Errorf("no DNS in tests") },
		KillUser: func(_ context.Context, user string) error {
			// Deletes run concurrently in the storm tests.
			e.killedMu.Lock()
			defer e.killedMu.Unlock()
			e.killed = append(e.killed, user)
			return nil
		},
	}
	e.WordPress = &wp.Ops{WP: e.wpcli, OpenSite: open, PHP: phpControl{e.Ops}}
	return e
}

// phpControl is the site's master as ops/wp sees it.
type phpControl struct{ o *Ops }

func (p phpControl) Reload(ctx context.Context, s state.Site) error  { return p.o.ReloadPHP(ctx, s) }
func (p phpControl) Restart(ctx context.Context, s state.Site) error { return p.o.RestartPHP(ctx, s) }

// installPHP makes the FPM binaries of versions exist.
func (e *env) installPHP(t *testing.T, versions ...string) {
	t.Helper()
	for _, v := range versions {
		if err := os.WriteFile(filepath.Join(e.Paths.PHPBinDir, "php-fpm"+v), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func (e *env) site(id int64, domain string, typ state.SiteType) state.Site {
	s := state.Site{
		ID: id, Domain: domain, Type: typ, Profile: state.ProfileBalanced, Engine: state.EngineNginx,
		SystemUser: fmt.Sprintf("cg-site-%d", id), RootPath: filepath.Join(e.Paths.SitesRoot, domain),
		Status: state.SiteProvisioning,
	}
	if typ != state.SiteStatic && typ != state.SiteProxy {
		s.PHPVersion = "8.4"
		s.Config.Resources.MemoryHighMB = 256
	}
	if typ == state.SiteProxy {
		s.Config.ProxyUpstream = "http://127.0.0.1:3000"
	}
	if typ == state.SiteWordPress || typ == state.SiteWooCommerce {
		s.Config.Database = state.DatabaseConfig{Enabled: true, Name: fmt.Sprintf("wp_%d", id), User: fmt.Sprintf("wp_%d", id), Host: "localhost", Port: 3306}
	}
	return s
}

func (e *env) create(t *testing.T, s state.Site) rpc.CreateSiteResult {
	t.Helper()
	res, err := e.Create(context.Background(), rpc.CreateSiteParams{Site: s, DBPassword: strings.Repeat("p", 20)})
	if err != nil {
		t.Fatalf("create %s: %v", s.Domain, err)
	}
	return res
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

// leftovers lists what of s is still on the machine.
func (e *env) leftovers(s state.Site) []string {
	var out []string
	for _, p := range []string{s.RootPath, e.vhostPath(s), e.cacheZonePath(s), e.poolPath(s), e.unitPath(s), e.dropinDir(s),
		filepath.Join(e.Paths.LogRoot, s.Domain)} {
		if exists(p) {
			out = append(out, p)
		}
	}
	if _, ok := e.accounts.User(s.SystemUser); ok {
		out = append(out, "user "+s.SystemUser)
	}
	if e.accounts.HasGroup(s.SystemUser) {
		out = append(out, "group "+s.SystemUser)
	}
	if e.dbs.has(s.Config.Database.Name) {
		out = append(out, "database "+s.Config.Database.Name)
	}
	if u, ok := e.systemd.Unit(phpfpm.UnitName(s.SystemUser)); ok && u.ActiveState == "active" {
		out = append(out, "running "+u.Name)
	}
	return out
}
