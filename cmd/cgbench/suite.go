package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// tools is what the commands need from the outside world. Tests replace it.
type tools struct {
	stdout io.Writer
	stderr io.Writer
	// wrk runs the load generator with args, writing its stdout and stderr
	// to out. Its error is reported, never fatal: a failed run still leaves
	// its output, and its row reads "na".
	wrk func(ctx context.Context, args []string, out io.Writer) error
	// shell runs an install: a command for bash -c, in dir, with extra
	// environment, output to out and stdin closed.
	shell func(ctx context.Context, dir, command string, env []string, out io.Writer) error
	// get fetches url and returns the status code.
	get   func(ctx context.Context, url string) (int, error)
	sleep func(time.Duration)
	// parity measures CPU parity (bench/SPEC.md §5).
	parity func(ctx context.Context) parityResult
}

func realTools(wrkPath string, getenv func(string) string) tools {
	return tools{
		stdout: os.Stdout,
		stderr: os.Stderr,
		wrk: func(ctx context.Context, args []string, out io.Writer) error {
			cmd := exec.CommandContext(ctx, wrkPath, args...)
			cmd.Stdout, cmd.Stderr = out, out
			return cmd.Run()
		},
		shell: func(ctx context.Context, dir, command string, env []string, out io.Writer) error {
			cmd := exec.CommandContext(ctx, "bash", "-c", command)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), env...)
			cmd.Stdout, cmd.Stderr = out, out
			return cmd.Run()
		},
		get:    httpStatus,
		sleep:  time.Sleep,
		parity: func(ctx context.Context) parityResult { return measureParity(ctx, defaultLoops(getenv)) },
	}
}

// hangMargin is how long past its duration a wrk run may take before it is
// killed.
const hangMargin = 2 * time.Minute

// runSuite runs the scenarios against base and writes the results into dir
// (bench/SPEC.md §3). label only names the run in the progress output.
func runSuite(ctx context.Context, t tools, st settings, scs []scenario, base, label, dir string) (summary, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	scripts, err := os.MkdirTemp("", "cgbench-lua-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(scripts)
	if err := writeScripts(scripts); err != nil {
		return nil, err
	}

	fmt.Fprintf(t.stdout, "== %s against %s ==\n", label, base)
	for _, sc := range scs {
		fmt.Fprintf(t.stdout, "  %s\n", sc.name)
		var out bytes.Buffer
		rctx, cancel := context.WithTimeout(ctx, st.duration(sc)+hangMargin)
		err := t.wrk(rctx, st.wrkArgs(sc, base, scripts), &out)
		cancel()
		if err != nil && ctx.Err() == nil {
			fmt.Fprintf(&out, "\ncgbench: wrk: %v\n", err)
		}
		if werr := os.WriteFile(filepath.Join(dir, sc.name+".txt"), out.Bytes(), 0o644); werr != nil {
			return nil, werr
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}

	s := summary{}
	for _, sc := range scs {
		raw, err := os.ReadFile(filepath.Join(dir, sc.name+".txt"))
		if err != nil {
			continue
		}
		s[sc.name] = parseWrk(sc.name, string(raw))
	}
	var tsv bytes.Buffer
	if err := writeSummary(&tsv, s); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "summary.tsv"), tsv.Bytes(), 0o644); err != nil {
		return nil, err
	}
	fmt.Fprintf(t.stdout, "\n%s\nraw output in %s\n", tsv.String(), dir)
	for _, name := range summaryOrder {
		if r, ok := s[name]; ok && r.non2xx > 0 {
			fmt.Fprintf(t.stderr, "warning: %s got %d non-2xx/3xx responses; its numbers describe error pages too\n", name, r.non2xx)
		}
	}
	return s, nil
}
