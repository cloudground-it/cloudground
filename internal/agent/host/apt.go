package host

import (
	"fmt"
	"strconv"
	"sync/atomic"
	"time"
)

var aptRuns atomic.Int64

// AptEnv is apt's environment: no question on a terminal, the C locale.
// Its first two entries (PATH, the locale) are systemd-run's own.
var AptEnv = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C", "DEBIAN_FRONTEND=noninteractive"}

// AptUnitArgs is the systemd-run argv (after the binary) that runs apt-get
// with args as a transient root service. apt cannot run in the agent's own
// sandbox: /usr, dpkg's database and apt's cache are read-only there
// (ProtectSystem=strict), so systemd starts it outside it, waits for its
// end and passes apt's output and exit status back. runtimeMax
// (RuntimeMaxSec) bounds the run even when the caller stops waiting first:
// a dpkg run is not interrupted by the caller going away. The unit is named
// cg-apt-… so that needrestart leaves it alone (internal/install SPEC §7).
func AptUnitArgs(description string, runtimeMax time.Duration, args ...string) []string {
	a := []string{"--quiet", "--collect", "--wait", "--pipe", "--service-type=exec",
		fmt.Sprintf("--unit=cg-apt-%d-%d", time.Now().UnixNano(), aptRuns.Add(1)),
		"--description=" + description, "-p", "RuntimeMaxSec=" + strconv.Itoa(int(runtimeMax/time.Second))}
	for _, kv := range AptEnv {
		a = append(a, "--setenv="+kv)
	}
	return append(append(a, "--", "apt-get"), args...)
}
