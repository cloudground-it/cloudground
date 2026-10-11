package version

import "testing"

// SPEC §1: every daemon must stop on a version or help request, and only
// on an argument that is exactly one.
func TestFlagStopsTheProcess(t *testing.T) {
	stops := [][]string{{"-v"}, {"--version"}, {"version"}, {"-h"}, {"--help"}, {"help"}, {"--socket", "/x", "-v"}}
	runs := [][]string{nil, {"serve"}, {"--socket", "/tmp/x"}, {"-version"}, {"--versions"}}
	for _, a := range stops {
		if out := stdout(t, func() {
			if !HandleFlag(a, "panel-agent", "summary") {
				t.Errorf("%q did not stop the process", a)
			}
		}); out == "" {
			t.Errorf("%q printed nothing", a)
		}
	}
	for _, a := range runs {
		if out := stdout(t, func() {
			if HandleFlag(a, "panel-agent", "summary") {
				t.Errorf("%q stopped the process", a)
			}
		}); out != "" {
			t.Errorf("%q printed %q", a, out)
		}
	}
}

func TestFlagOutput(t *testing.T) {
	old := Version
	t.Cleanup(func() { Version = old })
	Version = "v2.1.0"
	if got := stdout(t, func() { HandleFlag([]string{"--version"}, "cgctl", "s") }); got != "cgctl v2.1.0\n" {
		t.Errorf("version: %q", got)
	}
	if got := stdout(t, func() { HandleFlag([]string{"help", "-v"}, "cgctl", "Usage: cgctl") }); got != "cgctl v2.1.0\n\nUsage: cgctl\n" {
		t.Errorf("help: %q", got)
	}
}
