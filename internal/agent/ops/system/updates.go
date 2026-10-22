package system

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

// Ubuntu updates (SPEC §7): the pending upgrades, the reboot notice, and
// the upgrade. Every apt run is a transient root service: the agent's
// sandbox has apt's and dpkg's state read-only.

// The files the update check reads, unless Paths names others (tests).
const (
	defaultRebootRequired = "/run/reboot-required"
	defaultRebootPackages = "/run/reboot-required.pkgs"
	defaultAptUpdateStamp = "/var/lib/apt/periodic/update-success-stamp"
)

// Bounds of the apt runs.
const (
	aptRefreshMax = 10 * time.Minute
	aptListMax    = 5 * time.Minute
	aptUpgradeMax = 60 * time.Minute
	maxRebootPkgs = 100
)

// debName is a Debian package name; anything else in apt's output is not
// taken as one.
var debName = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]*$`)

// instLine is one package of `apt-get -s upgrade`: Inst <name>
// [<installed>] (<candidate> <origins> [<arch>]).
var instLine = regexp.MustCompile(`^Inst (\S+) (?:\[([^\]]*)\] )?\((\S+) (.*?)(?: \[[^\]]*\])?\)`)

// aptOptions run apt with no question asked, keeping the server's version
// of a configuration file the package also changed.
var aptOptions = []string{"-o", "DPkg::Lock::Timeout=120", "-o", "Dpkg::Options::=--force-confdef", "-o", "Dpkg::Options::=--force-confold", "-y", "-q"}

// apt runs apt-get with args as a transient root service and returns its
// output.
func (o *Ops) apt(ctx context.Context, what string, max time.Duration, args ...string) ([]byte, error) {
	res, err := o.Exec.Run(ctx, host.Cmd{Name: "systemd-run", Args: host.AptUnitArgs("CloudGround: "+what, max, args...),
		Env: host.AptEnv[:2], Timeout: max + time.Minute})
	if err != nil {
		o.log().Warn("apt-get failed", "what", what, "err", err, "stdout", tail(res.Stdout, 2000))
		return res.Stdout, fmt.Errorf("apt-get %s: %w", what, err)
	}
	return res.Stdout, nil
}

// tail is the end of b, at most n bytes, for the journal.
func tail(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}

// SystemUpdates lists the pending upgrades, after apt-get update when
// asked (SPEC §7).
func (o *Ops) SystemUpdates(ctx context.Context, p rpc.SystemUpdatesParams) (rpc.SystemUpdatesResult, error) {
	if p.Refresh {
		if _, err := o.apt(ctx, "update the package lists", aptRefreshMax, "-o", "DPkg::Lock::Timeout=60", "update", "-qq"); err != nil {
			return rpc.SystemUpdatesResult{}, err
		}
	}
	return o.pendingUpdates(ctx)
}

// pendingUpdates is the simulation of the upgrade, the reboot notice and
// the lists' age.
func (o *Ops) pendingUpdates(ctx context.Context) (rpc.SystemUpdatesResult, error) {
	out, err := o.apt(ctx, "list the pending updates", aptListMax, "-s", "-q", "-o", "Debug::NoLocking=1", "upgrade", "--with-new-pkgs")
	if err != nil {
		return rpc.SystemUpdatesResult{}, err
	}
	res := rpc.SystemUpdatesResult{Packages: parseUpgrade(out)}
	for _, p := range res.Packages {
		if p.Security {
			res.Security++
		}
	}
	res.RebootRequired, res.RebootPackages = o.rebootNotice()
	if fi, err := os.Stat(orDefault(o.Paths.AptUpdateStamp, defaultAptUpdateStamp)); err == nil {
		res.ListsUpdatedAt = fi.ModTime().UTC()
	}
	return res, nil
}

// parseUpgrade reads apt-get -s upgrade's Inst lines: security first, then
// by name.
func parseUpgrade(out []byte) []rpc.PendingUpdate {
	list := []rpc.PendingUpdate{}
	seen := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		m := instLine.FindStringSubmatch(sc.Text())
		if m == nil || !debName.MatchString(m[1]) || seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		origin := strings.TrimSpace(m[4])
		list = append(list, rpc.PendingUpdate{Name: m[1], From: m[2], To: m[3], Origin: origin,
			Security: isSecurityOrigin(origin), New: m[2] == ""})
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].Security != list[j].Security {
			return list[i].Security
		}
		return list[i].Name < list[j].Name
	})
	return list
}

// isSecurityOrigin reports whether one of apt's origins ("Ubuntu:24.04/
// noble-updates, Ubuntu:24.04/noble-security") is a security archive.
func isSecurityOrigin(origins string) bool {
	for _, o := range strings.Split(origins, ",") {
		o = strings.TrimSpace(o)
		if i := strings.LastIndexByte(o, '/'); i >= 0 {
			o = o[i+1:]
		}
		if strings.HasSuffix(o, "-security") {
			return true
		}
	}
	return false
}

// rebootNotice is whether the server waits for a reboot, and the packages
// that asked for it.
func (o *Ops) rebootNotice() (bool, []string) {
	if _, err := os.Stat(orDefault(o.Paths.RebootRequired, defaultRebootRequired)); err != nil {
		return false, nil
	}
	b, err := os.ReadFile(orDefault(o.Paths.RebootPackages, defaultRebootPackages))
	if err != nil {
		return true, nil
	}
	var pkgs []string
	seen := map[string]bool{}
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if debName.MatchString(l) && !seen[l] && len(pkgs) < maxRebootPkgs {
			seen[l] = true
			pkgs = append(pkgs, l)
		}
	}
	return true, pkgs
}

func orDefault(p, def string) string {
	if p == "" {
		return def
	}
	return p
}

// ApplyUpdates upgrades every pending package, or the security updates
// alone (SPEC §7). The packages are the agent's own list, never the
// caller's.
func (o *Ops) ApplyUpdates(ctx context.Context, p rpc.ApplyUpdatesParams) (rpc.ApplyUpdatesResult, error) {
	before, err := o.pendingUpdates(ctx)
	if err != nil {
		return rpc.ApplyUpdatesResult{}, err
	}
	var args []string
	n := 0
	if p.SecurityOnly {
		var names []string
		for _, u := range before.Packages {
			if u.Security && !u.New {
				names = append(names, u.Name)
			}
		}
		if len(names) > 0 {
			args = append(append(append([]string{}, aptOptions...), "install", "--only-upgrade"), names...)
		}
		n = len(names)
	} else if len(before.Packages) > 0 {
		args = append(append([]string{}, aptOptions...), "upgrade", "--with-new-pkgs")
		n = len(before.Packages)
	}
	if args == nil {
		return rpc.ApplyUpdatesResult{Pending: before}, nil
	}
	what := "upgrade the system packages"
	if p.SecurityOnly {
		what = "install the security updates"
	}
	o.log().Info("applying system updates", "packages", n, "security_only", p.SecurityOnly)
	if _, err := o.apt(ctx, what, aptUpgradeMax, args...); err != nil {
		return rpc.ApplyUpdatesResult{}, err
	}
	after, err := o.pendingUpdates(ctx)
	if err != nil {
		return rpc.ApplyUpdatesResult{}, errors.Join(errors.New("the upgrade ran, but the pending updates could not be read again"), err)
	}
	return rpc.ApplyUpdatesResult{Upgraded: n, Pending: after}, nil
}
