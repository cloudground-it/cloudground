package install

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/install/managed"
)

// testdata/golden/<distro> holds what the old installer/install.sh wrote,
// captured on 2026-10-10 by running its own lines (the heredocs and the sed
// edits, by line range of the committed script) inside ubuntu:24.04 (nginx
// 1.24.0, no HTTP/3) and ubuntu:26.04 (nginx 1.28.3, HTTP/3) containers.
// The new renders must be the same bytes, so the A/B of the old and new
// installer on a server compares like with like (SPEC §8). One deliberate
// change since (R-10.1): the comment lines of sysctl.conf, logrotate.conf,
// sshd.conf and renew-hook.sh are new text; their settings are the same.

func golden(t *testing.T, distro, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata/golden", distro, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func render(t *testing.T, path string, v Vars) string {
	t.Helper()
	f, ok := managed.Lookup(path)
	if !ok {
		t.Fatalf("%s is not managed", path)
	}
	b, err := Render(f, v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestGoldenRenders(t *testing.T) {
	for _, distro := range []string{"24.04", "26.04"} {
		for path, name := range map[string]string{
			managed.Sysctl:    "sysctl.conf",
			managed.SSHD:      "sshd.conf",
			managed.Logrotate: "logrotate.conf",
			managed.RenewHook: "renew-hook.sh",
		} {
			if got := render(t, path, Vars{}); got != golden(t, distro, name) {
				t.Errorf("%s: %s differs from what install.sh wrote", distro, path)
			}
		}
		if got := render(t, managed.Vhost, Vars{QUIC: true}); got != golden(t, distro, "vhost.quic") {
			t.Errorf("%s: the vhost with HTTP/3 differs:\n%s", distro, got)
		}
		if got := render(t, managed.Vhost, Vars{QUIC: false}); got != golden(t, distro, "vhost.noquic") {
			t.Errorf("%s: the vhost without HTTP/3 differs:\n%s", distro, got)
		}
		// What the distribution's own nginx got: the probe is nginx -V.
		native := strings.TrimSpace(golden(t, distro, "quic")) == "quic"
		if got := render(t, managed.Vhost, Vars{QUIC: native}); got != golden(t, distro, "vhost.native") {
			t.Errorf("%s: the vhost for this distribution's nginx differs", distro)
		}
		stock := golden(t, distro, "nginx.conf.stock")
		tuned := string(TuneNginxConf([]byte(stock)))
		if tuned != golden(t, distro, "nginx.conf.tuned") {
			t.Errorf("%s: the nginx.conf edit differs from install.sh's sed:\n%s", distro, tuned)
		}
		if string(TuneNginxConf([]byte(tuned))) == tuned {
			// The edit is not idempotent by itself (the marker guards it, as
			// before): a second pass would insert the lines again.
			t.Errorf("%s: TuneNginxConf changed nothing on its own output; the marker check is what keeps it once", distro)
		}
	}
}

// The modes install.sh left (stat in the capture container) are the modes
// in the managed list.
func TestGoldenModes(t *testing.T) {
	for _, distro := range []string{"24.04", "26.04"} {
		for _, line := range strings.Split(strings.TrimSpace(golden(t, distro, "modes")), "\n") {
			f := strings.Fields(line) // "644 root:root /etc/…"
			if len(f) != 3 {
				t.Fatalf("modes: %q", line)
			}
			mode, _ := strconv.ParseUint(f[0], 8, 32)
			m, ok := managed.Lookup(f[2])
			if !ok {
				continue // a directory
			}
			if uint64(m.Mode) != mode || f[1] != "root:root" {
				t.Errorf("%s: install.sh left %s %s, managed says %04o", f[2], f[0], f[1], m.Mode)
			}
		}
	}
}

// The edit on nginx.conf, line by line, as the sed expressions did it.
func TestTuneNginxConf(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"worker_processes auto;\nevents {\n\tworker_connections 768;\n}\n",
			"# cloudground-tuned\nworker_rlimit_nofile 65535;\nworker_processes auto;\nevents {\n\tworker_connections 4096;\n}\n"},
		// Only the first worker_processes at the start of a line; indented
		// ones and later ones are left.
		{"  worker_processes 2;\nworker_processes 1;\nworker_processes 3;\n",
			"  worker_processes 2;\n# cloudground-tuned\nworker_rlimit_nofile 65535;\nworker_processes 1;\nworker_processes 3;\n"},
		// No worker_processes: nothing inserted. The rest of a line is kept.
		{"events { worker_connections 1024; } \n    worker_connections  512; # note",
			"events { worker_connections 1024; } \n    worker_connections 4096; # note"},
		{"", ""},
	} {
		if got := string(TuneNginxConf([]byte(tc.in))); got != tc.want {
			t.Errorf("TuneNginxConf(%q)\n= %q\nwant %q", tc.in, got, tc.want)
		}
	}
}

// AGENTS.md rule 6: the settings measured as no win stay out (SPEC §4).
func TestRejectedTuningStaysOut(t *testing.T) {
	check := func(where, text string) {
		for _, l := range strings.Split(text, "\n") {
			l = strings.TrimSpace(l)
			if l == "" || strings.HasPrefix(l, "#") {
				continue
			}
			for _, bad := range RejectedTuning {
				if strings.Contains(l, bad) {
					t.Errorf("%s sets %s, which was measured as no win: %q", where, bad, l)
				}
			}
		}
	}
	for _, f := range managed.Files {
		if f.Kind != managed.Rendered {
			continue
		}
		for _, q := range []bool{false, true} {
			check(f.Path, render(t, f.Path, Vars{QUIC: q}))
		}
	}
	for _, distro := range []string{"24.04", "26.04"} {
		stock := golden(t, distro, "nginx.conf.stock")
		added := strings.TrimPrefix(string(TuneNginxConf([]byte(stock))), "")
		for _, l := range strings.Split(stock, "\n") {
			added = strings.Replace(added, l+"\n", "", 1)
		}
		check("the nginx.conf edit", added)
	}
	spec, err := os.ReadFile("SPEC.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, numbers := range []string{"9529 vs 9438 rps", "21216 vs 21447", "sustained 6642 rps, static 14246 rps", "sustained 9177 rps, static 19601 rps"} {
		if !strings.Contains(string(spec), numbers) {
			t.Errorf("SPEC.md §4 lost the measurement %q", numbers)
		}
	}
}
