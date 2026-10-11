package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// buildSpec is one installable build: a checkout holding dist/ and
// installer/ (bench/SPEC.md §4).
type buildSpec struct {
	name     string
	checkout string
	dist     string
}

// resolveBuild accepts a checkout directory or its dist/ directory.
func resolveBuild(path, name string) (buildSpec, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return buildSpec{}, err
	}
	b := buildSpec{name: name}
	switch {
	case fileExists(filepath.Join(abs, "dist", "panel-api")):
		b.checkout, b.dist = abs, filepath.Join(abs, "dist")
	case fileExists(filepath.Join(abs, "panel-api")):
		b.checkout, b.dist = filepath.Dir(abs), abs
	default:
		return buildSpec{}, fmt.Errorf("%s: neither it nor its dist/ holds a panel-api binary", path)
	}
	if b.name == "" {
		b.name = filepath.Base(b.checkout)
	}
	return b, nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

const defaultInstall = "bash installer/install.sh"

// abConfig is everything one A/B or repeat session needs.
type abConfig struct {
	base       string
	dir        string // <out>/<label>
	label      string
	builds     []buildSpec // one for repeat, two for A/B
	rounds     int
	scenarios  []scenario
	st         settings
	installCmd string
	install    bool
	ready      time.Duration
	settle     time.Duration
	warmup     time.Duration
	floor      float64
}

const installTimeout = 30 * time.Minute

// runSessions alternates the builds for the configured rounds: install,
// wait, warm up, measure parity, run the suite. With one build and install
// off it is a plain repetition.
func runSessions(ctx context.Context, t tools, c abConfig) (report, error) {
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return report{}, err
	}
	if err := writeBuilds(c.dir, c.builds); err != nil {
		return report{}, err
	}
	var runs []run
	var parity []string
	for round := 1; round <= c.rounds; round++ {
		for i, b := range c.builds {
			r := run{build: string(rune('A' + i)), round: round}
			id := buildLetter(i, len(c.builds)) + strconv.Itoa(round)
			if c.install {
				if err := installBuild(ctx, t, c, b, id); err != nil {
					return report{}, err
				}
				if err := waitReady(ctx, t, joinURL(c.base, c.st.pathCached), c.ready); err != nil {
					return report{}, fmt.Errorf("%s after installing %s: %w", id, b.name, err)
				}
				t.sleep(c.settle)
			}
			if c.warmup > 0 {
				warmupRun(ctx, t, c)
			}
			runDir := filepath.Join(c.dir, id)
			if err := os.MkdirAll(runDir, 0o755); err != nil {
				return report{}, err
			}
			p := t.parity(ctx)
			var pb bytes.Buffer
			p.write(&pb)
			if err := os.WriteFile(filepath.Join(runDir, "parity.txt"), pb.Bytes(), 0o644); err != nil {
				return report{}, err
			}
			parity = append(parity, fmt.Sprintf("%s %s s, steal %s", id, p.loopSeconds, p.steal))
			if p.stolen() {
				fmt.Fprintf(t.stderr, "warning: %s: steal is %s%%, this box shares its CPU\n", id, p.steal)
			}
			s, err := runSuite(ctx, t, c.st, c.scenarios, c.base, fmt.Sprintf("%s %s (%s)", c.label, id, b.name), runDir)
			if err != nil {
				return report{}, err
			}
			r.s = s
			runs = append(runs, r)
		}
	}
	rep := analyse(runs, c.builds[0].name, lastName(c.builds), c.floor)
	rep.parity = parity
	return rep, writeReport(c.dir, rep)
}

func buildLetter(i, n int) string {
	if n == 1 {
		return "run"
	}
	return string(rune('A' + i))
}

func lastName(b []buildSpec) string {
	if len(b) < 2 {
		return ""
	}
	return b[1].name
}

func installBuild(ctx context.Context, t tools, c abConfig, b buildSpec, id string) error {
	logPath := filepath.Join(c.dir, "install-"+id+".log")
	f, err := os.Create(logPath)
	if err != nil {
		return err
	}
	defer f.Close()
	fmt.Fprintf(t.stdout, "== %s: installing %s from %s ==\n", id, b.name, b.dist)
	ictx, cancel := context.WithTimeout(ctx, installTimeout)
	defer cancel()
	env := []string{"CLOUDGROUND_LOCAL_BUILD=" + b.dist, "CG_CHECKOUT=" + b.checkout}
	if err := t.shell(ictx, b.checkout, c.installCmd, env, f); err != nil {
		return fmt.Errorf("%s: installing %s failed (%v); see %s", id, b.name, err, logPath)
	}
	return nil
}

