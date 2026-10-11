package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent"
	"github.com/cloudground-it/cloudground/internal/agent/ops/db"
	"github.com/cloudground-it/cloudground/internal/config"
	"github.com/cloudground-it/cloudground/internal/state"
)

// adoptName is the subcommand that rebuilds a lost record of the sites'
// databases (internal/agent/ops/db SPEC §2, "A lost record").
const adoptName = "adopt-databases"

// adoptMain is `panel-agent adopt-databases [--state <state.db>]
// [--dry-run]`: root only, with the agent stopped (a running agent holds
// the record in memory and would write its own over the adopted one). It
// reads the panel's state read-only, lists the server's databases and
// accounts, adopts what passes the agent's checks into the record and
// prints every name it adopted, kept or refused. It is a command of the
// box: no RPC reaches it.
func adoptMain(args []string, out io.Writer) int {
	fs := flag.NewFlagSet(adoptName, flag.ContinueOnError)
	fs.SetOutput(out)
	statePath := fs.String("state", defaultStatePath(), "the panel's state database (read only)")
	dryRun := fs.Bool("dry-run", false, "print what would be adopted; write nothing")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(out, "adopt-databases must run as root")
		return 1
	}
	if agentRunning() {
		fmt.Fprintln(out, "the agent is running: stop it first (systemctl stop cloudground-agent), run this again, then start it")
		return 1
	}
	claims, err := state.ReadDatabaseClaims(*statePath)
	if err != nil {
		fmt.Fprintln(out, err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rep, err := agent.New(nil).AdoptDatabases(ctx, claims, *dryRun)
	printAdoption(out, rep)
	if err != nil {
		fmt.Fprintln(out, "the record was not written:", err)
		return 1
	}
	if *dryRun {
		fmt.Fprintln(out, "dry run: nothing was written")
	}
	return 0
}

// printAdoption prints one line per name.
func printAdoption(out io.Writer, rep db.AdoptReport) {
	for _, l := range rep.Adopted {
		fmt.Fprintln(out, "adopted", l)
	}
	for _, l := range rep.Kept {
		fmt.Fprintln(out, "kept", l)
	}
	for _, l := range rep.Refused {
		fmt.Fprintln(out, "refused", l)
	}
	fmt.Fprintf(out, "%d adopted, %d already recorded, %d refused\n", len(rep.Adopted), len(rep.Kept), len(rep.Refused))
}

// defaultStatePath is CLOUDGROUND_STATE from the API's configuration, else
// its default.
func defaultStatePath() string {
	if c, err := config.Load(config.API); err == nil && c.StatePath != "" {
		return c.StatePath
	}
	return "/var/lib/cloudground/state.db"
}

// agentRunning reports whether the agent's socket answers.
func agentRunning() bool {
	sock := "/run/cloudground/agent.sock"
	if c, err := config.Load(config.Agent); err == nil && c.AgentSocket != "" {
		sock = c.AgentSocket
	}
	conn, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
