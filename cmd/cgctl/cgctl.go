// cgctl is CloudGround's command line: the panel API's operations, for
// scripts. It authenticates with an API token, and --wait follows a command
// that starts a task to its end, with the exit code telling how it ended.
// The behaviour is specified in SPEC.md next to this file.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/cloudground-it/cloudground/internal/config"
	"github.com/cloudground-it/cloudground/internal/version"
)

// Exit codes (SPEC.md §4).
const (
	exitOK      = 0
	exitFailed  = 1
	exitUsage   = 2
	exitTimeout = 3
)

func main() {
	args := os.Args[1:]
	for _, a := range args {
		if a == "-v" || a == "--version" || a == "version" {
			fmt.Println("cgctl " + version.Version)
			return
		}
	}
	if len(args) > 0 && (args[0] == "install" || args[0] == "doctor") {
		os.Exit(runInstall(args[0], args[1:], os.Stdout, os.Stderr))
	}
	if len(args) > 0 && args[0] == "update" {
		os.Exit(runUpdate(args[1:], os.Stdout, os.Stderr))
	}
	if len(args) > 0 && args[0] == "uninstall" {
		os.Exit(runUninstall(args[1:], os.Stdin, os.Stdout, os.Stderr))
	}
	if len(args) > 0 && args[0] == "release" {
		os.Exit(releaseCommand(args[1:], os.Stdout, os.Stderr))
	}
	inv, err := parseArgs(args)
	if err != nil {
		if err != errHelp {
			fmt.Fprintln(os.Stderr, "cgctl:", err)
		}
		usage(os.Stderr)
		os.Exit(exitUsage)
	}
	cfg, err := config.Load(config.CLI)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cgctl:", err)
		os.Exit(exitUsage)
	}
	os.Exit(execute(cfg, inv, os.Stdout, os.Stderr))
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `cgctl — CloudGround command line (%s)

Usage: cgctl [--wait] [--timeout <duration>] <command> [arguments]

%s
--wait follows a command that starts a task until it ends: exit 0 when it
succeeded, 1 when it failed, 3 when --timeout (default 1h) ran out first.
Without a task to follow, --wait changes nothing.

cgctl release verify <dir> [file...] checks a downloaded release against the
signing keys built into cgctl; it needs no panel and no token.

Authentication: CLOUDGROUND_TOKEN or CLOUDGROUND_TOKEN_FILE (an API token);
without one, CLOUDGROUND_EMAIL and CLOUDGROUND_PASSWORD (and CLOUDGROUND_TOTP).
Create a token, signed in with the password: cgctl tokens create <name>.
Panel address: CLOUDGROUND_URL (default unix:///run/cloudground/api.sock).
Settings also come from /etc/cloudground/cgctl.env.

On the server, as root: cgctl install (see cgctl install --help) and
cgctl doctor set up CloudGround and report drift from what they set up;
cgctl update installs a newer signed release (cgctl update --help), and
cgctl uninstall removes CloudGround (cgctl uninstall --help).

Site types: wordpress woocommerce static php laravel proxy
Example flags-json for wordpress:
  '{"admin_email":"you@example.com","admin_user":"you","admin_password":"...","title":"My Site"}'
sites create prints the new site's summary, its database password included
(shown only then); --json prints the API's answer instead. A static or php
site gets a database with --with-database; the panel names it site_<id>
unless --db-name / --db-user choose, and generates the password unless
--db-password-stdin (one line read from stdin) or --db-password-file gives
one. --db-password <password> still works but shows the password in ps and
the shell history, and warns so.
`, version.Version, commandList())
}
