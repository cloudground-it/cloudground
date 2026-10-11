//go:build hostintegration

// Lines installed with crontab(1) and run by the real cron daemon: a
// throwaway Debian/Ubuntu container, as root, with cron running. It waits
// for cron's next minute.
//
//	go test -tags hostintegration -run IntegrationCron ./internal/cronjob/
package cronjob

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIntegrationCron(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root in a throwaway container")
	}
	if _, err := exec.LookPath("crontab"); err != nil {
		t.Skip("no crontab")
	}
	const user = "cgit-cron"
	home := "/tmp/cgit-cron-home"
	exec.Command("userdel", user).Run()
	os.RemoveAll(home)
	if out, err := exec.Command("useradd", "--system", "--no-create-home", "--home-dir", home, "--shell", "/usr/sbin/nologin", user).CombinedOutput(); err != nil {
		t.Fatalf("useradd: %v %s", err, out)
	}
	t.Cleanup(func() { exec.Command("crontab", "-u", user, "-r").Run(); exec.Command("userdel", user).Run() })
	os.MkdirAll(filepath.Join(home, "logs"), 0o755)
	exec.Command("chown", "-R", user+":", home).Run()

	cmds := map[int64]string{
		1: `echo 50%; date +%Y >/dev/null && echo dated`,
		2: `echo "\%" '\%'`,
		3: `echo "it's"; exit 3`,
		4: `echo a # a comment`,
		5: `id -un; pwd`,
	}
	var b strings.Builder
	b.WriteString("PATH=/usr/local/bin:/usr/bin:/bin\n")
	for id, c := range cmds {
		l, err := Job{ID: id, Schedule: "* * * * *", Command: c}.Line()
		if err != nil {
			t.Fatal(err)
		}
		b.WriteString(l + "\n")
	}
	inst := exec.Command("crontab", "-u", user, "-")
	inst.Stdin = strings.NewReader(b.String())
	if out, err := inst.CombinedOutput(); err != nil {
		t.Fatalf("crontab refused the lines: %v %s\n%s", err, out, b.String())
	}

	want := map[int64][]string{
		1: {"50%\ndated\n", "status=0]"},
		2: {`\% \%` + "\n", "status=0]"},
		3: {"it's\n", "status=3]"},
		4: {"a\n", "status=0]"},
		5: {user + "\n" + home + "\n", "status=0]"},
	}
	deadline := time.Now().Add(130 * time.Second)
	for id, parts := range want {
		path := filepath.Join(home, LogPath(id))
		var got string
		for {
			b, _ := os.ReadFile(path)
			got = string(b)
			if strings.Contains(got, "[cloudground end") || time.Now().After(deadline) {
				break
			}
			time.Sleep(time.Second)
		}
		if !strings.Contains(got, "[cloudground start ") {
			t.Errorf("job %d: no start marker in %q", id, got)
		}
		for _, p := range parts {
			if !strings.Contains(got, p) {
				t.Errorf("job %d: log %q lacks %q", id, got, p)
			}
		}
		fmt.Printf("job %d log:\n%s\n", id, got)
	}
}
