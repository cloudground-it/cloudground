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

// The end of the run lists the kept repositories without the password a
// rest: URL carries: it goes to the terminal and to whatever log captures
// the run, while the credentials belong in backup.env only.
func TestUninstallMasksRepositoryPasswords(t *testing.T) {
	env := uninstallRoot(t)
	kept := filepath.Join(env.Root, install.DefaultKeptDir)
	os.MkdirAll(kept, 0o700)
	readme := "The backup repositories were NOT touched; they still hold every backup:\n" +
		"  rest:https://cg:s3cr3t-pw@backup.example.com:8000/cg/\n  /var/backups/cloudground/repo\n"
	os.WriteFile(filepath.Join(kept, install.KeptReadme), []byte(readme), 0o600)
	var out bytes.Buffer
	printKeptAfter(&out, install.Inventory{}, install.UninstallOptions{KeptDir: install.DefaultKeptDir}, env)
	if strings.Contains(out.String(), "s3cr3t-pw") {
		t.Errorf("the repository's password is printed:\n%s", out.String())
	}
	for _, want := range []string{"rest:https://cg:***@backup.example.com:8000/cg/", "/var/backups/cloudground/repo"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("%q is not listed:\n%s", want, out.String())
		}
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

// withNginx makes nginx "installed" and systemd "running" for the summary.
type withNginx struct{ install.Commands }

func (withNginx) LookPath(name string) bool { return name == "nginx" }

type runningSd struct{ install.Systemd }

func (runningSd) Running() bool { return true }

// The closing summary says nginx was stopped and how to start it again
// (#212), and says nothing when it was not touched.
func TestUninstallSummaryTellsNginxIsStopped(t *testing.T) {
	env := uninstallRoot(t)
	opts := install.UninstallOptions{KeptDir: install.DefaultKeptDir}
	var out bytes.Buffer
	printKeptAfter(&out, install.Inventory{}, opts, env)
	if strings.Contains(out.String(), "nginx") {
		t.Errorf("the summary mentions nginx, which was not touched:\n%s", out.String())
	}
	env.Cmd, env.Sd = withNginx{}, runningSd{}
	out.Reset()
	printKeptAfter(&out, install.Inventory{}, opts, env)
	for _, want := range []string{"nginx was stopped and disabled", "systemctl enable --now nginx"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the summary lacks %q:\n%s", want, out.String())
		}
	}
}
