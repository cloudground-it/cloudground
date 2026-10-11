// Command badapi stands in for panel-api in the upgrade test's broken
// release (scripts/upgrade-bundle.sh, e2e section "R-6.5 upgrade"): it
// answers --version like panel-api and otherwise exits 1 at once, so the
// update guard finds the release unhealthy and rolls it back. It is signed
// with the test's throwaway key only, and never part of a release.
package main

import (
	"fmt"
	"os"

	"github.com/cloudground-it/cloudground/internal/version"
)

func main() {
	if version.HandleFlag(os.Args[1:], "panel-api", "A broken panel-api, for the upgrade test's rollback.") {
		return
	}
	fmt.Fprintln(os.Stderr, "badapi: this release is broken on purpose (the upgrade test's rollback)")
	os.Exit(1)
}
