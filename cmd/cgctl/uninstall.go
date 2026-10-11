package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/cloudground-it/cloudground/internal/install"
)

// `cgctl uninstall` (internal/install/SPEC.md §9).

const uninstallUsage = `Usage:
  cgctl uninstall [--plan] [--yes] [--remove-sites [--confirm <hostname>]] [--purge-packages]

Removes CloudGround from this server: its services, units, binaries, the
configuration files it owns, the panel's user and its state.

By default the sites' files (/srv/sites), their databases and every backup
are kept: the sites stop being served and their users are locked, nothing
of theirs is deleted. A copy of the panel's state and the backup
repository's credentials are left in /root/cloudground-uninstall.

  --plan            print every action, change nothing
  --yes             do not ask for confirmation
  --remove-sites    also delete the sites: their users, /srv/sites, their
                    databases and database accounts, their certificates.
                    Asks for this server's hostname to be typed (or --confirm)
  --purge-packages  also remove nginx, MariaDB, PHP and certbot, only those
                    the installer installed (MariaDB only with --remove-sites
                    and no other database left)

Remote backup repositories are never touched.
`

type uninstallFlags struct {
	plan, yes, removeSites, purge bool
	confirm                       string
}

func parseUninstallFlags(args []string) (uninstallFlags, error) {
	var f uninstallFlags
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--plan":
			f.plan = true
		case "--yes", "-y":
			f.yes = true
		case "--remove-sites":
			f.removeSites = true
		case "--purge-packages":
			f.purge = true
		case "--confirm":
			if i+1 >= len(args) || args[i+1] == "" {
				return f, errors.New("--confirm needs this server's hostname")
			}
			i++
			f.confirm = args[i]
		case "-h", "--help", "help":
			return f, errHelp
		default:
			return f, fmt.Errorf("uninstall: unknown argument %q", a)
		}
	}
	if f.confirm != "" && !f.removeSites {
		return f, errors.New("--confirm goes with --remove-sites")
	}
	return f, nil
}

func runUninstall(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	f, err := parseUninstallFlags(args)
	if err != nil {
		if err != errHelp {
			fmt.Fprintln(stderr, "cgctl:", err)
		}
		fmt.Fprint(stderr, uninstallUsage)
		return exitUsage
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "cgctl: uninstall runs as root, on the server: sudo cgctl uninstall")
		return exitUsage
	}
	host, _ := os.Hostname()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return uninstall(ctx, install.System(nil), f, host, defaultStateDB, stdin, stdout, stderr)
}

// uninstall is the command over an Env, for tests.
func uninstall(ctx context.Context, env *install.Env, f uninstallFlags, host, stateDB string, stdin io.Reader, stdout, stderr io.Writer) int {
	o := install.UninstallOptions{RemoveSites: f.removeSites, PurgePackages: f.purge, KeptDir: install.DefaultKeptDir}
	inv := env.TakeInventory(ctx)
	actions := install.UninstallActions(env, inv, o)
	if f.plan {
		install.PrintUninstallPlan(stdout, actions)
		printKept(stdout, inv, o, env)
		return exitOK
	}
	if len(actions) == 0 {
		fmt.Fprintln(stdout, "CloudGround is not installed here: nothing to remove")
		return exitOK
	}
	if err := runningBlockers(env.Path(stateDB)); err != nil {
		fmt.Fprintln(stderr, "cgctl: not uninstalling:", err)
		return exitFailed
	}

	fmt.Fprintf(stdout, "This removes CloudGround from %s (%d actions; `cgctl uninstall --plan` lists them).\n", host, len(actions))
	printKept(stdout, inv, o, env)
	in := bufio.NewReader(stdin)
	if f.removeSites {
		fmt.Fprintf(stdout, "\n--remove-sites DELETES %d site(s), their files under /srv/sites and %d database(s). This cannot be undone.\n",
			len(inv.Sites), len(inv.Databases))
		typed := f.confirm
		if typed == "" {
			fmt.Fprintf(stdout, "Type this server's hostname (%s) to confirm: ", host)
			line, _ := in.ReadString('\n')
			typed = strings.TrimSpace(line)
		}
		if host == "" || typed != host {
			fmt.Fprintln(stderr, "cgctl: the hostname does not match: nothing was changed")
			return exitFailed
		}
	} else if !f.yes {
		fmt.Fprint(stdout, "\nRemove CloudGround? [y/N] ")
		line, _ := in.ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			fmt.Fprintln(stderr, "cgctl: nothing was changed")
			return exitFailed
		}
	}

	if err := os.MkdirAll(env.Path(o.KeptDir), 0o700); err == nil {
		if lf, err := os.OpenFile(env.Path(filepath.Join(o.KeptDir, "uninstall.log")), os.O_WRONLY|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW, 0o600); err == nil {
			defer lf.Close()
			env.Log = lf
		}
	}
	fmt.Fprintln(stdout)
	err := install.RunUninstall(ctx, env, actions, stdout)
	fmt.Fprintln(stdout)
	if err != nil {
		fmt.Fprintf(stderr, "cgctl: the uninstall did not finish: %v\ncgctl: every action can run again: fix the cause and run `cgctl uninstall` again\n", err)
		return exitFailed
	}
	fmt.Fprintln(stdout, "CloudGround was removed.")
	printKeptAfter(stdout, inv, o, env)
	return exitOK
}

