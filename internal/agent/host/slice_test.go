package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestValidSlice(t *testing.T) {
	for name, want := range map[string]bool{
		"cg-site-3.slice": true, "cg.slice": true, "a_b.c.slice": true,
		"": false, ".slice": false, "-x.slice": false, "cg-site-3": false, "cg/x.slice": false,
		"cg site.slice": false, "cg-site-3.service": false, "x.slice\n": false,
	} {
		if ValidSlice(name) != want {
			t.Errorf("ValidSlice(%q) = %v", name, !want)
		}
	}
}

func TestTransientRunSliceIsChecked(t *testing.T) {
	sd := NewFakeSystemd()
	r := TransientRun{Unit: "cg-cron-x-1", Argv: []string{"/bin/true"}, User: "cg-site-1", Dir: "/srv", MaxRuntime: time.Minute, Slice: "x;y.slice"}
	if _, err := sd.RunTransient(context.Background(), r); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("a bad slice: %v", err)
	}
	r.Slice = "cg-site-1.slice"
	if _, err := sd.RunTransient(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if err := sd.Stop(context.Background(), "cg-site-1.slice"); err != nil {
		t.Fatalf("the slice of a run: %v", err)
	}
	if err := sd.Stop(context.Background(), "cg-site-2.slice"); !errors.Is(err, ErrNoSuchUnit) {
		t.Fatalf("an empty slice: %v", err)
	}
}

// The fake reads unit files and their drop-ins on a daemon reload, as
// systemd does; stopping a slice stops what is in it.
func TestFakeSystemdUnitDir(t *testing.T) {
	dir := t.TempDir()
	sd := NewFakeSystemd()
	sd.UnitDir = dir
	ctx := context.Background()
	unit := filepath.Join(dir, "php-a.service")
	if err := os.WriteFile(unit, []byte("[Service]\nExecStart=/bin/true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := sd.Start(ctx, "php-a.service"); !errors.Is(err, ErrNoSuchUnit) {
		t.Fatalf("started before a reload: %v", err)
	}
	if err := os.MkdirAll(unit+".d", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unit+".d", "slice.conf"), []byte("[Service]\nSlice=cg-site-1.slice\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := sd.DaemonReload(ctx); err != nil {
		t.Fatal(err)
	}
	if err := sd.Start(ctx, "php-a.service"); err != nil {
		t.Fatal(err)
	}
	if sd.SliceOf("php-a.service") != "cg-site-1.slice" {
		t.Fatalf("slice %q", sd.SliceOf("php-a.service"))
	}
	// Its file goes while it runs: it can still be stopped, by its slice.
	if err := os.Remove(unit); err != nil {
		t.Fatal(err)
	}
	if err := sd.DaemonReload(ctx); err != nil {
		t.Fatal(err)
	}
	if u, _ := sd.Unit("php-a.service"); u.LoadState != "not-found" || u.ActiveState != "active" {
		t.Fatalf("%+v", u)
	}
	if err := sd.Stop(ctx, "cg-site-1.slice"); err != nil {
		t.Fatal(err)
	}
	if _, ok := sd.Unit("php-a.service"); ok {
		t.Fatal("a stopped unit with no file is still loaded")
	}
}
