package main

import (
	"bytes"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
)

// recordedAB reads an alternated A/B table from one of the integrator's
// bench/phase-*.md records: | Scenario | A 1 | B 1 | A 2 | B 2 | Mean change |.
// It returns the runs and the published mean change per scenario.
func recordedAB(t *testing.T, file string) ([]run, map[string]float64) {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	a := []summary{{}, {}}
	bb := []summary{{}, {}}
	published := map[string]float64{}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "| ") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "| "), " | ")
		if len(cells) != 6 {
			continue
		}
		name := strings.TrimRight(strings.Fields(cells[0])[0], ",")
		if !knownScenario(name) {
			continue
		}
		for i, c := range cells[1:5] {
			r := row{scenario: name, rpsText: strings.ReplaceAll(c, ",", ""), p50: "na", p99: "na", errors: "none"}
			if i%2 == 0 {
				a[i/2][name] = r
			} else {
				bb[i/2][name] = r
			}
		}
		pct := strings.NewReplacer("\u2212", "-", "%", "", " ", "", "+", "").Replace(cells[5])
		v, err := strconv.ParseFloat(pct, 64)
		if err != nil {
			t.Fatalf("%s: mean change %q: %v", file, cells[5], err)
		}
		published[name] = v
	}
	if len(published) != 7 {
		t.Fatalf("%s: read %d scenarios, want 7", file, len(published))
	}
	return interleave(a, bb), published
}

func knownScenario(n string) bool {
	for _, s := range summaryOrder {
		if s == n {
			return true
		}
	}
	return false
}

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

// The tool's "mean change" is the one the records were computed with.
func TestMeanChangeReproducesRecords(t *testing.T) {
	for _, file := range []string{"../../bench/phase-3.md", "../../bench/phase-4-integration.md"} {
		runs, published := recordedAB(t, file)
		rep := analyse(runs, "A", "B", 0)
		for _, res := range rep.results {
			// The records print req/s to one decimal under 100 and computed
			// the change from the unrounded values: for uncached (about 14
			// req/s) the rounding alone moves the change by up to 0.7 points.
			tol := 0.051
			if res.meanA < 100 {
				tol = 0.7
			}
			if !near(res.change, published[res.name], tol) {
				t.Errorf("%s %s: change %.2f, record says %.1f", file, res.name, res.change, published[res.name])
			}
		}
	}
}

func resultFor(t *testing.T, rep report, name string) result {
	t.Helper()
	for _, r := range rep.results {
		if r.name == name {
			return r
		}
	}
	t.Fatalf("no %s in report", name)
	return result{}
}

// bench/phase-4-integration.md: two rounds flagged cached-sustained, and six
// alternated runs of that scenario alone showed it was noise.
func TestVerdictOnIntegrationRecord(t *testing.T) {
	runs, _ := recordedAB(t, "../../bench/phase-4-integration.md")
	rep := analyse(runs, "phase3", "integ", 0)

	sus := resultFor(t, rep, "cached-sustained")
	if sus.verdict != verdictRegression {
		t.Errorf("cached-sustained: %s (change %.1f, noise %.1f)", sus.verdict, sus.change, sus.noise)
	}
	flood := resultFor(t, rep, "flood")
	// integ alone ran 11,055 and 9,367: that spread covers the -10 %.
	if flood.verdict != verdictNoise || !near(flood.spreadB, 16.5, 0.1) {
		t.Errorf("flood: %s, spread B %.2f", flood.verdict, flood.spreadB)
	}
	if !strings.HasPrefix(rep.verdict, verdictRegression+": ") || !strings.Contains(rep.verdict, "cached-sustained") {
		t.Errorf("overall: %s", rep.verdict)
	}

	// The repetition, integ first: integ 12,607 / 12,481 / 12,966,
	// phase3 13,558 / 13,010 / 12,395.
	one := func(v string) summary {
		return summary{"cached-sustained": {scenario: "cached-sustained", rpsText: v, errors: "none"}}
	}
	again := interleave(
		[]summary{one("13558"), one("13010"), one("12395")},
		[]summary{one("12607"), one("12481"), one("12966")},
	)
	rep2 := analyse(again, "phase3", "integ", 0)
	r := resultFor(t, rep2, "cached-sustained")
	if !near(r.change, -2.3, 0.05) || !near(r.spreadA, 8.95, 0.05) || r.verdict != verdictNoise {
		t.Errorf("repeat: change %.2f spread %.2f %s", r.change, r.spreadA, r.verdict)
	}
	if rep2.verdict != verdictAllNoise {
		t.Errorf("repeat overall: %s", rep2.verdict)
	}
}

