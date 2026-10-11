package main

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

// run is one suite run of one build, as it took place.
type run struct {
	build string // "A" (the reference) or "B"
	round int    // 1-based
	s     summary
}

func (r run) id() string { return r.build + strconv.Itoa(r.round) }

// Verdicts, per scenario and overall (bench/SPEC.md §6).
const (
	verdictNoise       = "within noise"
	verdictRegression  = "regression beyond the measured spread"
	verdictImprovement = "improvement beyond the measured spread"
	verdictFewRuns     = "not enough runs"
	verdictAllNoise    = "change within noise"
)

// result is one scenario's line of the report.
type result struct {
	name       string
	values     []cellValue // one per run, in run order
	meanA      float64
	meanB      float64
	spreadA    float64 // percent; NaN with fewer than two runs
	spreadB    float64
	change     float64 // percent; NaN unless both builds have a run
	noise      float64
	verdict    string
	errorRuns  []string // "A1: connect 0, read 0, write 0, timeout 12"
	non2xxRuns []string
}

type cellValue struct {
	run string
	rps float64
	ok  bool
}

// report is the outcome of comparing runs of two builds, or of describing
// the runs of one (when there are no B runs).
type report struct {
	nameA, nameB string
	runs         []run
	results      []result
	verdict      string
	regressed    []string
	single       bool
	parity       []string // one note per run, when it was measured
}

// analyse builds the report. floor is the smallest noise, in percent, a
// change has to exceed before it counts.
func analyse(runs []run, nameA, nameB string, floor float64) report {
	rep := report{nameA: nameA, nameB: nameB, runs: runs, single: true}
	for _, r := range runs {
		if r.build == "B" {
			rep.single = false
		}
	}
	few := false
	for _, name := range scenarioNames(runs) {
		res := result{name: name}
		var a, b []float64
		for _, r := range runs {
			row, present := r.s[name]
			v, ok := row.rps()
			res.values = append(res.values, cellValue{run: r.id(), rps: v, ok: present && ok})
			if !present {
				continue
			}
			if ok {
				if r.build == "A" {
					a = append(a, v)
				} else {
					b = append(b, v)
				}
			}
			if row.hasErrors() {
				res.errorRuns = append(res.errorRuns, r.id()+": "+row.errors)
			}
			if row.non2xx > 0 {
				res.non2xxRuns = append(res.non2xxRuns, fmt.Sprintf("%s: %d", r.id(), row.non2xx))
			}
		}
		res.meanA, res.spreadA = meanSpread(a)
		res.meanB, res.spreadB = meanSpread(b)
		res.change = math.NaN()
		if len(a) > 0 && len(b) > 0 && res.meanA > 0 {
			res.change = (res.meanB - res.meanA) / res.meanA * 100
		}
		if !rep.single {
			res.noise = math.Max(math.Max(nanZero(res.spreadA), nanZero(res.spreadB)), floor)
			res.verdict = judge(res.change, res.noise, len(a), len(b))
			switch res.verdict {
			case verdictRegression:
				rep.regressed = append(rep.regressed, name)
			case verdictFewRuns:
				few = true
			}
		}
		rep.results = append(rep.results, res)
	}
	switch {
	case rep.single:
		rep.verdict = ""
	case len(rep.regressed) > 0:
		rep.verdict = verdictRegression + ": " + strings.Join(rep.regressed, ", ")
	case few:
		rep.verdict = verdictFewRuns
	default:
		rep.verdict = verdictAllNoise
	}
	return rep
}

// judge is the per-scenario verdict.
func judge(change, noise float64, nA, nB int) string {
	if nA < 2 || nB < 2 || math.IsNaN(change) {
		return verdictFewRuns
	}
	switch {
	case change < -noise:
		return verdictRegression
	case change > noise:
		return verdictImprovement
	default:
		return verdictNoise
	}
}

// meanSpread returns the mean and the spread, (max-min)/mean in percent. The
// spread is NaN with fewer than two values.
func meanSpread(v []float64) (mean, spread float64) {
	if len(v) == 0 {
		return math.NaN(), math.NaN()
	}
	lo, hi, sum := v[0], v[0], 0.0
	for _, x := range v {
		sum += x
		lo, hi = math.Min(lo, x), math.Max(hi, x)
	}
	mean = sum / float64(len(v))
	if len(v) < 2 || mean == 0 {
		return mean, math.NaN()
	}
	return mean, (hi - lo) / mean * 100
}

func nanZero(x float64) float64 {
	if math.IsNaN(x) {
		return 0
	}
	return x
}

