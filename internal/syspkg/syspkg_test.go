package syspkg

import (
	"slices"
	"strings"
	"testing"
)

// #113: what the agent ensures at start is exactly what the installer
// installs but PHP's packages, nftables included.
func TestReleaseIsTheInstallersListButPHP(t *testing.T) {
	installer := append(slices.Clone(Base), Main("8.4")...)
	var notPHP []string
	for _, p := range installer {
		if !strings.HasPrefix(p, "php8.4-") {
			notPHP = append(notPHP, p)
		}
	}
	if got := Release(); !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(notPHP))) {
		t.Fatalf("Release() = %v, the installer's non-PHP packages %v", got, notPHP)
	}
	if !slices.Contains(Release(), "nftables") {
		t.Error("nftables is not ensured")
	}
}

// The lists are copies: a caller's change does not reach them.
func TestListsAreCopies(t *testing.T) {
	r, s, m := Release(), System(), Main("8.4")
	r[0], s[0], m[0] = "x", "x", "x"
	if Release()[0] == "x" || System()[0] == "x" || Main("8.4")[0] == "x" {
		t.Error("a list is shared with its caller")
	}
}
