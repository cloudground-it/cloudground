package host

import (
	"slices"
	"testing"
)

func TestSystemdAtLeast(t *testing.T) {
	for out, want := range map[string]bool{
		"systemd 255 (255.4-1ubuntu8)\n+PAM +AUDIT": true,
		"systemd 254 (254)":                         true,
		"systemd 253 (253.5)":                       false,
		"systemd 259 (259-1ubuntu1)":                true,
		"":                                          false,
		"garbage":                                   false,
	} {
		if got := systemdAtLeast(out, noExpandSince); got != want {
			t.Errorf("%q: %v, want %v", out, got, want)
		}
	}
}

// A "$VAR" in a run's command reaches the command as written: with
// --expand-environment=no where systemd-run has it, else doubled so that
// systemd's own expansion gives it back.
func TestServiceArgsNeverExpandsTheCommand(t *testing.T) {
	cmd := []string{"/usr/bin/wp", "search-replace", "$HOME", "${PATH}x"}
	if got := commandArgv(cmd, true); !slices.Equal(got, cmd) {
		t.Errorf("expansion off: %q", got)
	}
	if got := commandArgv(cmd, false); !slices.Equal(got, []string{"/usr/bin/wp", "search-replace", "$$HOME", "$${PATH}x"}) {
		t.Errorf("expansion on: %q", got)
	}
	if cmd[2] != "$HOME" {
		t.Error("the caller's argv was changed")
	}
	old := noExpand
	noExpandOnce.Do(func() {})
	noExpand = true
	defer func() { noExpand = old }()
	a := ServiceArgs(Service{User: "cg-site-1", Argv: cmd})
	if !slices.Contains(a, "--expand-environment=no") || !slices.Equal(a[len(a)-4:], cmd) {
		t.Errorf("argv: %q", a)
	}
}
