package install

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// The fakes the steps run against (SPEC §1): a filesystem root in a
// temporary directory, a package manager, systemd, accounts, ownership, a
// command runner and a downloader, each recording what it was asked.

type fakeCmd struct {
	mu    sync.Mutex
	calls []string
	tools map[string]bool // LookPath answers
	// handle answers a command; nil, or ok=false, = success with no output.
	handle func(line string) (out string, err error, ok bool)
}

func (c *fakeCmd) Run(_ context.Context, name string, args ...string) (string, error) {
	line := strings.TrimSpace(name + " " + strings.Join(args, " "))
	c.mu.Lock()
	c.calls = append(c.calls, line)
	h := c.handle
	c.mu.Unlock()
	if h != nil {
		if out, err, ok := h(line); ok {
			return out, err
		}
	}
	return "", nil
}

func (c *fakeCmd) Quiet(ctx context.Context, name string, args ...string) (string, error) {
	return c.Run(ctx, name, args...)
}

func (c *fakeCmd) LookPath(name string) bool { return c.tools[name] }

func (c *fakeCmd) ran(prefix string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, l := range c.calls {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}

type fakePkg struct {
	root      string
	installed map[string]bool
	updates   int
	installs  [][]string
	ppas      []string
}

func (p *fakePkg) Missing(_ context.Context, pkgs []string) ([]string, error) {
	var m []string
	for _, x := range pkgs {
		if !p.installed[x] {
			m = append(m, x)
		}
	}
	return m, nil
}

func (p *fakePkg) Update(context.Context) error { p.updates++; return nil }

func (p *fakePkg) Install(_ context.Context, pkgs []string) error {
	p.installs = append(p.installs, slices.Clone(pkgs))
	for _, x := range pkgs {
		p.installed[x] = true
	}
	return nil
}

func (p *fakePkg) AddPPA(_ context.Context, ppa string) error {
	p.ppas = append(p.ppas, ppa)
	dir := filepath.Join(p.root, "etc/apt/sources.list.d")
	_ = os.MkdirAll(dir, 0o755)
	return os.WriteFile(filepath.Join(dir, "ondrej-ubuntu-php-noble.sources"), []byte("x"), 0o644)
}

type unitState struct{ exists, enabled, active bool }

type fakeSd struct {
	running bool
	units   map[string]*unitState
	calls   []string
	// stuck units never become active, whatever they are told (a unit
	// that fails at start and is restarted for ever).
	stuck map[string]bool
}

func (s *fakeSd) unit(name string) *unitState {
	name = strings.TrimSuffix(name, ".service")
	u := s.units[name]
	if u == nil {
		u = &unitState{}
		s.units[name] = u
	}
	return u
}

func (s *fakeSd) Running() bool { return s.running }
func (s *fakeSd) Exists(_ context.Context, u string) (bool, error) {
	return s.unit(u).exists, nil
}
func (s *fakeSd) Enabled(_ context.Context, u string) (bool, error) {
	return s.unit(u).enabled, nil
}
func (s *fakeSd) Active(_ context.Context, u string) (bool, error) {
	if s.stuck[strings.TrimSuffix(u, ".service")] {
		return false, nil
	}
	return s.unit(u).active, nil
}

func (s *fakeSd) Do(_ context.Context, args ...string) error {
	s.calls = append(s.calls, strings.Join(args, " "))
	if len(args) == 0 {
		return errors.New("no verb")
	}
	units := args[1:]
	now := false
	if len(units) > 0 && units[0] == "--now" {
		now, units = true, units[1:]
	}
	for _, u := range units {
		st := s.unit(u)
		switch args[0] {
		case "enable":
			st.exists, st.enabled = true, true
			st.active = st.active || now
		case "disable":
			st.enabled = false
			st.active = st.active && !now
		case "stop":
			st.active = false
		case "restart", "start":
			st.active = true
		}
	}
	return nil
}

func (s *fakeSd) did(call string) bool { return slices.Contains(s.calls, call) }

type fakeAcct struct {
	users  map[string][2]int
	groups map[string]int
	next   int
}

func (a *fakeAcct) User(n string) (int, int, bool) {
	u, ok := a.users[n]
	return u[0], u[1], ok
}
func (a *fakeAcct) Group(n string) (int, bool) { g, ok := a.groups[n]; return g, ok }
func (a *fakeAcct) AddSystemUser(_ context.Context, n string) error {
	a.next++
	a.users[n] = [2]int{a.next, a.next}
	a.groups[n] = a.next
	return nil
}
func (a *fakeAcct) AddSystemGroup(_ context.Context, n string) error {
	if _, ok := a.groups[n]; !ok {
		a.next++
		a.groups[n] = a.next
	}
	return nil
}

// fakeFS keeps ownership in a map: a test cannot chown without root. A file
// it was never told about is root's.
type fakeFS struct {
	mu     sync.Mutex
	owners map[string][2]int
}

func (f *fakeFS) Owner(path string) (int, int, error) {
	if _, err := os.Lstat(path); err != nil {
		return 0, 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.owners[path]
	return o[0], o[1], nil
}

func (f *fakeFS) Chown(path string, uid, gid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.owners[path]
	if uid >= 0 {
		o[0] = uid
	}
	if gid >= 0 {
		o[1] = gid
	}
	f.owners[path] = o
	return nil
}

func (f *fakeFS) Rename(from, to string) error {
	if err := os.Rename(from, to); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.owners[to] = f.owners[from]
	delete(f.owners, from)
	return nil
}

// owner is the recorded owner of a path inside the root.
func (f *fakeFS) owner(path string) [2]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.owners[path]
}

type fakeNet struct {
	files map[string][]byte
	gets  []string
}

func (n *fakeNet) Fetch(_ context.Context, url string) ([]byte, error) {
	n.gets = append(n.gets, url)
	b, ok := n.files[url]
	if !ok {
		return nil, fmt.Errorf("%s: HTTP 404", url)
	}
	return b, nil
}

// machine is a fake Ubuntu server.
type machine struct {
	t    *testing.T
	root string
	env  *Env
	cmd  *fakeCmd
	pkg  *fakePkg
	sd   *fakeSd
	acct *fakeAcct
	fs   *fakeFS
	net  *fakeNet

	quic      bool
	nginxOK   func() bool // nginx -t
	listening map[int]bool
	// healthErr is what /healthz fails with once both panel units run;
	// journal is each unit's journal (journalctl -u).
	healthErr error
	journal   map[string]string
}

func newMachine(t *testing.T, distro string) *machine {
	t.Helper()
	root := t.TempDir()
	m := &machine{t: t, root: root,
		cmd:       &fakeCmd{tools: map[string]bool{}},
		pkg:       &fakePkg{root: root, installed: map[string]bool{}},
		sd:        &fakeSd{running: true, units: map[string]*unitState{}, stuck: map[string]bool{}},
		acct:      &fakeAcct{users: map[string][2]int{"root": {0, 0}, "www-data": {33, 33}}, groups: map[string]int{"root": 0, "www-data": 33}, next: 900},
		fs:        &fakeFS{owners: map[string][2]int{}},
		net:       &fakeNet{files: map[string][]byte{}},
		listening: map[int]bool{},
		journal:   map[string]string{},
	}
	m.nginxOK = func() bool { return true }
	m.cmd.handle = m.answer
	m.env = &Env{Root: root, Cmd: m.cmd, Pkg: m.pkg, Sd: m.sd, Acct: m.acct, FS: m.fs, Net: m.net,
		EUID: 0, Arch: "x86_64",
		FreeMB:    func(string) (int64, error) { return 50000, nil },
		Listening: func(p int) bool { return m.listening[p] },
		// /healthz answers through the API and pings the agent.
		PanelHealth: func(ctx context.Context) error {
			for _, u := range panelUnits {
				if on, _ := m.sd.Active(ctx, u); !on {
					return errors.New("connection refused")
				}
			}
			return m.healthErr
		},
	}
	name := map[string]string{"24.04": "Ubuntu 24.04.3 LTS", "26.04": "Ubuntu 26.04 LTS"}[distro]
	m.write("/etc/os-release", fmt.Sprintf("PRETTY_NAME=%q\nNAME=\"Ubuntu\"\nVERSION_ID=%q\nID=ubuntu\n", name, distro), 0o644)
	m.write("/proc/meminfo", "MemTotal:        1999000 kB\nMemFree:          100000 kB\n", 0o444)
	stock, err := os.ReadFile(filepath.Join("testdata/golden", distro, "nginx.conf.stock"))
	if err != nil {
		t.Fatal(err)
	}
	m.write("/etc/nginx/nginx.conf", string(stock), 0o644)
	m.write("/etc/nginx/sites-enabled/default", "server {}\n", 0o644)
	return m
}

// answer is the fake command line of the machine.
func (m *machine) answer(line string) (string, error, bool) {
	switch {
	case line == "nginx -V":
		v := "nginx version: nginx/1.24.0\nconfigure arguments: --with-http_v2_module"
		if m.quic {
			v += " --with-http_v3_module"
		}
		return v, nil, true
	case line == "nginx -t":
		if m.nginxOK() {
			return "syntax is ok", nil, true
		}
		return "nginx: [emerg] test failed", errors.New("exit status 1"), true
	case strings.HasPrefix(line, "journalctl -u "):
		unit := strings.Fields(line)[2]
		if j, ok := m.journal[unit]; ok {
			return j, nil, true
		}
		return "", errors.New("exit status 1"), true
	case strings.HasSuffix(line, "/restic version"):
		b, err := os.ReadFile(m.path("/usr/local/bin/restic"))
		if err != nil {
			return "", err, true
		}
		if strings.Contains(string(b), "0.19.1") {
			return "restic 0.19.1 compiled with go1.25 on linux/amd64", nil, true
		}
		return "restic 0.16.4", nil, true
	}
	return "", nil, false
}

func (m *machine) path(p string) string { return filepath.Join(m.root, p) }

func (m *machine) write(p, content string, mode os.FileMode) {
	m.t.Helper()
	full := m.path(p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		m.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), mode); err != nil {
		m.t.Fatal(err)
	}
	if err := os.Chmod(full, mode); err != nil {
		m.t.Fatal(err)
	}
}