// printKept says what an uninstall with these options keeps.
func printKept(w io.Writer, inv install.Inventory, o install.UninstallOptions, env *install.Env) {
	fmt.Fprintln(w, "\nKept:")
	if !o.RemoveSites {
		for _, s := range inv.Sites {
			switch {
			case s.Domain != "" && s.User != "":
				fmt.Fprintf(w, "  site %-30s /srv/sites/%s (its user %s is locked)\n", s.Domain, s.Domain, s.User)
			case s.Domain != "":
				fmt.Fprintf(w, "  site %-30s /srv/sites/%s\n", s.Domain, s.Domain)
			}
		}
		if len(inv.Databases) > 0 {
			fmt.Fprintf(w, "  databases: %s (MariaDB keeps running)\n", strings.Join(inv.Databases, " "))
		}
	}
	fmt.Fprintln(w, "  every backup, in its repository (never touched)")
	if env.Exists("/var/lib/cloudground/state.db") {
		fmt.Fprintf(w, "  the panel's state and the backup credentials, copied to %s\n", o.KeptDir)
	}
}

func printKeptAfter(w io.Writer, inv install.Inventory, o install.UninstallOptions, env *install.Env) {
	repos, creds := env.KeptSummary(o.KeptDir)
	if len(repos) > 0 {
		fmt.Fprintln(w, "\nThe backup repositories were kept as they are:")
		for _, r := range repos {
			fmt.Fprintf(w, "  %s\n", r)
		}
	}
	if creds {
		fmt.Fprintf(w, "Their credentials: %s (root only). Without them a backup cannot be read.\n", filepath.Join(o.KeptDir, "backup.env"))
		fmt.Fprintln(w, "WARNING: that file holds the repository's password in plain text. Store it somewhere safe, off this server, then delete it.")
	}
	if !o.RemoveSites && len(inv.Lineages) > 0 {
		fmt.Fprintf(w, "\nThe sites' certificates were kept (/etc/letsencrypt/live/<domain>: %s). Their renewal used CloudGround's\n", strings.Join(inv.Lineages, " "))
		fmt.Fprintln(w, "ACME webroot, which is gone: they will not renew. Renew one with your own web server")
		fmt.Fprintln(w, "(certbot certonly --webroot -w <root> --cert-name <domain> -d <domain>), or remove it: certbot delete --cert-name <domain>")
	}
	if !o.RemoveSites && len(inv.Sites) > 0 {
		fmt.Fprint(w, "\nThe sites' files are in /srv/sites")
		if len(inv.Databases) > 0 {
			fmt.Fprint(w, " and their databases in MariaDB")
		}
		fmt.Fprintln(w, "; nothing serves them now.")
	}
	if readme := filepath.Join(o.KeptDir, install.KeptReadme); env.Exists(readme) {
		fmt.Fprintf(w, "What was kept, and how to use it: %s\n", readme)
	}
}
