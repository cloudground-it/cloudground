package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestParseInstallFlags(t *testing.T) {
	f, err := parseInstallFlags("install", []string{"--plan", "--from", "/x/dist", "--skip-packages", "--container"})
	if err != nil || !f.plan || f.from != "/x/dist" || !f.skipPackages || !f.container {
		t.Fatalf("%+v %v", f, err)
	}
	for _, tc := range []struct {
		cmd  string
		args []string
		want string
	}{
		{"install", []string{"--from"}, "needs a directory"},
		{"install", []string{"--from", "a", "--release", "b"}, "exclude each other"},
		{"doctor", []string{"--plan"}, "unknown argument"},
		{"doctor", []string{"--from", "x"}, "unknown argument"},
		{"install", []string{"--wait"}, "unknown argument"},
	} {
		if _, err := parseInstallFlags(tc.cmd, tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s %v: err = %v, want %q", tc.cmd, tc.args, err, tc.want)
		}
	}
	if f, err := parseInstallFlags("doctor", []string{"--container", "--skip-packages"}); err != nil || !f.container || !f.skipPackages {
		t.Errorf("doctor flags: %+v %v", f, err)
	}
}

// A usage error prints the installer's usage and exits 2, before touching
// the machine.
func TestRunInstallUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runInstall("install", []string{"--bogus"}, &out, &errb); code != exitUsage {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errb.String(), "cgctl doctor") || out.Len() != 0 {
		t.Errorf("stderr %q stdout %q", errb.String(), out.String())
	}
}

// --remove-release-dir deletes the bootstrap's download however the run
// ends, here with a failed preflight (the test machine is not Ubuntu).
func TestRemoveReleaseDir(t *testing.T) {
	dir := t.TempDir() + "/dl"
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := runInstall("install", []string{"--plan", "--release", dir, "--remove-release-dir"}, &out, &errb); code == exitOK {
		t.Fatalf("plan on a non-Ubuntu test machine succeeded: %s", out.String())
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the release dir is still there: %v", err)
	}
	if _, err := parseInstallFlags("install", []string{"--remove-release-dir"}); err == nil {
		t.Error("--remove-release-dir without --release accepted")
	}
}
