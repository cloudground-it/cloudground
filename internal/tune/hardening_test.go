package tune

import (
	"strings"
	"testing"
)

func TestRenderEndsWithTheHardening(t *testing.T) {
	out, err := CalculateMariaDB(2048, 2, RoleShared, false).Render()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(out, "\n"+HardeningHeader+"\nlocal_infile = 0\nskip-symbolic-links\n") {
		t.Fatalf("the file does not end with the security settings:\n%s", out)
	}
	if HardeningBlock(out) != "" {
		t.Fatalf("a rendered file still lacks %q", HardeningBlock(out))
	}
}

func TestHardeningBlock(t *testing.T) {
	old := "[mysqld]\ninnodb_buffer_pool_size = 512M\n"
	if got, want := HardeningBlock(old), "\n"+HardeningHeader+"\nlocal_infile = 0\nskip-symbolic-links\n"; got != want {
		t.Fatalf("an old file gets %q, want %q", got, want)
	}
	if !HardeningNeedsRestart(HardeningBlock(old)) {
		t.Fatal("adding skip-symbolic-links needs no restart")
	}
	// A file from the first fix (local_infile only) gets the rest.
	if got := HardeningBlock(old + "\n" + HardeningHeader + "\nlocal_infile = 0\n"); got != "\nskip-symbolic-links\n" {
		t.Fatalf("a file with local_infile gets %q", got)
	}
	if HardeningNeedsRestart("\nlocal_infile = 0\n") {
		t.Fatal("local_infile alone needs a restart")
	}
	for _, has := range []string{"skip-symbolic-links", "skip_symbolic_links"} {
		if strings.Contains(HardeningBlock(old+has+"\n"), "symbolic") {
			t.Fatalf("%q counts as missing", has)
		}
	}
	for _, has := range []string{"local_infile = 0", "local-infile=0", "local_infile   =   0"} {
		if got := HardeningBlock(old + has + "\n"); strings.Contains(got, "local_infile") {
			t.Fatalf("%q counts as missing: %q", has, got)
		}
	}
	// A later line wins in an option file: a 1 after the 0 is not hardened.
	if got := HardeningBlock(old + "local_infile = 0\nlocal_infile = 1\n"); !strings.Contains(got, "local_infile = 0") {
		t.Fatalf("a later local_infile = 1 is not overridden: %q", got)
	}
	// A commented line is not a setting.
	if got := HardeningBlock(old + "# local_infile = 0\n"); !strings.Contains(got, "local_infile = 0") {
		t.Fatalf("a comment counts as the setting: %q", got)
	}
	// The header is written once.
	if got := HardeningBlock(old + "\n" + HardeningHeader + "\nlocal_infile = 1\n"); strings.Contains(got, HardeningHeader) {
		t.Fatalf("the header is repeated: %q", got)
	}
}
