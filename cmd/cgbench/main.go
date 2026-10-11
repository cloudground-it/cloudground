// Command cgbench runs CloudGround's benchmark suite with wrk, repeats it,
// alternates two installed builds, and says whether a difference between them
// is larger than the noise measured between runs of the same build.
// The behaviour is specified in bench/SPEC.md.
//
//	cgbench suite   [flags] <https://target> [label]
//	cgbench repeat  [flags] -n 3 <https://target> [label]
//	cgbench ab      [flags] -a <checkout> -b <checkout> <https://target>
//	cgbench compare [flags] <session-dir> | -a d1,d2 -b d3,d4
//	cgbench parity
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(runCLI(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

const usage = `usage:
  cgbench suite   [flags] <https://target> [label]     every scenario once
  cgbench repeat  [flags] <https://target> [label]     the same build N times
  cgbench ab      [flags] -a <checkout> -b <checkout> <https://target>
                                                       alternate two installed builds
  cgbench compare [flags] <dir> | -a d1,d2 -b d3,d4    report from saved runs
  cgbench parity                                       CPU parity of this box
Run "cgbench <command> -h" for the flags. See bench/SPEC.md.
`

func runCLI(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "suite":
		err = cmdSuite(ctx, args[1:], getenv, stdout, stderr)
	case "repeat":
		err = cmdSessions(ctx, "repeat", args[1:], getenv, stdout, stderr)
	case "ab":
		err = cmdSessions(ctx, "ab", args[1:], getenv, stdout, stderr)
	case "compare":
		err = cmdCompare(args[1:], stdout, stderr)
	case "parity":
		p := measureParity(ctx, defaultLoops(getenv))
		p.write(stdout)
		if p.stolen() {
			err = errors.New("steal is non-zero: this box is sharing CPU and its numbers are not comparable")
		}
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
	default:
		fmt.Fprint(stderr, usage)
		return 2
	}
	var ue usageError
	switch {
	case errors.As(err, &ue):
		fmt.Fprintln(stderr, err)
		return 2
	case errors.Is(err, flag.ErrHelp):
		return 0
	case err != nil:
		fmt.Fprintln(stderr, "cgbench:", err)
		return 1
	}
	return 0
}

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

// common holds the flags every load-generating command takes.
type common struct {
	out       string
	wrk       string
	scenarios string
	st        settings
}

func addCommon(fs *flag.FlagSet, c *common, getenv func(string) string) error {
	st, err := defaultSettings(getenv)
	if err != nil {
		return err
	}
	c.st = st
	fs.StringVar(&c.out, "out", "bench/results", "directory the results go under")
	fs.StringVar(&c.wrk, "wrk", "wrk", "the wrk binary")
	fs.StringVar(&c.scenarios, "scenarios", "", "comma-separated scenarios to run (default: all)")
	fs.StringVar(&c.st.pathCached, "path-cached", c.st.pathCached, "cached page (env PATH_CACHED)")
	fs.StringVar(&c.st.pathStatic, "path-static", c.st.pathStatic, "static fixture (env PATH_STATIC)")
	fs.DurationVar(&c.st.long, "duration", c.st.long, "length of cached-sustained (env DURATION)")
	fs.DurationVar(&c.st.short, "short", c.st.short, "length of every other scenario (env SHORT)")
	fs.DurationVar(&c.st.latTimeout, "lat-timeout", c.st.latTimeout, "wrk timeout where latency is reported (env LAT_TIMEOUT)")
	return nil
}

// prepare checks wrk and raises the descriptor limit the flood needs.
func (c common) prepare(getenv func(string) string, stderr io.Writer) (tools, []scenario, error) {
	scs, err := pickScenarios(c.scenarios)
	if err != nil {
		return tools{}, nil, usageError{err.Error()}
	}
	path, err := exec.LookPath(c.wrk)
	if err != nil {
		return tools{}, nil, errors.New("wrk is not installed")
	}
	if err := raiseFileLimit(65535); err != nil {
		fmt.Fprintln(stderr, "warning: could not raise the fd limit; the flood result will measure it:", err)
	}
	t := realTools(path, getenv)
	t.stderr = stderr
	return t, scs, nil
}

func positional(fs *flag.FlagSet, min, max int) ([]string, error) {
	a := fs.Args()
	if len(a) < min || len(a) > max {
		return nil, usageError{fmt.Sprintf("usage: cgbench %s [flags] <https://target> [label]", fs.Name())}
	}
	return a, nil
}

func cmdSuite(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("suite", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var c common
	if err := addCommon(fs, &c, getenv); err != nil {
		return err
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	pos, err := positional(fs, 1, 2)
	if err != nil {
		return err
	}
	label := "run"
	if len(pos) == 2 {
		label = pos[1]
	}
	t, scs, err := c.prepare(getenv, stderr)
	if err != nil {
		return err
	}
	t.stdout = stdout
	_, err = runSuite(ctx, t, c.st, scs, pos[0], label, filepath.Join(c.out, label))
	return err
}

func cmdSessions(ctx context.Context, name string, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var c common
	if err := addCommon(fs, &c, getenv); err != nil {
		return err
	}
	var (
		a, b, aName, bName, installCmd, label string
		rounds                                int
		ready, settle, warmup                 time.Duration
		floor                                 float64
	)
	if name == "ab" {
		fs.StringVar(&a, "a", "", "reference build: a checkout with dist/ and installer/, or its dist/")
		fs.StringVar(&b, "b", "", "build under test, same form as -a")
		fs.StringVar(&aName, "a-name", "", "name of build A in the report (default: its directory)")
		fs.StringVar(&bName, "b-name", "", "name of build B in the report")
		fs.StringVar(&installCmd, "install-cmd", defaultInstall, "install command, run with bash -c in the checkout")
		fs.DurationVar(&ready, "ready", 3*time.Minute, "how long to wait for the site after an install")
		fs.DurationVar(&settle, "settle", 10*time.Second, "pause after the site is ready")
		fs.IntVar(&rounds, "rounds", 2, "rounds; each round runs A then B")
		fs.Float64Var(&floor, "floor", defaultFloor, "smallest noise, in percent, a change must exceed")
		fs.StringVar(&label, "label", "", "results directory name (default: ab-<A>-<B>)")
	} else {
		fs.IntVar(&rounds, "n", 3, "how many runs")
	}
	fs.DurationVar(&warmup, "warmup", 10*time.Second, "discarded warm-up before each run (0: none)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	maxPos := 1
	if name == "repeat" {
		maxPos = 2
	}
	pos, err := positional(fs, 1, maxPos)
	if err != nil {
		return err
	}
	if rounds < 1 {
		return usageError{"rounds must be at least 1"}
	}
	cfg := abConfig{base: pos[0], rounds: rounds, st: c.st, installCmd: installCmd,
		ready: ready, settle: settle, warmup: warmup, floor: floor}
	if name == "ab" {
		if a == "" || b == "" {
			return usageError{"ab needs -a and -b"}
		}
		ba, err := resolveBuild(a, aName)
		if err != nil {
			return err
		}
		bb, err := resolveBuild(b, bName)
		if err != nil {
			return err
		}
		if ba.name == bb.name {
			ba.name, bb.name = ba.name+"-A", bb.name+"-B"
		}
		cfg.builds, cfg.install, cfg.label = []buildSpec{ba, bb}, true, "ab-"+ba.name+"-"+bb.name
	} else {
		cfg.label = "repeat"
		if len(pos) == 2 {
			cfg.label = pos[1]
		}
		cfg.builds = []buildSpec{{name: cfg.label}}
	}
	if label != "" {
		cfg.label = label
	}
	cfg.dir = filepath.Join(c.out, cfg.label)
	t, scs, err := c.prepare(getenv, stderr)
	if err != nil {
		return err
	}
	t.stdout = stdout
	cfg.scenarios = scs
	rep, err := runSessions(ctx, t, cfg)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout)
	rep.writeMarkdown(stdout)
	if hint := rerunHint(cfg, rep); hint != "" {
		fmt.Fprintf(stdout, "\nA spread from %d runs is a weak estimate. Repeat the flagged scenarios alone before concluding:\n  %s\n", rounds, hint)
	}
	fmt.Fprintf(stdout, "\nreport in %s\n", filepath.Join(cfg.dir, "report.md"))
	return nil
}

func cmdCompare(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var a, b, aName, bName string
	var floor float64
	fs.StringVar(&a, "a", "", "comma-separated run directories of the reference build")
	fs.StringVar(&b, "b", "", "comma-separated run directories of the build under test")
	fs.StringVar(&aName, "a-name", "", "name of build A")
	fs.StringVar(&bName, "b-name", "", "name of build B")
	fs.Float64Var(&floor, "floor", defaultFloor, "smallest noise, in percent, a change must exceed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var runs []run
	var parity []string
	var dir string
	switch {
	case a != "" && fs.NArg() == 0:
		as, err := loadList(a)
		if err != nil {
			return err
		}
		bs, err := loadList(b)
		if err != nil {
			return err
		}
		runs = interleave(as, bs)
	case a == "" && b == "" && fs.NArg() == 1:
		dir = fs.Arg(0)
		var err error
		if runs, parity, err = loadSession(dir); err != nil {
			return err
		}
		names := readBuildNames(dir)
		if aName == "" {
			aName = names["A"]
			if aName == "" {
				aName = names["run"]
			}
		}
		if bName == "" {
			bName = names["B"]
		}
	default:
		return usageError{"usage: cgbench compare <session-dir> | -a d1,d2 [-b d3,d4]"}
	}
	if aName == "" {
		aName = "A"
	}
	if bName == "" {
		bName = "B"
	}
	rep := analyse(runs, aName, bName, floor)
	rep.parity = parity
	rep.writeMarkdown(stdout)
	if dir != "" {
		return writeReport(dir, rep)
	}
	return nil
}

func loadList(list string) ([]summary, error) {
	var out []summary
	for _, d := range strings.Split(list, ",") {
		if d = strings.TrimSpace(d); d == "" {
			continue
		}
		s, err := loadRun(d)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// defaultFloor is the smallest noise a change must exceed to be called a
// regression or an improvement: two runs of one build that happen to land
// close together must not make a 2 % difference look significant. On the test
// VPS runs of one build spread 1–28 % (bench/phase-3.md, phase-4-integration.md).
const defaultFloor = 3.0
