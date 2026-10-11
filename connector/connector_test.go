package connector

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// phpRunner runs a PHP file of php/tests: with the php on PATH, else in the
// php:8.4-cli image. It skips the test when neither is available.
func phpRunner(t *testing.T) func(ini string, script string) ([]byte, error) {
	t.Helper()
	dir, err := filepath.Abs("php")
	if err != nil {
		t.Fatal(err)
	}
	if php, err := exec.LookPath("php"); err == nil {
		return func(ini, script string) ([]byte, error) {
			return exec.Command(php, "-d", ini, filepath.Join(dir, "tests", script)).CombinedOutput()
		}
	}
	docker, err := exec.LookPath("docker")
	if err != nil || exec.Command(docker, "info").Run() != nil {
		t.Skip("neither php nor a working docker is available")
	}
	image := os.Getenv("CG_PHP_IMAGE")
	if image == "" {
		image = "php:8.4-cli"
	}
	return func(ini, script string) ([]byte, error) {
		// Not root, so the unwritable-directory case means something.
		return exec.Command(docker, "run", "--rm", "-u", "65534:65534", "-v", dir+":/src:ro", image,
			"php", "-d", ini, "/src/tests/"+script).CombinedOutput()
	}
}

// The mu-plugin's suite (SPEC part 2) against the WordPress stand-in.
func TestConnectorPHP(t *testing.T) {
	run := phpRunner(t)
	out, err := run("display_errors=stderr", "connector.php")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	t.Logf("%s", out)
}

// Two-factor sign-in (SPEC 2.10): its own process, because the mode is read
// from the environment when the mu-plugin loads.
func TestTwoFactorPHP(t *testing.T) {
	run := phpRunner(t)
	out, err := run("display_errors=stderr", "two-factor.php")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	t.Logf("%s", out)
}

// Hardening (SPEC 2.12), in its own process for the same reason.
func TestHardeningPHP(t *testing.T) {
	run := phpRunner(t)
	out, err := run("display_errors=stderr", "hardening.php")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	t.Logf("%s", out)
}

// The drop-in's suite (SPEC part 3) as wp-cli runs it, with APCu off; and
// as PHP-FPM does, where the APCu extension is installed (test/flush/run.sh
// always runs it that way).
func TestObjectCacheDropin(t *testing.T) {
	run := phpRunner(t)
	for _, cli := range []string{"0", "1"} {
		t.Run("apc.enable_cli="+cli, func(t *testing.T) {
			out, err := run("apc.enable_cli="+cli, "object-cache.php")
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			t.Logf("%s", out)
		})
	}
}

// Both files carry their licence, and the connector has one way left to
// flush the object cache across processes (SPEC 3.4).
func TestEmbeddedFiles(t *testing.T) {
	for name, src := range map[string]string{"connector": PHP, "drop-in": ObjectCacheAPCu} {
		if !strings.Contains(src, "SPDX-License-Identifier: GPL-2.0-or-later") {
			t.Errorf("%s: no GPL-2.0-or-later header", name)
		}
		if !strings.HasPrefix(src, "<?php\n") {
			t.Errorf("%s: does not start with <?php", name)
		}
	}
	if strings.Contains(PHP, "cloudground_flush") {
		t.Error("the connector still has the FPM flush endpoint")
	}
	if !strings.Contains(ObjectCacheAPCu, "'.cloudground-cache-epoch'") {
		t.Error("the drop-in no longer reads the generation file the agent writes (ops/wp sharedEpoch)")
	}
}
