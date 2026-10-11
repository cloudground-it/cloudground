// Package siteshell is the login shell of a site's shell login
// (cg-shell-<id>, ops/sftp SPEC §5.4). panel-agent runs it when it is
// started as cg-site-shell, which is how sshd starts it: as the user, with
// no privilege. It runs nothing itself. It hands its standard input, output
// and error (the session's terminal, or sshd's pipes) to the agent's gate,
// which runs bash in a sandboxed systemd service in the site's slice, and it
// stays until that service ends, exiting with its status. A hang-up or a
// kill of this process stops the service.
package siteshell

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Name is the name panel-agent answers to as a login shell.
const Name = "cg-site-shell"

// Socket is the agent's gate.
const Socket = "/run/cloudground/shell.sock"

// answerTimeout bounds the wait for the gate's first answer.
const answerTimeout = 30 * time.Second

// Request is what the client asks of the gate, one JSON line.
type Request struct {
	// HasCommand: `ssh host <command>` (sshd's "-c <command>"); without it
	// the session is a login shell.
	HasCommand bool   `json:"has_command,omitempty"`
	Command    string `json:"command,omitempty"`
	Term       string `json:"term,omitempty"`
	// Env holds the language variables (LANG, LANGUAGE, LC_*, COLORTERM).
	Env []string `json:"env,omitempty"`
}

// Interactive reports a login shell rather than a command.
func (r Request) Interactive() bool { return !r.HasCommand }

var (
	termRe   = regexp.MustCompile(`^[A-Za-z0-9._+-]{1,64}$`)
	envKeyRe = regexp.MustCompile(`^(LANG|LANGUAGE|COLORTERM|LC_[A-Z]+)$`)
)

// maxCommand bounds a command (scp and rsync pass long ones).
const maxCommand = 32 << 10

// Check refuses a request the gate must not pass on: a command with a NUL,
// a terminal name or a variable that is not plain.
func (r Request) Check() error {
	if len(r.Command) > maxCommand || strings.ContainsRune(r.Command, 0) {
		return errors.New("bad command")
	}
	if !r.HasCommand && r.Command != "" {
		return errors.New("bad command")
	}
	if r.Term != "" && !termRe.MatchString(r.Term) {
		return errors.New("bad terminal name")
	}
	if len(r.Env) > 16 {
		return errors.New("too many variables")
	}
	for _, kv := range r.Env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || !envKeyRe.MatchString(k) || len(v) > 256 || strings.ContainsAny(v, "\x00\n\r") {
			return errors.New("bad variable")
		}
	}
	return nil
}

// Is reports whether argv0 names the gate client: "cg-site-shell" or, for a
// login shell, "-cg-site-shell", with or without a directory.
func Is(argv0 string) bool {
	return strings.TrimPrefix(filepath.Base(argv0), "-") == Name
}

// Main runs the client with the process's arguments and returns the exit
// status: the session's, or 1 when there is no session.
func Main(args []string) int {
	// This process runs as the site's uid outside the sandbox for the
	// whole session: not dumpable, so the shell (same uid) cannot reach
	// the host's mounts through /proc/<pid>/root, nor read its memory.
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		fmt.Fprintln(os.Stderr, Name+": cannot make the session safe:", err)
		return 1
	}
	req, err := parseArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, Name+":", err)
		return 2
	}
	req.Term = os.Getenv("TERM")
	if !termRe.MatchString(req.Term) {
		req.Term = ""
	}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if envKeyRe.MatchString(k) && len(v) <= 256 && !strings.ContainsAny(v, "\x00\n\r") && len(req.Env) < 16 {
			req.Env = append(req.Env, kv)
		}
	}
	code, err := Run(Socket, req, [3]*os.File{os.Stdin, os.Stdout, os.Stderr})
	if err != nil {
		fmt.Fprintln(os.Stderr, Name+": the site shell is not available:", err)
		return 1
	}
	return code
}

// Forced is the ForceCommand of the shell logins' sshd block (ops/sftp
// SPEC §5.3): sshd then always runs "<login shell> -c Forced", with the
// client's own command, if any, in SSH_ORIGINAL_COMMAND. A global
// Subsystem or ForceCommand in sshd_config can therefore never run anything
// but this client for a shell login.
const Forced = "cloudground-site-shell"

// parseArgs reads sshd's forms: no argument (a session), "-c <command>",
// and "-c Forced" with the client's command in SSH_ORIGINAL_COMMAND (set:
// that command; unset: a session).
func parseArgs(args []string) (Request, error) {
	return parseArgsEnv(args, os.LookupEnv)
}

func parseArgsEnv(args []string, lookup func(string) (string, bool)) (Request, error) {
	switch {
	case len(args) <= 1:
		return Request{}, nil
	case len(args) == 3 && args[1] == "-c" && args[2] == Forced:
		if cmd, ok := lookup("SSH_ORIGINAL_COMMAND"); ok {
			return Request{HasCommand: true, Command: cmd}, nil
		}
		return Request{}, nil
	case len(args) == 3 && args[1] == "-c":
		return Request{HasCommand: true, Command: args[2]}, nil
	}
	return Request{}, errors.New("usage: " + Name + " [-c command]")
}

// Run hands req and stdio to the gate at socket and waits for the
// session's end: its exit status, or an error when it never started.
func Run(socket string, req Request, stdio [3]*os.File) (int, error) {
	// Hang-ups and kills before the session ends stop it.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT)
	defer signal.Stop(sigs)

	d := net.Dialer{Timeout: answerTimeout}
	nc, err := d.Dial("unix", socket)
	if err != nil {
		return 0, fmt.Errorf("the agent cannot be reached: %w", err)
	}
	c := nc.(*net.UnixConn)
	defer c.Close()
	line, err := json.Marshal(req)
	if err != nil {
		return 0, err
	}
	rights := unix.UnixRights(int(stdio[0].Fd()), int(stdio[1].Fd()), int(stdio[2].Fd()))
	if _, _, err := c.WriteMsgUnix(append(line, '\n'), rights, nil); err != nil {
		return 0, fmt.Errorf("the agent cannot be reached: %w", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(answerTimeout))
	r := bufio.NewReaderSize(c, 512)
	first, err := r.ReadString('\n')
	if err != nil {
		return 0, fmt.Errorf("no answer from the agent: %w", err)
	}
	first = strings.TrimSpace(first)
	if msg, ok := strings.CutPrefix(first, "error "); ok {
		return 0, errors.New(msg)
	}
	if !strings.HasPrefix(first, "ok ") {
		return 0, fmt.Errorf("unexpected answer %q", first)
	}
	_ = c.SetReadDeadline(time.Time{})
	go func() {
		for range sigs {
			_, _ = c.Write([]byte("stop\n"))
		}
	}()
	last, err := r.ReadString('\n')
	if err != nil {
		return 255, nil // the agent went away; the session goes with the terminal
	}
	n, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(last), "exit "))
	if err != nil || !strings.HasPrefix(last, "exit ") {
		return 255, nil
	}
	return n, nil
}
