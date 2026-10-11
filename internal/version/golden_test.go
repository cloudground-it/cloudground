package version

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The golden file pins what the version helpers answer over a matrix of
// inputs (Phase 5 gate). Captured from the code before the rewrite;
// regenerate with CG_GOLDEN_UPDATE=1 only for an intended change.

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if os.Getenv("CG_GOLDEN_UPDATE") != "" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (CG_GOLDEN_UPDATE=1 writes it)", err)
	}
	if string(want) != got {
		t.Fatalf("%s differs from the golden:\nwant:\n%s\ngot:\n%s", name, want, got)
	}
}

// stdout runs f and returns what it printed.
func stdout(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	f()
	os.Stdout = old
	w.Close()
	return <-done
}

func TestGoldenVersion(t *testing.T) {
	var b strings.Builder
	fmt.Fprintf(&b, "Version=%q UpdateURL=%q UpdatePublicKey=%q\n", Version, UpdateURL, UpdatePublicKey)
	for _, args := range [][]string{nil, {}, {"serve"}, {"-v"}, {"--version"}, {"version"}, {"-h"}, {"--help"}, {"help"},
		{"--socket", "/tmp/x", "--version"}, {"-version"}, {"--Version"}, {"serve", "help", "-v"}} {
		var stop bool
		out := stdout(t, func() { stop = HandleFlag(args, "panel-agent", "Usage: panel-agent [flags]") })
		fmt.Fprintf(&b, "HandleFlag(%q)=%v out=%q\n", args, stop, out)
	}
	old := Version
	Version = "v1.2.3"
	out := stdout(t, func() { HandleFlag([]string{"--version"}, "cgctl", "s") })
	Version = old
	fmt.Fprintf(&b, "HandleFlag(v1.2.3)=%q\n", out)

	tags := []string{"", "dev", "v1", "v1.2", "1.2.3", "v1.2.3", "v1.2.3-4-gabcdef", "v01.002.3", "v1.2.3.4",
		"v-1.2.3", "v1.x.3", " v2.0.0 ", "abc1234", "v0.0.0", "v10.0.0"}
	for _, a := range tags {
		v, ok := Parse(a)
		fmt.Fprintf(&b, "Parse(%q)=%v %v\n", a, v, ok)
	}
	for _, a := range tags {
		for _, c := range tags {
			fmt.Fprintf(&b, "Newer(%q,%q)=%v\n", a, c, Newer(a, c))
		}
	}
	for _, m := range []string{"", "v1.2.3", "v2.0.0"} {
		for _, r := range []string{"", "latest", " latest ", "v1.2.3", "v2.0.0", "bogus"} {
			for _, i := range []string{"", "dev", "v1.2.3", "v1.0.0", "v3.0.0"} {
				fmt.Fprintf(&b, "CheckUpdate(%q,%q,%q)=%v\n", m, r, i, CheckUpdate(m, r, i))
			}
		}
	}
	checkGolden(t, "version", b.String())
}
