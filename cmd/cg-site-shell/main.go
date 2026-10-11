// Command cg-site-shell is the login shell of a site's shell login
// (cg-shell-<id>): the client of the agent's shell gate (internal/siteshell,
// ops/sftp SPEC §5.4). It is its own small binary so that the process, which
// runs as the site's uid outside the sandbox, is made not dumpable before
// any code of this project runs: package early does it in its init, which
// only the runtime and the standard library's initialisation precede. In
// panel-agent, the whole agent's package initialisation ran first.
// panel-agent still answers to this name (argv[0]) for a server whose
// cg-site-shell is still the link to it, until the installer puts this
// binary in its place.
package main

import (
	_ "github.com/cloudground-it/cloudground/internal/siteshell/early"

	"os"

	"github.com/cloudground-it/cloudground/internal/siteshell"
)

func main() {
	os.Exit(siteshell.Main(os.Args))
}
