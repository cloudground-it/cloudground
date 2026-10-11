package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBox stands in for wrk, the installer and the server. The installed
// build decides the req/s that every scenario reports, so a test can make B
// slower or faster than A.
type fakeBox struct {
	mu        sync.Mutex
	events    []string
	installed string
	rps       map[string][]string // build name -> req/s per run, consumed in order
	runs      map[string]int
	notReady  int // GETs that fail before the site answers
	failOn    string
}

func (f *fakeBox) tools(t *testing.T, stdout io.Writer) tools {
	return tools{
		stdout: stdout,
		stderr: io.Discard,
		wrk: func(_ context.Context, args []string, out io.Writer) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if !contains(args, "--latency") {
				f.events = append(f.events, "warmup "+f.installed)
				return nil
			}
			f.events = append(f.events, "wrk "+f.installed+" "+args[1])
			rps := "na"
			if l := f.rps[f.installed]; len(l) > 0 {
				rps = l[f.runs[f.installed]%len(l)]
			}
			fmt.Fprintf(out, "  Latency Distribution\n     50%%   1.00ms\n     99%%   9.00ms\nRequests/sec: %s\n", rps)
			return nil
		},
		shell: func(_ context.Context, dir, command string, env []string, out io.Writer) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			build := filepath.Base(dir)
			if !contains(env, "CLOUDGROUND_LOCAL_BUILD="+filepath.Join(dir, "dist")) || !contains(env, "CG_CHECKOUT="+dir) {
				t.Errorf("install env %v for %s", env, dir)
			}
			f.events = append(f.events, "install "+build+": "+command)
			fmt.Fprintln(out, "installed", build)
			if build == f.failOn {
				return errors.New("exit status 1")
			}
			if f.installed != "" {
				f.runs[f.installed]++
			}
			f.installed = build
			return nil
		},
		get: func(context.Context, string) (int, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.notReady > 0 {
				f.notReady--
				return 502, nil
			}
			return 200, nil
		},
		sleep: func(time.Duration) {},
		parity: func(context.Context) parityResult {
			return parityResult{loopSeconds: "2.21", steal: "0", model: "Test CPU", cores: 2}
		},
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func checkout(t *testing.T, root, name string) string {
	t.Helper()
	d := filepath.Join(root, name, "dist")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "panel-api"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, name)
}

func abSetup(t *testing.T, rounds int, scenarios string) (abConfig, string) {
	t.Helper()
	root := t.TempDir()
	a, err := resolveBuild(checkout(t, root, "main"), "")
	if err != nil {
		t.Fatal(err)
	}
	// The dist directory is accepted too.
	b, err := resolveBuild(filepath.Join(checkout(t, root, "next"), "dist"), "")
	if err != nil {
		t.Fatal(err)
	}
	if a.name != "main" || b.name != "next" || b.checkout != filepath.Join(root, "next") {
		t.Fatalf("builds %+v %+v", a, b)
	}
	scs, err := pickScenarios(scenarios)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := defaultSettings(func(string) string { return "" })
	out := filepath.Join(root, "results", "ab")
	return abConfig{base: "https://bench.example", dir: out, label: "ab", builds: []buildSpec{a, b},
		rounds: rounds, scenarios: scs, st: st, installCmd: defaultInstall, install: true,
		ready: time.Minute, warmup: 10 * time.Second}, out
}