// scenarioNames lists every scenario the runs have: known ones in summary
// order, then any others alphabetically.
func scenarioNames(runs []run) []string {
	seen := map[string]bool{}
	for _, r := range runs {
		for n := range r.s {
			seen[n] = true
		}
	}
	var out []string
	for _, n := range summaryOrder {
		if seen[n] {
			out = append(out, n)
			delete(seen, n)
		}
	}
	var rest []string
	for n := range seen {
		rest = append(rest, n)
	}
	sort.Strings(rest)
	return append(out, rest...)
}

// interleave orders the runs of two builds as they would have run
// alternated: A1, B1, A2, B2, and so on.
func interleave(a, b []summary) []run {
	var out []run
	for i := 0; i < len(a) || i < len(b); i++ {
		if i < len(a) {
			out = append(out, run{build: "A", round: i + 1, s: a[i]})
		}
		if i < len(b) {
			out = append(out, run{build: "B", round: i + 1, s: b[i]})
		}
	}
	return out
}

// writeMarkdown prints the report as a Markdown table in the shape of the
// bench/phase-*.md records, then the notes and the verdict.
func (rep report) writeMarkdown(w io.Writer) {
	label := func(r run) string {
		if r.build == "A" {
			return fmt.Sprintf("%s %d", rep.nameA, r.round)
		}
		return fmt.Sprintf("%s %d", rep.nameB, r.round)
	}
	head := []string{"Scenario"}
	align := []string{"---"}
	for _, r := range rep.runs {
		head = append(head, label(r))
		align = append(align, "---:")
	}
	if rep.single {
		head = append(head, "Mean", "Spread")
		align = append(align, "---:", "---:")
	} else {
		head = append(head, "Mean change", "Spread "+rep.nameA, "Spread "+rep.nameB, "Verdict")
		align = append(align, "---:", "---:", "---:", "---")
	}
	fmt.Fprintf(w, "| %s |\n| %s |\n", strings.Join(head, " | "), strings.Join(align, " | "))
	var notes []string
	for _, res := range rep.results {
		name := res.name
		if len(res.errorRuns) > 0 || len(res.non2xxRuns) > 0 {
			name += " *"
		}
		cells := []string{name}
		for _, v := range res.values {
			if v.ok {
				cells = append(cells, formatRPS(v.rps))
			} else {
				cells = append(cells, missing)
			}
		}
		if rep.single {
			cells = append(cells, formatRPS(res.meanA), formatPct(res.spreadA, false))
		} else {
			cells = append(cells, formatPct(res.change, true), formatPct(res.spreadA, false),
				formatPct(res.spreadB, false), res.verdict)
		}
		fmt.Fprintf(w, "| %s |\n", strings.Join(cells, " | "))
		if len(res.errorRuns) > 0 {
			notes = append(notes, fmt.Sprintf("%s socket errors (percentiles cover only the requests served): %s",
				res.name, strings.Join(res.errorRuns, "; ")))
		}
		if len(res.non2xxRuns) > 0 {
			notes = append(notes, fmt.Sprintf("%s non-2xx/3xx responses: %s", res.name, strings.Join(res.non2xxRuns, "; ")))
		}
	}
	fmt.Fprintln(w)
	for _, n := range notes {
		fmt.Fprintf(w, "\\* %s\n", n)
	}
	if len(notes) > 0 {
		fmt.Fprintln(w)
	}
	if len(rep.parity) > 0 {
		fmt.Fprintf(w, "CPU parity before each run: %s.\n\n", strings.Join(rep.parity, "; "))
	}
	if !rep.single {
		fmt.Fprintf(w, "**Verdict:** %s.\n", rep.verdict)
	}
}

// writeRunsTSV writes one line per run and scenario.
func (rep report) writeRunsTSV(w io.Writer) {
	fmt.Fprintln(w, "scenario\tbuild\tround\treq_per_sec\tp50\tp99\terrors")
	for _, name := range scenarioNames(rep.runs) {
		for _, r := range rep.runs {
			row, ok := r.s[name]
			if !ok {
				continue
			}
			fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\t%s\n", name, r.build, r.round, row.rpsText, row.p50, row.p99, row.errors)
		}
	}
}

// formatRPS prints req/s the way the records do: whole numbers with
// thousands separators, one decimal under 100.
func formatRPS(v float64) string {
	if math.IsNaN(v) {
		return missing
	}
	if v < 100 {
		return strconv.FormatFloat(v, 'f', 1, 64)
	}
	s := strconv.FormatInt(int64(math.Round(v)), 10)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

func formatPct(v float64, signed bool) string {
	if math.IsNaN(v) {
		return missing
	}
	if signed {
		return strings.Replace(fmt.Sprintf("%+.1f %%", v), "-", "−", 1)
	}
	return fmt.Sprintf("%.1f %%", v)
}
