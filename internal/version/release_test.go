package version

import (
	"testing"
)

func TestParseAndNewer(t *testing.T) {
	for v, want := range map[string]bool{
		"v1.2.3": true, "1.2.3": true, "v1.2": true, "v0.4.0-3-gabc123": true, "v010.0.1": true,
		"": false, "dev": false, "abc1234": false, "v1": false, "v1.2.3.4": false, "v1.x.0": false, "v-1.0.0": false,
	} {
		if _, ok := Parse(v); ok != want {
			t.Errorf("Parse(%q) ok=%v, want %v", v, ok, want)
		}
	}
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"v0.4.0", "v0.3.9", true},
		{"v0.10.0", "v0.9.0", true}, // numeric, not lexical
		{"v1.0.0", "v0.99.99", true},
		{"v0.3.0", "v0.3.0", false},
		{"v0.3", "v0.3.0", false},
		{"v0.2.9", "v0.3.0", false},
		{"v0.4.0", "dev", false},
		{"dev", "v0.1.0", false},
	} {
		if got := Newer(tc.a, tc.b); got != tc.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// The signed manifest's version line decides what may be installed: never an
// older signed release (a downgrade puts back every bug fixed since), never a
// release other than the one the operator approved.
func TestCheckUpdate(t *testing.T) {
	for _, tc := range []struct {
		name, manifest, requested, installed string
		ok                                   bool
	}{
		{"newer, nothing requested", "v0.4.0", "", "v0.3.0", true},
		{"newer, latest", "v0.4.0", "latest", "v0.3.0", true},
		{"newer, as requested", "v0.4.0", "v0.4.0", "v0.3.0", true},
		{"requested without the v", "v0.4.0", "0.4.0", "v0.3.0", true},
		{"development build cannot order itself", "v0.1.0", "", "dev", true},
		{"older release", "v0.2.9", "", "v0.3.0", false},
		{"same release", "v0.3.0", "", "v0.3.0", false},
		{"older even when requested", "v0.2.9", "v0.2.9", "v0.3.0", false},
		{"other than requested", "v0.4.0", "v0.5.0", "v0.3.0", false},
		{"older than requested", "v0.3.1", "v0.4.0", "v0.3.0", false},
		{"unparseable request", "v0.4.0", "tomorrow", "v0.3.0", false},
		{"manifest without a version", "", "", "v0.3.0", false},
	} {
		err := CheckUpdate(tc.manifest, tc.requested, tc.installed)
		if (err == nil) != tc.ok {
			t.Errorf("%s: CheckUpdate(%q, %q, %q) = %v, want ok=%v", tc.name, tc.manifest, tc.requested, tc.installed, err, tc.ok)
		}
	}
}