func TestABAlternatesBuilds(t *testing.T) {
	cfg, out := abSetup(t, 2, "cached-sustained,static")
	box := &fakeBox{runs: map[string]int{}, notReady: 2, rps: map[string][]string{
		"main": {"10000", "10400"},
		"next": {"9000", "9100"},
	}}
	var stdout bytes.Buffer
	rep, err := runSessions(context.Background(), box.tools(t, &stdout), cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"install main: bash installer/install.sh", "warmup main", "wrk main -c500", "wrk main -c200",
		"install next: bash installer/install.sh", "warmup next", "wrk next -c500", "wrk next -c200",
		"install main: bash installer/install.sh", "warmup main", "wrk main -c500", "wrk main -c200",
		"install next: bash installer/install.sh", "warmup next", "wrk next -c500", "wrk next -c200",
	}
	if !reflect.DeepEqual(box.events, want) {
		t.Fatalf("events:\n%s", strings.Join(box.events, "\n"))
	}
	for _, d := range []string{"A1", "B1", "A2", "B2"} {
		for _, f := range []string{"summary.tsv", "parity.txt", "cached-sustained.txt", "static.txt"} {
			if _, err := os.Stat(filepath.Join(out, d, f)); err != nil {
				t.Error(err)
			}
		}
	}
	for _, f := range []string{"install-A1.log", "install-B2.log", "report.md", "runs.tsv", "builds.tsv"} {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Error(err)
		}
	}
	// main 10,000 / 10,400 (spread 3.9 %), next 9,000 / 9,100: -11.3 %.
	r := resultFor(t, rep, "cached-sustained")
	if r.verdict != verdictRegression || !near(r.change, -11.27, 0.01) {
		t.Errorf("%+v", r)
	}
	if hint := rerunHint(cfg, rep); !strings.Contains(hint, "-rounds 3 -scenarios cached-sustained,static -label ab-recheck https://bench.example") {
		t.Errorf("hint %q", hint)
	}

	// compare rebuilds the same report from the directory.
	var cmp bytes.Buffer
	if code := runCLI(context.Background(), []string{"compare", out}, func(string) string { return "" }, &cmp, io.Discard); code != 0 {
		t.Fatalf("compare exit %d", code)
	}
	saved, _ := os.ReadFile(filepath.Join(out, "report.md"))
	if !strings.Contains(cmp.String(), "| main 1 | next 1 | main 2 | next 2 |") ||
		!strings.Contains(cmp.String(), "**Verdict:** "+verdictRegression+": cached-sustained, static.") {
		t.Errorf("compare:\n%s", cmp.String())
	}
	if !bytes.Equal(saved, cmp.Bytes()) {
		t.Errorf("compare report differs from the saved one:\n%s\n---\n%s", saved, cmp.String())
	}
}

func TestABStopsOnFailedInstall(t *testing.T) {
	cfg, out := abSetup(t, 2, "cached-single")
	box := &fakeBox{runs: map[string]int{}, failOn: "next", rps: map[string][]string{"main": {"7000"}}}
	_, err := runSessions(context.Background(), box.tools(t, io.Discard), cfg)
	if err == nil || !strings.Contains(err.Error(), "B1: installing next failed") {
		t.Fatalf("err %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "B1")); !os.IsNotExist(err) {
		t.Error("no run directory after a failed install")
	}
	log, _ := os.ReadFile(filepath.Join(out, "install-B1.log"))
	if !strings.Contains(string(log), "installed next") {
		t.Errorf("install log %q", log)
	}
}

func TestABGivesUpWhenSiteNeverAnswers(t *testing.T) {
	cfg, _ := abSetup(t, 1, "cached-single")
	cfg.ready = 0
	box := &fakeBox{runs: map[string]int{}, notReady: 1000}
	_, err := runSessions(context.Background(), box.tools(t, io.Discard), cfg)
	if err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("err %v", err)
	}
}

