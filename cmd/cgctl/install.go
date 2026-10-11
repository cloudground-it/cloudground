package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cloudground-it/cloudground/internal/install"
)

// The installer's subcommands (internal/install/SPEC.md §1). They run on the
// server as root and need no panel configuration, so main hands them over
// before the API client is set up.

const installUsage = `Usage:
  cgctl install [--plan] [--from <dir> | --release <dir>] [--skip-packages] [--container]
  cgctl doctor [--skip-packages] [--container]

install          install CloudGround on this server, or bring it up to date
  --plan         print what would change, change nothing
  --from <dir>   binaries from a local build (CLOUDGROUND_LOCAL_BUILD)
  --release <dir> binaries from a downloaded release, verified first
  --remove-release-dir  delete the --release directory at the end (the bootstrap's download)
  --skip-packages  no apt, no downloads
  --container    no systemd, kernel or firewall work (a container test)
doctor           check every step and report drift (exit 1 when any)
`

type installFlags struct {
	plan, skipPackages, container, removeRelease bool
	from, release                                string
}

func parseInstallFlags(cmd string, args []string) (installFlags, error) {
	var f installFlags
	for i := 0; i < len(args); i++ {
		a := args[i]
		val := func() (string, error) {
			if i+1 >= len(args) || args[i+1] == "" {
				return "", fmt.Errorf("%s needs a directory", a)
			}
			i++
			return args[i], nil
		}
		var err error
		switch {
		case a == "--plan" && cmd == "install":
			f.plan = true
		case a == "--skip-packages":
			f.skipPackages = true
		case a == "--container":
			f.container = true
		case a == "--from" && cmd == "install":
			f.from, err = val()
		case a == "--release" && cmd == "install":
			f.release, err = val()
		case a == "--remove-release-dir" && cmd == "install":
			f.removeRelease = true
		case a == "-h" || a == "--help" || a == "help":
			return f, errHelp
		default:
			return f, fmt.Errorf("%s: unknown argument %q", cmd, a)
		}
		if err != nil {
			return f, err
		}
	}
	if f.removeRelease && f.release == "" {
		return f, errors.New("install: --remove-release-dir needs --release")
	}
	if f.from != "" && f.release != "" {
		return f, errors.New("install: --from and --release exclude each other")
	}
	return f, nil
}

const installLog = "/var/log/cloudground-install.log"

// runInstall is `cgctl install` and `cgctl doctor`; it returns the exit code.
func runInstall(cmd string, args []string, stdout, stderr io.Writer) int {
	f, err := parseInstallFlags(cmd, args)
	if err != nil {
		if err != errHelp {
			fmt.Fprintln(stderr, "cgctl:", err)
		}
		fmt.Fprint(stderr, installUsage)
		return exitUsage
	}
	// The parents the installer creates (os.MkdirAll) take their mode from
	// the umask; a hardened root shell (027) would leave /var/www 0750 and
	// nginx could not reach the ACME webroot. Leaf modes are set explicitly.
	syscall.Umask(0o022)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if f.removeRelease {
		// The bootstrap's download goes, however the run ends.
		defer os.RemoveAll(f.release)
	}

	applying := cmd == "install" && !f.plan
	var logFile *os.File
	logPath := ""
	if applying {
		for _, p := range []string{installLog, "/tmp/cloudground-install.log"} {
			// O_NOFOLLOW: a link planted at the log's name is not written through.
			if lf, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600); err == nil {
				logFile, logPath = lf, p
				break
			}
		}
		if logFile != nil {
			defer logFile.Close()
		}
	}
	var log io.Writer = io.Discard
	if logFile != nil {
		log = logFile
	}
	env := install.System(log)

	ui := &install.UI{Out: stdout, TTY: isTTY(stdout)}
	if applying {
		ui.Banner()
	}
	fail := func(err error) int {
		if applying {
			ui.Fail(err, logPath, tail(logPath, 12))
		} else {
			fmt.Fprintln(stderr, "cgctl:", err)
		}
		return exitFailed
	}

	pre, err := env.RunPreflight(ctx, f.container || cmd == "doctor")
	if err != nil {
		return fail(err)
	}
	if len(pre.Problems) > 0 && (cmd == "install" || !pre.OS.Supported()) {
		fmt.Fprintln(stdout, "\n  This machine cannot run CloudGround yet:")
		for _, p := range pre.Problems {
			fmt.Fprintf(stdout, "    - %s\n", p)
			fmt.Fprintf(log, "preflight: %s\n", p)
		}
		return fail(fmt.Errorf("%d preflight problem(s), listed above", len(pre.Problems)))
	}
	fmt.Fprintln(log, pre.Summary)

	opts := install.Options{OS: pre.OS, Upgrade: pre.Upgrade, From: f.from, SkipPackages: f.skipPackages,
		Container: f.container, Doctor: cmd == "doctor"}
	if f.release != "" {
		installed := installedVersions(ctx)
		v, staged, err := install.VerifyRelease(install.ReleaseRequest{
			Dir:            f.release,
			PublicKey:      os.Getenv("CLOUDGROUND_RELEASE_PUBKEY"),
			Requested:      os.Getenv("CLOUDGROUND_VERSION"),
			AllowDowngrade: os.Getenv("CLOUDGROUND_ALLOW_DOWNGRADE") == "1",
			Installed:      installed,
		})
		if err != nil {
			return fail(err)
		}
		defer os.RemoveAll(staged)
		// Another release over an installed panel is an update, which only
		// `cgctl update` makes safely (SPEC §5 step 3a).
		if err := install.CheckSameRelease(pre.Upgrade, installedPanelAPI(ctx), v); err != nil {
			return fail(err)
		}
		fmt.Fprintf(log, "release %s verified, staged in %s\n", v, staged)
		opts.Release = staged
	}
	steps := install.Steps(env, opts)

	if !applying {
		n := install.PrintPlan(stdout, install.Plan(ctx, steps))
		if cmd == "doctor" {
			if n > 0 {
				fmt.Fprintf(stdout, "drift: %d step(s) not as installed; `cgctl install` puts them back\n", n)
				return exitFailed
			}
			fmt.Fprintln(stdout, "no drift")
			return exitOK
		}
		fmt.Fprintf(stdout, "%d step(s) to apply\n", n)
		return exitOK
	}

	start := time.Now()
	if err := install.Apply(ctx, env, steps, ui); err != nil {
		return fail(err)
	}
	url := "https://<server-ip>:8443"
	var fwRules []string
	if !f.container {
		url = install.SetupURL(ctx, env)
		fwRules = install.FirewallOffRules(ctx, env)
	}
	ui.Finished(url, logPath, time.Since(start), fwRules)
	return exitOK
}

// installedVersions are the floors a downloaded release must not go below:
// the installed panel-api's version and the newest release self-updated to.
func installedVersions(ctx context.Context) []string {
	var v []string
	if have := installedPanelAPI(ctx); have != "" {
		v = append(v, have)
	}
	if b, err := os.ReadFile(install.HighestReleaseFile); err == nil {
		v = append(v, strings.TrimSpace(string(b)))
	}
	return v
}

// installedPanelAPI is the installed panel-api's version, "" when there is
// none or it does not answer.
func installedPanelAPI(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, install.BinDir+"/panel-api", "--version").Output()
	if err != nil {
		return ""
	}
	if f := strings.Fields(strings.SplitN(string(out), "\n", 2)[0]); len(f) >= 2 {
		return f[1]
	}
	return ""
}

func isTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// tail returns the last n lines of the file at path.
func tail(path string, n int) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
