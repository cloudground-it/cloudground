package host

import (
	"context"
	"errors"
	"testing"
)

func TestShellLoginArgv(t *testing.T) {
	s, fx := shadowWithDB(t, nil)
	ctx := context.Background()
	if err := s.CreateUser(ctx, UserSpec{Name: "cg-shell-1", System: true, Home: "/srv/sites/a", Shell: "/usr/local/bin/cg-site-shell",
		PrimaryGroup: "cg-site-1", Groups: []string{"cg-shell"}, UID: 998, NonUnique: true}); err != nil {
		t.Fatal(err)
	}
	want := "useradd --system --home-dir /srv/sites/a --no-create-home --shell /usr/local/bin/cg-site-shell --gid cg-site-1 --groups cg-shell --uid 998 --non-unique cg-shell-1"
	if got := fx.Calls()[0].Line(); got != want {
		t.Fatalf("argv %q, want %q", got, want)
	}
	if err := s.CreateUser(ctx, UserSpec{Name: "x", NonUnique: true}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("non-unique without a uid: %v", err)
	}
	if err := s.ForceDeleteUser(ctx, "cg-shell-1"); err != nil {
		t.Fatal(err)
	}
	if got := fx.Calls()[1].Line(); got != "userdel --force cg-shell-1" {
		t.Fatalf("argv %q", got)
	}
	if err := s.ForceDeleteUser(ctx, "-r"); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("an option as a name: %v", err)
	}
}