// waitReady polls url until it answers 2xx or the budget runs out.
func waitReady(ctx context.Context, t tools, url string, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	var last error
	for {
		code, err := t.get(ctx, url)
		if err == nil && code >= 200 && code < 300 {
			return nil
		}
		if err == nil {
			err = fmt.Errorf("status %d", code)
		}
		last = err
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%s not ready after %s: %w", url, budget, last)
		}
		t.sleep(2 * time.Second)
	}
}

// warmupRun fills the caches after an install. Its numbers are thrown away.
func warmupRun(ctx context.Context, t tools, c abConfig) {
	args := []string{"-t2", "-c50", "-d" + wrkDuration(c.warmup), "--timeout", wrkDuration(c.st.latTimeout),
		"-H", "Accept-Encoding: gzip", joinURL(c.base, c.st.pathCached)}
	wctx, cancel := context.WithTimeout(ctx, c.warmup+hangMargin)
	defer cancel()
	var sink bytes.Buffer
	_ = t.wrk(wctx, args, &sink)
}

func httpStatus(ctx context.Context, url string) (int, error) {
	client := &http.Client{
		Timeout: 10 * time.Second,
		// A readiness probe against a freshly installed box: its
		// certificate may not be issued yet.
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

// builds.tsv records which build each letter was, so compare can name them.
func writeBuilds(dir string, builds []buildSpec) error {
	var b bytes.Buffer
	for i, s := range builds {
		fmt.Fprintf(&b, "%s\t%s\t%s\n", buildLetter(i, len(builds)), s.name, s.checkout)
	}
	return os.WriteFile(filepath.Join(dir, "builds.tsv"), b.Bytes(), 0o644)
}

func readBuildNames(dir string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(filepath.Join(dir, "builds.tsv"))
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		p := strings.Split(sc.Text(), "\t")
		if len(p) >= 2 {
			out[p[0]] = p[1]
		}
	}
	return out
}

func writeReport(dir string, rep report) error {
	var md, tsv bytes.Buffer
	rep.writeMarkdown(&md)
	rep.writeRunsTSV(&tsv)
	if err := os.WriteFile(filepath.Join(dir, "report.md"), md.Bytes(), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "runs.tsv"), tsv.Bytes(), 0o644)
}

var runDirName = regexp.MustCompile(`^(A|B|run)(\d+)$`)

// loadSession reads back the runs of a directory written by ab or repeat,
// with the CPU parity notes of the runs that have a parity.txt.
func loadSession(dir string) ([]run, []string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	type found struct {
		r    run
		name string
	}
	var all []found
	for _, e := range entries {
		m := runDirName.FindStringSubmatch(e.Name())
		if !e.IsDir() || m == nil {
			continue
		}
		s, err := loadRun(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, nil, err
		}
		n, _ := strconv.Atoi(m[2])
		build := m[1]
		if build == "run" {
			build = "A"
		}
		all = append(all, found{run{build: build, round: n, s: s}, e.Name()})
	}
	if len(all) == 0 {
		return nil, nil, errors.New(dir + ": no A<n>, B<n> or run<n> directories")
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].r.round != all[j].r.round {
			return all[i].r.round < all[j].r.round
		}
		return all[i].r.build < all[j].r.build
	})
	var runs []run
	var parity []string
	for _, f := range all {
		runs = append(runs, f.r)
		if p, ok := readParity(filepath.Join(dir, f.name, "parity.txt")); ok {
			parity = append(parity, fmt.Sprintf("%s %s s, steal %s", f.name, p.loopSeconds, p.steal))
		}
	}
	return runs, parity, nil
}

// readParity parses what parityResult.write printed.
func readParity(path string) (parityResult, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return parityResult{}, false
	}
	p := parityResult{loopSeconds: "unknown", steal: "unknown"}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, _ := strings.Cut(line, " ")
		switch k {
		case "cpu_loop_seconds":
			p.loopSeconds = v
		case "steal_percent":
			p.steal = v
		case "model":
			p.model = v
		case "cores":
			p.cores, _ = strconv.Atoi(v)
		}
	}
	return p, true
}

// rerunHint is the command that repeats only the regressed scenarios.
func rerunHint(c abConfig, rep report) string {
	if len(rep.regressed) == 0 || len(c.builds) < 2 {
		return ""
	}
	rounds := c.rounds + 1
	if rounds < 3 {
		rounds = 3
	}
	return fmt.Sprintf("cgbench ab -a %s -b %s -rounds %d -scenarios %s -label %s-recheck %s",
		c.builds[0].checkout, c.builds[1].checkout, rounds, strings.Join(rep.regressed, ","), c.label, c.base)
}
