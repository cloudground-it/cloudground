package site

import "testing"

func TestShellUser(t *testing.T) {
	for in, want := range map[string]string{
		"cg-site-12": "cg-shell-12",
		"cg-site-1":  "cg-shell-1",
		"cg-site-":   "",
		"root":       "",
		"cg-site-1a": "",
	} {
		if got := ShellUser(in); got != want {
			t.Errorf("ShellUser(%q) = %q, want %q", in, got, want)
		}
		if want != "" {
			if back := SiteUserOfShell(want); back != in {
				t.Errorf("SiteUserOfShell(%q) = %q, want %q", want, back, in)
			}
		}
	}
	for _, bad := range []string{"cg-site-12", "cg-shell-", "cg-shell-x", "root", ""} {
		if got := SiteUserOfShell(bad); got != "" {
			t.Errorf("SiteUserOfShell(%q) = %q, want none", bad, got)
		}
	}
}
