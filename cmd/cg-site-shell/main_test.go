package main

import (
	"go/build"
	"strings"
	"testing"
)

// The gate client stays small: of this module it imports siteshell and
// early only (nothing of the agent), and early depends on syscall alone, so
// its init, which makes the process not dumpable, runs before the init of
// any package that does more.
func TestImportsStayMinimal(t *testing.T) {
	const mod = "github.com/cloudground-it/cloudground/"
	seen := map[string]bool{}
	var walk func(path string)
	walk = func(path string) {
		if seen[path] || path == "C" {
			return
		}
		seen[path] = true
		if first, _, _ := strings.Cut(path, "/"); !strings.Contains(first, ".") {
			return // the standard library
		}
		p, err := build.Default.Import(path, ".", 0)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		for _, imp := range p.Imports {
			walk(imp)
		}
	}
	walk(mod + "cmd/cg-site-shell")
	for p := range seen {
		if strings.HasPrefix(p, mod) && p != mod+"cmd/cg-site-shell" && p != mod+"internal/siteshell" && p != mod+"internal/siteshell/early" {
			t.Errorf("cg-site-shell imports %s", p)
		}
	}
	early, err := build.Default.Import(mod+"internal/siteshell/early", ".", 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(early.Imports, ",") != "syscall" {
		t.Errorf("early imports %v, want syscall alone", early.Imports)
	}
}
