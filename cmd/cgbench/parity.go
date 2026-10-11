package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// parityResult is what bench/SPEC.md §5 prints. Missing values are "unknown".
type parityResult struct {
	loopSeconds string
	steal       string
	model       string
	cores       int
}

func (p parityResult) write(w io.Writer) {
	fmt.Fprintf(w, "cpu_loop_seconds %s\n", p.loopSeconds)
	fmt.Fprintf(w, "steal_percent %s\n", p.steal)
	fmt.Fprintf(w, "model %s\n", p.model)
	fmt.Fprintf(w, "cores %d\n", p.cores)
}

// stolen reports whether the hypervisor took CPU time during the sample.
func (p parityResult) stolen() bool {
	return p.steal != "unknown" && p.steal != "0"
}

func defaultLoops(getenv func(string) string) int {
	if n, err := strconv.Atoi(getenv("LOOPS")); err == nil && n > 0 {
		return n
	}
	return 20_000_000
}

// parityProgram is the fixed CPU loop. It is an awk program, not Go, so that
// its timings stay comparable with the historical records.
const parityProgram = `BEGIN { for (i = 0; i < n; i++) s += i % 7; if (s < 0) print s }`

const stealSample = 2 * time.Second

func measureParity(ctx context.Context, loops int) parityResult {
	p := parityResult{loopSeconds: "unknown", steal: "unknown", model: "unknown", cores: runtime.NumCPU()}
	start := time.Now()
	if err := exec.CommandContext(ctx, "awk", "-v", "n="+strconv.Itoa(loops), parityProgram).Run(); err == nil {
		p.loopSeconds = fmt.Sprintf("%.2f", time.Since(start).Seconds())
	}
	if info, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		if m := cpuModel(string(info)); m != "" {
			p.model = m
		}
	}
	before, err1 := os.ReadFile("/proc/stat")
	select {
	case <-ctx.Done():
		return p
	case <-time.After(stealSample):
	}
	after, err2 := os.ReadFile("/proc/stat")
	if err1 == nil && err2 == nil {
		if s, ok := stealPercent(string(before), string(after)); ok {
			p.steal = s
		}
	}
	return p
}

// cpuModel is the first "model name" in /proc/cpuinfo.
func cpuModel(cpuinfo string) string {
	sc := bufio.NewScanner(strings.NewReader(cpuinfo))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if ok && strings.TrimSpace(k) == "model name" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// cpuTimes returns the aggregate "cpu" line of /proc/stat: the total of its
// fields and the steal field (the eighth).
func cpuTimes(stat string) (total, steal uint64, ok bool) {
	sc := bufio.NewScanner(strings.NewReader(stat))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 9 || f[0] != "cpu" {
			continue
		}
		// guest and guest_nice (fields 9 and 10) are already counted in
		// user and nice, so they stay out of the total.
		for i := 1; i <= 8 && i < len(f); i++ {
			v, err := strconv.ParseUint(f[i], 10, 64)
			if err != nil {
				return 0, 0, false
			}
			total += v
			if i == 8 {
				steal = v
			}
		}
		return total, steal, true
	}
	return 0, 0, false
}

// stealPercent is the share of CPU time stolen between two /proc/stat
// samples, rounded to an integer the way vmstat prints it.
func stealPercent(before, after string) (string, bool) {
	t0, s0, ok0 := cpuTimes(before)
	t1, s1, ok1 := cpuTimes(after)
	if !ok0 || !ok1 || t1 <= t0 || s1 < s0 {
		return "", false
	}
	return strconv.Itoa(int(math.Round(float64(s1-s0) / float64(t1-t0) * 100))), true
}