func (m *machine) read(p string) string {
	m.t.Helper()
	b, err := os.ReadFile(m.path(p))
	if err != nil {
		m.t.Fatal(err)
	}
	return string(b)
}

// build makes a fake local build and returns its directory.
func (m *machine) build(tag string) string {
	dir := filepath.Join(m.root, "build-"+tag)
	_ = os.MkdirAll(dir, 0o755)
	for _, b := range Binaries {
		if err := os.WriteFile(filepath.Join(dir, b), []byte("#!/bin/sh\n# "+b+" "+tag+"\n"), 0o755); err != nil {
			m.t.Fatal(err)
		}
	}
	return dir
}

// pins points the pinned downloads at fixtures served by the fake network.
func (m *machine) pins() {
	m.t.Helper()
	restic, err := os.ReadFile("testdata/fake-restic.bz2")
	if err != nil {
		m.t.Fatal(err)
	}
	wp := []byte("<?php // fake wp-cli\n")
	composer := []byte("<?php // fake composer\n")
	old := [3]Pin{pinRestic, pinWPCLI, pinComposer}
	m.t.Cleanup(func() { pinRestic, pinWPCLI, pinComposer = old[0], old[1], old[2] })
	pinRestic = Pin{"0.19.1", "https://example.test/restic.bz2", sha256Hex(restic)}
	pinWPCLI = Pin{"2.12.0", "https://example.test/wp-cli.phar", sha256Hex(wp)}
	pinComposer = Pin{"2.10.3", "https://example.test/composer.phar", sha256Hex(composer)}
	m.net.files[pinRestic.URL] = restic
	m.net.files[pinWPCLI.URL] = wp
	m.net.files[pinComposer.URL] = composer
}

// preflight runs the machine's preflight and returns the options of a run.
func (m *machine) options(container bool) Options {
	m.t.Helper()
	p, err := m.env.RunPreflight(context.Background(), container)
	if err != nil {
		m.t.Fatal(err)
	}
	if len(p.Problems) > 0 {
		m.t.Fatalf("preflight: %v", p.Problems)
	}
	return Options{OS: p.OS, Upgrade: p.Upgrade}
}

// needed returns the IDs of the steps whose check says needed.
func needed(res []PlanResult) []string {
	var ids []string
	for _, r := range res {
		if r.Needed {
			ids = append(ids, r.ID)
		}
	}
	return ids
}

// mustApply runs the install and fails the test on an error.
func (m *machine) mustApply(o Options) {
	m.t.Helper()
	m.listening[443] = true
	if err := Apply(context.Background(), m.env, Steps(m.env, o), nil); err != nil {
		m.t.Fatal(err)
	}
}