func TestVerdictOnPhase3Record(t *testing.T) {
	runs, _ := recordedAB(t, "../../bench/phase-3.md")
	rep := analyse(runs, "main", "phase3", 0)
	// The record concluded "no regression"; nothing went down by more than
	// its own spread.
	if len(rep.regressed) != 0 {
		t.Errorf("regressed: %v", rep.regressed)
	}
	for _, r := range rep.results {
		if r.change < 0 && r.verdict != verdictNoise {
			t.Errorf("%s: %.1f %s", r.name, r.change, r.verdict)
		}
	}
}

func TestJudge(t *testing.T) {
	cases := []struct {
		change, noise float64
		nA, nB        int
		want          string
	}{
		{-3, 5, 2, 2, verdictNoise},
		{5, 5, 2, 2, verdictNoise},
		{-5.1, 5, 2, 2, verdictRegression},
		{6, 5, 3, 3, verdictImprovement},
		{-20, 5, 1, 2, verdictFewRuns},
		{math.NaN(), 0, 2, 2, verdictFewRuns},
	}
	for _, c := range cases {
		if got := judge(c.change, c.noise, c.nA, c.nB); got != c.want {
			t.Errorf("judge(%v, %v, %d, %d) = %s, want %s", c.change, c.noise, c.nA, c.nB, got, c.want)
		}
	}
}

func TestFloorAndMissingRuns(t *testing.T) {
	s := func(v string) summary { return summary{"static": {scenario: "static", rpsText: v, errors: "none"}} }
	runs := interleave([]summary{s("100"), s("100")}, []summary{s("97"), s("97")})
	if r := analyse(runs, "A", "B", 0).results[0]; r.verdict != verdictRegression {
		t.Errorf("zero spread, -3 %%: %s", r.verdict)
	}
	if r := analyse(runs, "A", "B", 5).results[0]; r.verdict != verdictNoise {
		t.Errorf("5 %% floor: %s", r.verdict)
	}
	// One run of B produced nothing: one valid run is not enough.
	runs = interleave([]summary{s("100"), s("101")}, []summary{s("na"), s("99")})
	rep := analyse(runs, "A", "B", 0)
	if rep.results[0].verdict != verdictFewRuns || rep.verdict != verdictFewRuns {
		t.Errorf("%s / %s", rep.results[0].verdict, rep.verdict)
	}
}

func TestMarkdownReport(t *testing.T) {
	flood := parseWrk("flood", fixture(t, "flood.txt"))
	unc := parseWrk("uncached", fixture(t, "uncached.txt"))
	a := []summary{{"flood": flood, "uncached": unc}, {"flood": flood, "uncached": unc}}
	runs := interleave(a, a)
	rep := analyse(runs, "main", "next", 0)
	rep.parity = []string{"A1 2.21 s, steal 0"}
	var b bytes.Buffer
	rep.writeMarkdown(&b)
	out := b.String()
	for _, want := range []string{
		"| Scenario | main 1 | next 1 | main 2 | next 2 | Mean change | Spread main | Spread next | Verdict |",
		"| flood * | 7,863 | 7,863 | 7,863 | 7,863 | +0.0 % | 0.0 % | 0.0 % | within noise |",
		"| uncached * | 6.0 | 6.0 | 6.0 | 6.0 |",
		"flood socket errors (percentiles cover only the requests served): A1: connect 0, read 0, write 0, timeout 1054",
		"uncached non-2xx/3xx responses: A1: 12; B1: 12",
		"CPU parity before each run: A1 2.21 s, steal 0.",
		"**Verdict:** change within noise.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
	var tsv bytes.Buffer
	rep.writeRunsTSV(&tsv)
	if !strings.HasPrefix(tsv.String(), "scenario\tbuild\tround\treq_per_sec\tp50\tp99\terrors\nflood\tA\t1\t7863.04\t196.27ms\t1.43s\t") {
		t.Errorf("runs.tsv:\n%s", tsv.String())
	}
}

func TestSingleBuildReport(t *testing.T) {
	s := func(v string) summary {
		return summary{"cached-sustained": {scenario: "cached-sustained", rpsText: v, errors: "none"}}
	}
	rep := analyse(interleave([]summary{s("12607"), s("12481"), s("12966")}, nil), "integ", "", 0)
	var b bytes.Buffer
	rep.writeMarkdown(&b)
	if !strings.Contains(b.String(), "| cached-sustained | 12,607 | 12,481 | 12,966 | 12,685 | 3.8 % |") ||
		strings.Contains(b.String(), "Verdict") {
		t.Errorf("single-build report:\n%s", b.String())
	}
}

func TestFormat(t *testing.T) {
	for in, want := range map[float64]string{13862: "13,862", 999.6: "1,000", 13.6: "13.6", 5.99: "6.0", 123456789: "123,456,789"} {
		if got := formatRPS(in); got != want {
			t.Errorf("formatRPS(%v) = %s", in, got)
		}
	}
	if formatPct(-4.84, true) != "\u22124.8 %" || formatPct(3.24, true) != "+3.2 %" || formatPct(math.NaN(), true) != "na" {
		t.Error("formatPct")
	}
}
