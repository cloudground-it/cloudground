package cron

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/rootfile"
	"github.com/cloudground-it/cloudground/internal/cronjob"
	"github.com/cloudground-it/cloudground/internal/site"
)

// IsClient reports whether argv0 names the scheduled jobs' runner.
func IsClient(argv0 string) bool { return filepath.Base(argv0) == ClientName }

// wpEvents runs WordPress's due events in the live release ($1).
const wpEvents = `cd "$1" && exec wp cron event run --due-now --quiet >/dev/null 2>&1`

// ScheduledArgv is the systemd-run argv (binary included) of one
// scheduled run of a site's job ("wp" for WordPress's events): a transient
// service in the site's slice, as the site user, with the site's sandbox
// and ScheduledLimit, named after the job and the time so runs that overlap
// do not collide.
func ScheduledArgv(spec Spec, which string, now time.Time, sitesRoot string) ([]string, error) {
	if !site.ValidSystemUser(spec.User) {
		return nil, fmt.Errorf("invalid site user %q", spec.User)
	}
	if filepath.Clean(spec.Root) != spec.Root || filepath.Dir(spec.Root) != filepath.Clean(sitesRoot) || filepath.Base(spec.Root) != spec.Domain || spec.Domain == "" {
		return nil, fmt.Errorf("the site's root %q is not a site", spec.Root)
	}
	var argv []string
	switch {
	case which == "wp":
		if !spec.WordPress {
			return nil, errors.New("not a WordPress site")
		}
		argv = []string{"/bin/sh", "-c", wpEvents, "cg-cron", spec.Root + "/current"}
	default:
		id, err := strconv.ParseInt(which, 10, 64)
		cmd, ok := spec.Jobs[which]
		if err != nil || id <= 0 || !ok {
			return nil, fmt.Errorf("no cron job %q for %s", which, spec.User)
		}
		if err := cronjob.ValidateCommand(cmd); err != nil {
			return nil, err
		}
		argv = []string{"/bin/sh", "-c", cronjob.Wrapper, "cg-cron", cmd, cronjob.LogPath(id)}
	}
	return append([]string{"systemd-run"}, host.ServiceArgs(host.Service{
		Unit:        fmt.Sprintf("cg-cron-%s-%s-%d.service", spec.User, which, now.Unix()),
		Description: fmt.Sprintf("CloudGround cron job %s of %s", which, spec.Domain),
		User:        spec.User, Slice: site.Slice(spec.User), Dir: spec.Root,
		Env: jobEnv(spec.Root), Props: sandboxOf(spec.Domain, spec.Root),
		MaxRuntime: ScheduledLimit, Argv: argv,
	})...), nil
}

// ClientMain is panel-agent run by cron as cg-site-cron <user> <job|wp>
// from a site's cron.d file, as root: it reads the site's spec and becomes
// systemd-run, which runs the job sandboxed and waits for it.
func ClientMain(args []string) int {
	if len(args) != 3 {
		fmt.Fprintln(os.Stderr, ClientName+": usage: "+ClientName+" <site user> <job id|wp>")
		return 2
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, ClientName+": runs as root, from cron")
		return 2
	}
	user, which := args[1], args[2]
	if !site.ValidSystemUser(user) {
		fmt.Fprintln(os.Stderr, ClientName+": invalid site user")
		return 2
	}
	b, err := rootfile.ReadNoFollow(SpecFile(DefaultSpecDir, user))
	if err != nil {
		fmt.Fprintln(os.Stderr, ClientName+":", err)
		return 1
	}
	var spec Spec
	if err := json.Unmarshal(b, &spec); err != nil || spec.User != user {
		fmt.Fprintln(os.Stderr, ClientName+": bad spec for", user)
		return 1
	}
	argv, err := ScheduledArgv(spec, which, time.Now(), "/srv/sites")
	if err != nil {
		fmt.Fprintln(os.Stderr, ClientName+":", err)
		return 1
	}
	bin, err := exec.LookPath(argv[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, ClientName+":", err)
		return 1
	}
	env := []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"}
	if which == "wp" {
		// WordPress's own events have no record to keep.
		err = syscall.Exec(bin, argv, env)
		fmt.Fprintln(os.Stderr, ClientName+": "+strings.TrimSpace(err.Error()))
		return 1
	}
	id, _ := strconv.ParseInt(which, 10, 64) // ScheduledArgv checked it
	return runAndRecord(bin, argv, env, DefaultRunDir, user, id)
}

// runAndRecord runs the job's systemd-run (which waits for the run) with
// the runner's own stdio, records how it ended, and exits with its status.
func runAndRecord(bin string, argv, env []string, runDir, user string, id int64) int {
	cmd := exec.Command(bin, argv[1:]...)
	cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = env, os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		code = ee.ExitCode()
	default:
		fmt.Fprintln(os.Stderr, ClientName+": "+strings.TrimSpace(err.Error()))
		return 1
	}
	if err := recordRun(runDir, user, id, runOf(time.Now(), code)); err != nil {
		fmt.Fprintln(os.Stderr, ClientName+": could not record the run:", err)
	}
	return code
}
