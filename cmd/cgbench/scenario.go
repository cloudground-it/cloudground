package main

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The Lua scripts travel inside the binary, so a copy of cgbench on a test
// server needs nothing else from the checkout.
//
//go:embed lua/*.lua
var luaFS embed.FS

// target says which configured path a scenario requests.
type target int

const (
	cachedPath target = iota
	staticPath
)

// length says which configured duration a scenario runs for.
type length int

const (
	longRun length = iota
	shortRun
)

// scenario is one wrk run of the suite (bench/SPEC.md §2).
type scenario struct {
	name    string
	path    target
	threads int
	conns   int
	length  length
	// timeout is fixed for the flood; zero means the latency timeout.
	timeout time.Duration
	script  string
}

// runOrder is the order the suite runs in.
var runOrder = []scenario{
	{name: "cached-sustained", path: cachedPath, threads: 4, conns: 500, length: longRun},
	{name: "cached-single", path: cachedPath, threads: 1, conns: 1, length: shortRun},
	{name: "cached-spike", path: cachedPath, threads: 4, conns: 200, length: shortRun},
	{name: "flood", path: cachedPath, threads: 8, conns: 2000, length: shortRun, timeout: 5 * time.Second},
	{name: "static", path: staticPath, threads: 4, conns: 200, length: shortRun},
	{name: "browsers", path: cachedPath, threads: 4, conns: 200, length: shortRun, script: "browsers.lua"},
	{name: "uncached", path: cachedPath, threads: 2, conns: 10, length: shortRun, script: "uncached.lua"},
}

// summaryOrder is the order of the rows in summary.tsv and in reports.
var summaryOrder = []string{
	"cached-sustained", "cached-single", "cached-spike", "static", "flood", "browsers", "uncached",
}

// settings are the knobs every suite run takes, with defaults from the
// environment (bench/SPEC.md §2).
type settings struct {
	pathCached string
	pathStatic string
	long       time.Duration
	short      time.Duration
	latTimeout time.Duration
}

func defaultSettings(getenv func(string) string) (settings, error) {
	s := settings{pathCached: "/shop/", pathStatic: "/bench-static.txt",
		long: 60 * time.Second, short: 30 * time.Second, latTimeout: 10 * time.Second}
	if v := getenv("PATH_CACHED"); v != "" {
		s.pathCached = v
	}
	if v := getenv("PATH_STATIC"); v != "" {
		s.pathStatic = v
	}
	for _, d := range []struct {
		env string
		dst *time.Duration
	}{{"DURATION", &s.long}, {"SHORT", &s.short}, {"LAT_TIMEOUT", &s.latTimeout}} {
		v := getenv(d.env)
		if v == "" {
			continue
		}
		t, err := time.ParseDuration(v)
		if err != nil || t <= 0 {
			return s, fmt.Errorf("%s=%q is not a duration like 30s", d.env, v)
		}
		*d.dst = t
	}
	return s, nil
}

func (s settings) duration(sc scenario) time.Duration {
	if sc.length == longRun {
		return s.long
	}
	return s.short
}

// wrkArgs is the wrk command line for one scenario. scripts is the directory
// the embedded Lua files were written to.
func (s settings) wrkArgs(sc scenario, base, scripts string) []string {
	timeout := sc.timeout
	if timeout == 0 {
		timeout = s.latTimeout
	}
	path := s.pathCached
	if sc.path == staticPath {
		path = s.pathStatic
	}
	args := []string{
		fmt.Sprintf("-t%d", sc.threads),
		fmt.Sprintf("-c%d", sc.conns),
		"-d" + wrkDuration(s.duration(sc)),
		"--timeout", wrkDuration(timeout),
		"-H", "Accept-Encoding: gzip",
		"--latency",
	}
	if sc.script != "" {
		args = append(args, "-s", filepath.Join(scripts, sc.script))
	}
	return append(args, joinURL(base, path))
}

// joinURL appends a path to the target without doubling the slash.
func joinURL(base, path string) string {
	return strings.TrimRight(base, "/") + path
}

// wrkDuration writes a duration the way wrk reads it: whole seconds with an
// "s", which is what every scenario uses.
func wrkDuration(d time.Duration) string {
	if d%time.Second == 0 {
		return fmt.Sprintf("%ds", int(d/time.Second))
	}
	return fmt.Sprintf("%dms", d.Milliseconds())
}

// pickScenarios returns the scenarios named in a comma list, in run order.
// An empty list means all of them.
func pickScenarios(list string) ([]scenario, error) {
	if strings.TrimSpace(list) == "" {
		return runOrder, nil
	}
	want := map[string]bool{}
	for _, n := range strings.Split(list, ",") {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		found := false
		for _, sc := range runOrder {
			if sc.name == n {
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("unknown scenario %q (known: %s)", n, strings.Join(summaryOrder, ", "))
		}
		want[n] = true
	}
	var out []scenario
	for _, sc := range runOrder {
		if want[sc.name] {
			out = append(out, sc)
		}
	}
	return out, nil
}

// writeScripts copies the embedded Lua files into dir.
func writeScripts(dir string) error {
	entries, err := luaFS.ReadDir("lua")
	if err != nil {
		return err
	}
	for _, e := range entries {
		b, err := luaFS.ReadFile("lua/" + e.Name())
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), b, 0o644); err != nil {
			return err
		}
	}
	return nil
}
