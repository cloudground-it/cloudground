package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/install"
)

// a machine root with an installed CloudGround binary and a site tree.
func uninstallRoot(t *testing.T) *install.Env {
	root := t.TempDir()
	for p, body := range map[string]string{
		"usr/local/bin/panel-agent":           "agent",
		"srv/sites/a.test/current/index.html": "page",
	} {
		os.MkdirAll(filepath.Join(root, filepath.Dir(p)), 0o755)
		os.WriteFile(filepath.Join(root, p), []byte(body), 0o644)
	}
	env := install.System(nil)
	env.Root = root
	env.Sd = nil // no systemd here
	return env
}

// --remove-sites deletes data: a hostname that does not match changes
// nothing, and neither does a "no" to the plain uninstall.
func TestUninstallAsksFirst(t *testing.T) {
	for _, c := range []struct {
		f     uninstallFlags
		stdin string
	}{
		{uninstallFlags{removeSites: true}, "not-this-host\n"},
		{uninstallFlags{removeSites: true, yes: true}, "\n"},
		{uninstallFlags{}, "n\n"},
	} {
		env := uninstallRoot(t)
		var out, errb bytes.Buffer
		code := uninstall(context.Background(), env, c.f, "box-1", "/var/lib/cloudground/state.db", strings.NewReader(c.stdin), &out, &errb)
		if code != exitFailed || !strings.Contains(errb.String(), "nothing was changed") {
			t.Errorf("%+v: exit %d: %s", c.f, code, errb.String())
		}
		for _, p := range []string{"usr/local/bin/panel-agent", "srv/sites/a.test/current/index.html"} {
			if _, err := os.Stat(filepath.Join(env.Root, p)); err != nil {
				t.Errorf("%+v: %s was removed", c.f, p)
			}
		}
	}
}

// The right hostname, from --confirm, goes ahead and deletes the sites.
func TestUninstallRemoveSitesWithTheHostname(t *testing.T) {
	env := uninstallRoot(t)
	var out, errb bytes.Buffer
	f := uninstallFlags{removeSites: true, confirm: "box-1"}
	if code := uninstall(context.Background(), env, f, "box-1", "/var/lib/cloudground/state.db", strings.NewReader(""), &out, &errb); code != exitOK {
		t.Fatalf("exit %d: %s %s", code, out.String(), errb.String())
	}
	for _, p := range []string{"usr/local/bin/panel-agent", "srv/sites"} {
		if _, err := os.Stat(filepath.Join(env.Root, p)); !os.IsNotExist(err) {
			t.Errorf("%s is still there", p)
		}
	}
	// Nothing is left: a second run says so.
	out.Reset()
	if code := uninstall(context.Background(), env, uninstallFlags{yes: true}, "box-1", "/var/lib/cloudground/state.db", strings.NewReader(""), &out, &errb); code != exitOK ||
		!strings.Contains(out.String(), "nothing to remove") {
		t.Fatalf("second run: exit %d: %s", code, out.String())
	}
}

func TestParseUninstallFlags(t *testing.T) {
	if _, err := parseUninstallFlags([]string{"--confirm", "x"}); err == nil {
		t.Error("--confirm without --remove-sites accepted")
	}
	f, err := parseUninstallFlags([]string{"--remove-sites", "--purge-packages", "--plan"})
	if err != nil || !f.removeSites || !f.purge || !f.plan {
		t.Fatalf("%+v %v", f, err)
	}
}