func TestRepeatRunsOneBuild(t *testing.T) {
	root := t.TempDir()
	scs, _ := pickScenarios("cached-sustained")
	st, _ := defaultSettings(func(string) string { return "" })
	cfg := abConfig{base: "https://bench.example", dir: filepath.Join(root, "rep"), label: "rep",
		builds: []buildSpec{{name: "rep"}}, rounds: 3, scenarios: scs, st: st}
	box := &fakeBox{runs: map[string]int{}, rps: map[string][]string{"": {"12607"}}}
	rep, err := runSessions(context.Background(), box.tools(t, io.Discard), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(box.events) != 3 || !rep.single || rep.verdict != "" {
		t.Fatalf("events %v, report %+v", box.events, rep)
	}
	for _, d := range []string{"run1", "run2", "run3"} {
		if _, err := os.Stat(filepath.Join(cfg.dir, d, "summary.tsv")); err != nil {
			t.Error(err)
		}
	}
	runs, _, err := loadSession(cfg.dir)
	if err != nil || len(runs) != 3 || runs[0].build != "A" {
		t.Fatalf("%v %v", runs, err)
	}
}

// The suite writes what bench/SPEC.md §3 says, in the shape the old shell
// suite printed.
func TestSuiteOutput(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "label")
	var stdout bytes.Buffer
	var calls [][]string
	tl := tools{stdout: &stdout, stderr: &bytes.Buffer{},
		wrk: func(_ context.Context, args []string, out io.Writer) error {
			calls = append(calls, args)
			var f string
			switch args[1] {
			case "-c500":
				f = "sustained.txt"
			case "-c1":
				f = "single.txt"
			case "-c2000":
				f = "flood.txt"
			case "-c10":
				f = "uncached.txt"
			default:
				f = "refused.txt"
			}
			io.WriteString(out, fixture(t, f))
			return errors.New("exit status 1")
		}}
	st, _ := defaultSettings(func(string) string { return "" })
	s, err := runSuite(context.Background(), tl, st, runOrder, "https://bench.example", "label", dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 7 || len(s) != 7 {
		t.Fatalf("%d calls, %d rows", len(calls), len(s))
	}
	// The scripts exist while wrk runs.
	if p := calls[5][len(calls[5])-2]; !strings.HasSuffix(p, "browsers.lua") {
		t.Errorf("browsers script %s", p)
	}
	tsv, _ := os.ReadFile(filepath.Join(dir, "summary.tsv"))
	want := summaryHeader + "\n" +
		"cached-sustained\t9877.75\t55.30ms\t629.43ms\tnone\n" +
		"cached-single\t6887.16\t129.00us\t250.00us\tnone\n" +
		"cached-spike\tna\tna\tna\tnone\n" +
		"static\tna\tna\tna\tnone\n" +
		"flood\t7863.04\t196.27ms\t1.43s\tconnect 0, read 0, write 0, timeout 1054\n" +
		"browsers\tna\tna\tna\tnone\n" +
		"uncached\t5.99\t1.64s\t2.56s\tnone\n"
	if string(tsv) != want {
		t.Errorf("summary.tsv:\n%s", tsv)
	}
	wantOut := "== label against https://bench.example ==\n" +
		"  cached-sustained\n  cached-single\n  cached-spike\n  flood\n  static\n  browsers\n  uncached\n" +
		"\n" + want + "\nraw output in " + dir + "\n"
	if stdout.String() != wantOut {
		t.Errorf("stdout:\n%s\nwant:\n%s", stdout.String(), wantOut)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "static.txt"))
	if !strings.Contains(string(raw), "Connection refused") || !strings.Contains(string(raw), "cgbench: wrk: exit status 1") {
		t.Errorf("static.txt keeps wrk's output: %q", raw)
	}
	if !strings.Contains(tl.stderr.(*bytes.Buffer).String(), "uncached got 12 non-2xx/3xx responses") {
		t.Error("no non-2xx warning")
	}
}

func TestCLIUsage(t *testing.T) {
	env := func(string) string { return "" }
	for _, args := range [][]string{
		nil, {"bogus"}, {"suite"}, {"ab", "https://x"}, {"compare"},
		{"suite", "-scenarios", "nope", "https://x"},
	} {
		if code := runCLI(context.Background(), args, env, io.Discard, io.Discard); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	if code := runCLI(context.Background(), []string{"suite", "-wrk", "/nonexistent/wrk", "https://x"}, env, io.Discard, io.Discard); code != 1 {
		t.Errorf("missing wrk: exit %d", code)
	}
}

func TestCompareLists(t *testing.T) {
	root := t.TempDir()
	write := func(name, rps string) string {
		d := filepath.Join(root, name)
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, "summary.tsv"), []byte(summaryHeader+"\nstatic\t"+rps+"\t1ms\t2ms\tnone\n"), 0o644)
		return d
	}
	a := write("a1", "16000") + "," + write("a2", "16400")
	b := write("b1", "16100") + "," + write("b2", "16300")
	var out bytes.Buffer
	if code := runCLI(context.Background(), []string{"compare", "-a-name", "old", "-a", a, "-b", b}, func(string) string { return "" }, &out, io.Discard); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "| static | 16,000 | 16,100 | 16,400 | 16,300 | +0.0 % | 2.5 % | 1.2 % | within noise |") {
		t.Errorf("%s", out.String())
	}
}
