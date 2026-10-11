package install

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// installer/install.sh is the bootstrap of SPEC §6: short, everything in
// main, handing over to cgctl install with --from or --release.
func TestBootstrap(t *testing.T) {
	const path = "../../installer/install.sh"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if n := strings.Count(s, "\n"); n > 50 {
		t.Errorf("the bootstrap has %d lines; the installer belongs in internal/install", n)
	}
	for _, want := range []string{
		`exec "$CLOUDGROUND_LOCAL_BUILD/cgctl" install --from "$CLOUDGROUND_LOCAL_BUILD" "$@"`,
		`exec "$dl/cgctl" install --release "$dl" --remove-release-dir "$@"`,
		"CLOUDGROUND_VERSION=${CLOUDGROUND_VERSION:-@CGCTL_VERSION@}"[len("CLOUDGROUND_VERSION="):],
		"@CGCTL_SHA256@",
		"CLOUDGROUND_INSTALL_SHA256",
		"$base/latest/download",
		"--proto '=https' --proto-redir '=https'",
		"\nmain \"$@\"\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("the bootstrap lacks %q", want)
		}
	}
	if !strings.HasSuffix(s, "main \"$@\"\n") {
		t.Error("main must be called on the last line")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	if out, err := exec.Command("bash", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v %s", err, out)
	}
	// Cut off half way (curl | bash interrupted): nothing runs.
	half := s[:len(s)/2]
	out, _ := exec.Command("bash", "-c", half+"\necho RAN-SOMETHING").CombinedOutput()
	if strings.Contains(string(out), "cloudground:") {
		t.Errorf("a truncated bootstrap ran its checks: %s", out)
	}
	// On anything but Ubuntu 24.04/26.04 (the test image is Debian), or not as
	// root, it stops with the reason and runs no cgctl.
	cmd := exec.Command("bash", path)
	cmd.Env = append(os.Environ(), "CLOUDGROUND_LOCAL_BUILD=/nonexistent")
	out, err = cmd.CombinedOutput()
	if err == nil || !(strings.Contains(string(out), "LTS required") || strings.Contains(string(out), "run as root") || strings.Contains(string(out), "amd64 required")) {
		t.Errorf("on this machine the bootstrap should refuse: %v %s", err, out)
	}
}
