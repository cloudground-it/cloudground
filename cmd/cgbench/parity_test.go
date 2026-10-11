package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

const stat0 = `cpu  1000 10 500 8000 20 0 30 40 0 0
cpu0 500 5 250 4000 10 0 15 20 0 0
intr 12345
`

const stat1 = `cpu  1100 10 550 8300 20 0 30 40 0 0
cpu0 550 5 275 4150 10 0 15 20 0 0
`

const stat2 = `cpu  1100 10 550 8290 20 0 30 50 0 0
`

func TestStealPercent(t *testing.T) {
	if s, ok := stealPercent(stat0, stat1); !ok || s != "0" {
		t.Errorf("no steal: %q %v", s, ok)
	}
	// 10 of 450 jiffies stolen: 2 %.
	if s, ok := stealPercent(stat0, stat2); !ok || s != "2" {
		t.Errorf("steal: %q %v", s, ok)
	}
	if _, ok := stealPercent("nonsense", stat1); ok {
		t.Error("unreadable stat accepted")
	}
	if _, ok := stealPercent(stat1, stat0); ok {
		t.Error("counters going backwards accepted")
	}
}

func TestCPUModel(t *testing.T) {
	info := "processor\t: 0\nvendor_id\t: GenuineIntel\nmodel name\t: Intel Core Processor (Haswell, no TSX)\nprocessor\t: 1\nmodel name\t: other\n"
	if m := cpuModel(info); m != "Intel Core Processor (Haswell, no TSX)" {
		t.Errorf("%q", m)
	}
	if cpuModel("") != "" {
		t.Error("empty")
	}
}

func TestParityOutputAndReadBack(t *testing.T) {
	p := parityResult{loopSeconds: "2.21", steal: "0", model: "Intel Core Processor (Haswell, no TSX)", cores: 2}
	var b bytes.Buffer
	p.write(&b)
	want := "cpu_loop_seconds 2.21\nsteal_percent 0\nmodel Intel Core Processor (Haswell, no TSX)\ncores 2\n"
	if b.String() != want {
		t.Errorf("%q", b.String())
	}
	if p.stolen() || !(parityResult{steal: "3"}).stolen() || (parityResult{steal: "unknown"}).stolen() {
		t.Error("stolen")
	}
	path := filepath.Join(t.TempDir(), "parity.txt")
	writeFile(t, path, want)
	if back, ok := readParity(path); !ok || back != p {
		t.Errorf("%+v", back)
	}
}

func TestParityLoopRuns(t *testing.T) {
	if _, err := exec.LookPath("awk"); err != nil {
		t.Skip("no awk")
	}
	p := measureParity(context.Background(), 1000)
	if p.loopSeconds == "unknown" || p.cores < 1 {
		t.Errorf("%+v", p)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoopsFromEnvironment(t *testing.T) {
	if defaultLoops(func(string) string { return "" }) != 20_000_000 {
		t.Error("default")
	}
	if defaultLoops(func(string) string { return "500" }) != 500 {
		t.Error("LOOPS")
	}
}
