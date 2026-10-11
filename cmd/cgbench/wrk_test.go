package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseWrk(t *testing.T) {
	cases := []struct {
		file string
		want row
	}{
		{"sustained.txt", row{rpsText: "9877.75", p50: "55.30ms", p99: "629.43ms", errors: "none"}},
		{"single.txt", row{rpsText: "6887.16", p50: "129.00us", p99: "250.00us", errors: "none"}},
		{"flood.txt", row{rpsText: "7863.04", p50: "196.27ms", p99: "1.43s",
			errors: "connect 0, read 0, write 0, timeout 1054"}},
		{"uncached.txt", row{rpsText: "5.99", p50: "1.64s", p99: "2.56s", errors: "none", non2xx: 12}},
		{"refused.txt", row{rpsText: "na", p50: "na", p99: "na", errors: "none"}},
	}
	for _, c := range cases {
		got := parseWrk("x", fixture(t, c.file))
		c.want.scenario = "x"
		if got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.file, got, c.want)
		}
	}
}

func TestRowRPS(t *testing.T) {
	if v, ok := (row{rpsText: "9877.75"}).rps(); !ok || v != 9877.75 {
		t.Errorf("got %v %v", v, ok)
	}
	if _, ok := (row{rpsText: "na"}).rps(); ok {
		t.Error("na must not be a number")
	}
	if (row{errors: "none"}).hasErrors() || !(row{errors: "connect 0, read 0, write 0, timeout 1"}).hasErrors() {
		t.Error("hasErrors")
	}
}

func TestSummaryRoundTrip(t *testing.T) {
	s := summary{
		"flood":            parseWrk("flood", fixture(t, "flood.txt")),
		"cached-sustained": parseWrk("cached-sustained", fixture(t, "sustained.txt")),
		"uncached":         parseWrk("uncached", fixture(t, "uncached.txt")),
	}
	var b bytes.Buffer
	if err := writeSummary(&b, s); err != nil {
		t.Fatal(err)
	}
	want := "scenario\treq_per_sec\tp50\tp99\terrors\n" +
		"cached-sustained\t9877.75\t55.30ms\t629.43ms\tnone\n" +
		"flood\t7863.04\t196.27ms\t1.43s\tconnect 0, read 0, write 0, timeout 1054\n" +
		"uncached\t5.99\t1.64s\t2.56s\tnone\n"
	if b.String() != want {
		t.Fatalf("summary:\n%s\nwant:\n%s", b.String(), want)
	}
	back, err := readSummary(strings.NewReader(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	for name, r := range s {
		r.non2xx = 0 // not a column
		if back[name] != r {
			t.Errorf("%s: %+v != %+v", name, back[name], r)
		}
	}
	if _, err := readSummary(strings.NewReader("a\tb\n")); err == nil {
		t.Error("a wrong header must be refused")
	}
	if _, err := readSummary(strings.NewReader(summaryHeader + "\nx\t1\n")); err == nil {
		t.Error("a short line must be refused")
	}
}

func TestWrkArgsMatchSpec(t *testing.T) {
	st, err := defaultSettings(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	base := "https://bench.example/"
	want := map[string]string{
		"cached-sustained": "-t4 -c500 -d60s --timeout 10s -H Accept-Encoding: gzip --latency https://bench.example/shop/",
		"cached-single":    "-t1 -c1 -d30s --timeout 10s -H Accept-Encoding: gzip --latency https://bench.example/shop/",
		"cached-spike":     "-t4 -c200 -d30s --timeout 10s -H Accept-Encoding: gzip --latency https://bench.example/shop/",
		"flood":            "-t8 -c2000 -d30s --timeout 5s -H Accept-Encoding: gzip --latency https://bench.example/shop/",
		"static":           "-t4 -c200 -d30s --timeout 10s -H Accept-Encoding: gzip --latency https://bench.example/bench-static.txt",
		"browsers":         "-t4 -c200 -d30s --timeout 10s -H Accept-Encoding: gzip --latency -s /lua/browsers.lua https://bench.example/shop/",
		"uncached":         "-t2 -c10 -d30s --timeout 10s -H Accept-Encoding: gzip --latency -s /lua/uncached.lua https://bench.example/shop/",
	}
	var order []string
	for _, sc := range runOrder {
		order = append(order, sc.name)
		if got := strings.Join(st.wrkArgs(sc, base, "/lua"), " "); got != want[sc.name] {
			t.Errorf("%s:\n got %s\nwant %s", sc.name, got, want[sc.name])
		}
	}
	wantOrder := []string{"cached-sustained", "cached-single", "cached-spike", "flood", "static", "browsers", "uncached"}
	if !reflect.DeepEqual(order, wantOrder) {
		t.Errorf("run order %v", order)
	}
}

func TestSettingsFromEnvironment(t *testing.T) {
	env := map[string]string{"PATH_CACHED": "/", "PATH_STATIC": "/x.txt", "DURATION": "30s", "SHORT": "5s", "LAT_TIMEOUT": "2s"}
	st, err := defaultSettings(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if st.pathCached != "/" || st.pathStatic != "/x.txt" || st.long != 30*time.Second ||
		st.short != 5*time.Second || st.latTimeout != 2*time.Second {
		t.Fatalf("%+v", st)
	}
	got := strings.Join(st.wrkArgs(runOrder[3], "https://h", "/l"), " ")
	if got != "-t8 -c2000 -d5s --timeout 5s -H Accept-Encoding: gzip --latency https://h/" {
		t.Errorf("flood keeps its own 5s timeout: %s", got)
	}
	if _, err := defaultSettings(func(k string) string {
		if k == "SHORT" {
			return "thirty"
		}
		return ""
	}); err == nil {
		t.Error("a bad duration must be refused")
	}
}

func TestPickScenarios(t *testing.T) {
	scs, err := pickScenarios("uncached, cached-sustained")
	if err != nil || len(scs) != 2 || scs[0].name != "cached-sustained" || scs[1].name != "uncached" {
		t.Fatalf("%v %v", scs, err)
	}
	if _, err := pickScenarios("nope"); err == nil {
		t.Error("unknown scenario accepted")
	}
	if all, _ := pickScenarios(""); len(all) != 7 {
		t.Error("empty list must mean all")
	}
}

func TestEmbeddedScripts(t *testing.T) {
	dir := t.TempDir()
	if err := writeScripts(dir); err != nil {
		t.Fatal(err)
	}
	for _, sc := range runOrder {
		if sc.script == "" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, sc.script))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "function request()") {
			t.Errorf("%s has no request function", sc.script)
		}
	}
	unc, _ := os.ReadFile(filepath.Join(dir, "uncached.lua"))
	if !strings.Contains(string(unc), `thread:set("tid"`) {
		t.Error("uncached.lua must number its threads, or query strings repeat across them")
	}
}
