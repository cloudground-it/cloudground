package state

import (
	"errors"
	"slices"
	"testing"
)

// A function is created with an id, read by name with its domains, changed
// in one transaction, and deleted with its domains and every secret of its
// own (internal/state SPEC, Functions).
func TestFunctions(t *testing.T) {
	s := testStore(t)
	f, err := s.CreateFunction(Function{Name: "hello", Runtime: "node", Version: "22", MemoryMB: 128, TimeoutSec: 10,
		Env: map[string]string{"A": "1"}, Secrets: []string{"KEY"}, CORSOrigins: []string{"*"}})
	if err != nil || f.ID == 0 || f.Auth != FunctionPublic || f.CreatedAt == "" {
		t.Fatalf("%+v %v", f, err)
	}
	if _, err := s.CreateFunction(Function{Name: "hello", Runtime: "node", Version: "22"}); !errors.Is(err, ErrFunctionNameTaken) {
		t.Fatalf("a second function named hello: %v", err)
	}
	other, err := s.CreateFunction(Function{Name: "other", Runtime: "python", Version: "3.12"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetFunctionDomains(f.ID, []string{"b.example.com", "a.example.com"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetFunctionDomains(other.ID, []string{"a.example.com"}); !errors.Is(err, ErrFunctionDomainTaken) {
		t.Fatalf("a domain of another function: %v", err)
	}
	got, err := s.GetFunction("hello")
	if err != nil || got.Env["A"] != "1" || !slices.Equal(got.Secrets, []string{"KEY"}) || !slices.Equal(got.CORSOrigins, []string{"*"}) ||
		!slices.Equal(got.Domains, []string{"a.example.com", "b.example.com"}) {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := s.GetFunction("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	doms, err := s.FunctionDomains()
	if err != nil || doms["a.example.com"] != "hello" || len(doms) != 2 {
		t.Fatalf("%v %v", doms, err)
	}

	up, err := s.UpdateFunction("hello", func(f *Function) error {
		f.Release, f.Auth, f.Name, f.ID = "01J0000000000000000000000A", FunctionToken, "renamed", 99
		return nil
	})
	if err != nil || up.Name != "hello" || up.ID != f.ID || up.Release == "" || up.Auth != FunctionToken || len(up.Domains) != 2 {
		t.Fatalf("%+v %v", up, err)
	}
	if _, err := s.UpdateFunction("missing", func(*Function) error { return nil }); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	if _, err := s.UpdateFunction("hello", func(f *Function) error { f.Release = "x"; return boom }); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if again, _ := s.GetFunction("hello"); again.Release != up.Release {
		t.Fatalf("an aborted change was stored: %+v", again)
	}

	list, err := s.ListFunctions()
	if err != nil || len(list) != 2 || list[0].Name != "hello" || len(list[0].Domains) != 2 || len(list[1].Domains) != 0 {
		t.Fatalf("%+v %v", list, err)
	}

	for _, n := range []string{FunctionSecret(f.ID, "KEY"), FunctionTokenSecret(f.ID), FunctionSecret(other.ID, "KEY")} {
		if err := s.SetSecret(n, "v"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteFunction(f.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteFunction(f.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	for n, want := range map[string]string{FunctionSecret(f.ID, "KEY"): "", FunctionTokenSecret(f.ID): "", FunctionSecret(other.ID, "KEY"): "v"} {
		if v, _ := s.Secret(n); v != want {
			t.Errorf("%s = %q after the deletion, want %q", n, v, want)
		}
	}
	if doms, _ := s.FunctionDomains(); len(doms) != 0 {
		t.Fatalf("domains left: %v", doms)
	}
}

// The functions domain and the migration's marker are panel settings.
func TestFunctionsSettings(t *testing.T) {
	s := testStore(t)
	if _, err := s.UpdatePanelSettings(func(p *PanelSettings) error {
		p.FunctionsDomain, p.FunctionsMigration = "fn.example.com", "done"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	p, err := s.PanelSettings()
	if err != nil || p.FunctionsDomain != "fn.example.com" || p.FunctionsMigration != "done" {
		t.Fatalf("%+v %v", p, err)
	}
}
