package host

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// noExpandSince is the first systemd whose systemd-run has
// --expand-environment (254). Before it, systemd-run left ExecStart's
// arguments to the unit's own expansion of $VAR and ${VAR}; with it, and
// =no, the argv a run is given is the argv it gets.
const noExpandSince = 254

var (
	noExpandOnce sync.Once
	noExpand     bool
	// detectNoExpand reads systemd-run's version; tests replace it.
	detectNoExpand = func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "systemd-run", "--version").Output()
		if err != nil {
			return false
		}
		return systemdAtLeast(string(out), noExpandSince)
	}
)

// systemdAtLeast reads "systemd 255 (255.4-1ubuntu8)" and compares.
func systemdAtLeast(out string, v int) bool {
	f := strings.Fields(out)
	if len(f) < 2 || f[0] != "systemd" {
		return false
	}
	n, err := strconv.Atoi(f[1])
	return err == nil && n >= v
}

// expandOff reports whether systemd-run takes --expand-environment=no on
// this machine (read once per process).
func expandOff() bool {
	noExpandOnce.Do(func() { noExpand = detectNoExpand() })
	return noExpand
}
