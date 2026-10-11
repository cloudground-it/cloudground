package host

import (
	"strconv"
	"strings"
	"time"
)

// Service is a command run to completion as a site's user in a transient
// systemd service, by `systemd-run --wait`: the way a site's shell, its cron
// jobs and wp-cli run, all in the site's sandbox (Props, the PHP master's
// unit lines, phpfpm.Sandbox) and its slice. systemd-run reads the
// properties as unit-file text, so the very lines of the PHP master's unit
// apply. Its exit status is the command's.
type Service struct {
	Unit        string // "" lets systemd name it
	Description string
	User        string // the account (User= and Group=); required
	Slice       string
	Dir         string   // working directory
	Env         []string // KEY=value, the whole environment beyond systemd's own
	Props       []string // unit-file lines (Key=value), passed with -p
	// MaxRuntime is RuntimeMaxSec (0: none).
	MaxRuntime time.Duration
	// Pipe hands systemd-run's own stdin, stdout and stderr to the command
	// (--pipe); TTY instead gives it that terminal as its controlling one.
	Pipe bool
	TTY  string
	Argv []string
}

// SandboxedProps are the unit lines every sandboxed run gets beyond the
// site's sandbox: a stop that does not linger, and SIGPIPE delivered as to
// any shell pipeline (systemd ignores it in services by default).
var SandboxedProps = []string{"IgnoreSIGPIPE=no", "TimeoutStopSec=5"}

// ServiceArgs is the systemd-run argv (after the binary) for s.
func ServiceArgs(s Service) []string {
	a := []string{"--quiet", "--collect", "--wait", "--service-type=exec"}
	if expandOff() {
		// A "$VAR" in the command (a wp-cli search-replace value, a cron
		// job's line) is passed as written, not expanded by systemd.
		a = append(a, "--expand-environment=no")
	}
	if s.Unit != "" {
		a = append(a, "--unit="+s.Unit)
	}
	if s.Description != "" {
		a = append(a, "--description="+s.Description)
	}
	if s.Slice != "" {
		a = append(a, "--slice="+s.Slice)
	}
	a = append(a, "--uid="+s.User, "--gid="+s.User)
	if s.Dir != "" {
		a = append(a, "--working-directory="+s.Dir)
	}
	for _, p := range SandboxedProps {
		a = append(a, "-p", p)
	}
	for _, p := range s.Props {
		a = append(a, "-p", p)
	}
	if s.MaxRuntime > 0 {
		a = append(a, "-p", "RuntimeMaxSec="+strconv.FormatInt(int64(s.MaxRuntime/time.Second), 10))
	}
	for _, kv := range s.Env {
		a = append(a, "--setenv="+kv)
	}
	switch {
	case s.TTY != "":
		a = append(a, "-p", "TTYPath="+s.TTY, "-p", "StandardInput=tty-force",
			"-p", "StandardOutput=tty", "-p", "StandardError=tty")
	case s.Pipe:
		a = append(a, "--pipe")
	}
	return append(append(a, "--"), commandArgv(s.Argv, expandOff())...)
}

// SystemdCommand is the command ServiceArgs puts after "--" on this
// machine (commandArgv).
func SystemdCommand(argv []string) []string { return commandArgv(argv, expandOff()) }

// commandArgv is the command as systemd-run must be given it: as is when
// expansion is off, else with every "$" doubled, which systemd's expansion
// turns back into one (host SPEC: wpcli, ServiceArgs). The program itself
// (argv[0]) is never expanded.
func commandArgv(argv []string, off bool) []string {
	if off || len(argv) < 2 {
		return argv
	}
	out := append([]string{argv[0]}, argv[1:]...)
	for i := 1; i < len(out); i++ {
		out[i] = strings.ReplaceAll(out[i], "$", "$$")
	}
	return out
}
