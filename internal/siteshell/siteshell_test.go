package siteshell

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestIs(t *testing.T) {
	for argv0, want := range map[string]bool{
		"cg-site-shell":                true,
		"-cg-site-shell":               true,
		"/usr/local/bin/cg-site-shell": true,
		"panel-agent":                  false,
		"/usr/local/bin/panel-agent":   false,
		"cg-site-shell2":               false,
	} {
		if got := Is(argv0); got != want {
			t.Errorf("Is(%q) = %v, want %v", argv0, got, want)
		}
	}
}

func TestParseArgs(t *testing.T) {
	req, err := parseArgs([]string{"-cg-site-shell"})
	if err != nil || !req.Interactive() {
		t.Fatalf("login: %+v %v", req, err)
	}
	req, err = parseArgs([]string{"cg-site-shell", "-c", "git --version; id"})
	if err != nil || req.Interactive() || req.Command != "git --version; id" {
		t.Fatalf("command: %+v %v", req, err)
	}
	for _, bad := range [][]string{{"x", "-i"}, {"x", "-c"}, {"x", "-c", "a", "b"}, {"x", "--rcfile", "/tmp/x"}} {
		if _, err := parseArgs(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// Under the sshd block's ForceCommand the client gets "-c Forced" and reads
// the client's command from SSH_ORIGINAL_COMMAND; unset, it is a session.
func TestParseArgsForced(t *testing.T) {
	env := map[string]string{}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	req, err := parseArgsEnv([]string{"cg-site-shell", "-c", Forced}, lookup)
	if err != nil || !req.Interactive() {
		t.Fatalf("forced session: %+v %v", req, err)
	}
	env["SSH_ORIGINAL_COMMAND"] = "git pull"
	req, err = parseArgsEnv([]string{"cg-site-shell", "-c", Forced}, lookup)
	if err != nil || req.Interactive() || req.Command != "git pull" {
		t.Fatalf("forced command: %+v %v", req, err)
	}
	env["SSH_ORIGINAL_COMMAND"] = "internal-sftp"
	req, _ = parseArgsEnv([]string{"cg-site-shell", "-c", Forced}, lookup)
	if req.Command != "internal-sftp" {
		t.Fatalf("a subsystem request is a command in the sandbox, not sshd's SFTP: %+v", req)
	}
}

func TestRequestCheck(t *testing.T) {
	good := []Request{
		{}, {HasCommand: true, Command: "scp -t ."}, {Term: "xterm-256color", Env: []string{"LANG=it_IT.UTF-8", "LC_ALL=C"}},
	}
	for _, r := range good {
		if err := r.Check(); err != nil {
			t.Errorf("%+v: %v", r, err)
		}
	}
	bad := []Request{
		{HasCommand: true, Command: "a\x00b"},
		{Command: "id"}, // a command without -c
		{HasCommand: true, Command: strings.Repeat("x", maxCommand+1)},
		{Term: "xterm\nX"}, {Term: "$(id)"},
		{Env: []string{"LD_PRELOAD=/x.so"}}, {Env: []string{"PATH=/tmp"}}, {Env: []string{"LANG=a\nb"}}, {Env: []string{"LANG"}},
	}
	for _, r := range bad {
		if err := r.Check(); err == nil {
			t.Errorf("%+v accepted", r)
		}
	}
}

// fakeGate answers one connection with replies, after checking the request
// carries three descriptors; it returns what it read after the request.
func fakeGate(t *testing.T, replies ...string) (string, chan string) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "g.sock")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	after := make(chan string, 1)
	go func() {
		c, err := l.AcceptUnix()
		if err != nil {
			return
		}
		defer c.Close()
		buf, oob := make([]byte, 4096), make([]byte, unix.CmsgSpace(3*4))
		n, oobn, _, _, err := c.ReadMsgUnix(buf, oob)
		if err != nil {
			return
		}
		msgs, _ := unix.ParseSocketControlMessage(oob[:oobn])
		nfd := 0
		for _, m := range msgs {
			fds, _ := unix.ParseUnixRights(&m)
			nfd += len(fds)
			for _, fd := range fds {
				unix.Close(fd)
			}
		}
		var req Request
		if nfd != 3 || json.Unmarshal([]byte(strings.TrimSpace(string(buf[:n]))), &req) != nil {
			_, _ = c.Write([]byte("error bad request\n"))
			return
		}
		r := bufio.NewReader(c)
		for _, reply := range replies {
			if reply == "<wait stop>" {
				l, _ := r.ReadString('\n')
				after <- l
				continue
			}
			_, _ = c.Write([]byte(reply))
		}
	}()
	return sock, after
}

func stdio(t *testing.T) [3]*os.File {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close(); w.Close() })
	return [3]*os.File{r, w, w}
}

func TestRunWaitsForTheSessionsStatus(t *testing.T) {
	sock, _ := fakeGate(t, "ok cg-shell-1-9.service\n", "exit 7\n")
	if code, err := Run(sock, Request{}, stdio(t)); err != nil || code != 7 {
		t.Fatalf("Run = %d %v", code, err)
	}
	sock, _ = fakeGate(t, "error no site shell for this user\n")
	if _, err := Run(sock, Request{}, stdio(t)); err == nil || err.Error() != "no site shell for this user" {
		t.Fatalf("refused: %v", err)
	}
	sock, _ = fakeGate(t, "okay\n")
	if _, err := Run(sock, Request{}, stdio(t)); err == nil {
		t.Fatal("a wrong answer accepted")
	}
	// The agent going away mid-session: no status, 255.
	sock, _ = fakeGate(t, "ok u\n")
	if code, err := Run(sock, Request{}, stdio(t)); err != nil || code != 255 {
		t.Fatalf("agent gone: %d %v", code, err)
	}
	if _, err := Run(filepath.Join(t.TempDir(), "none.sock"), Request{}, stdio(t)); err == nil {
		t.Fatal("no gate accepted")
	}
}

func TestRunStopsTheSessionOnAHangUp(t *testing.T) {
	sock, after := fakeGate(t, "ok u\n", "<wait stop>", "exit 129\n")
	done := make(chan int, 1)
	go func() {
		code, _ := Run(sock, Request{}, stdio(t))
		done <- code
	}()
	// The client is waiting on its session: hang it up.
	time.Sleep(300 * time.Millisecond)
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	select {
	case l := <-after:
		if l != "stop\n" {
			t.Fatalf("after a hang-up the client sent %q", l)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no stop after a hang-up")
	}
	if code := <-done; code != 129 {
		t.Fatalf("status %d", code)
	}
}
