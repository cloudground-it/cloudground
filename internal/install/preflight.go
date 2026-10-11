package install

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// OS is the distribution preflight found.
type OS struct {
	ID, VersionID, Pretty string
	// PHP is the PHP version this distribution gets, and PPA whether it
	// comes from ppa:ondrej/php.
	PHP string
	PPA bool
}

// Supported reports whether the installer knows this distribution.
func (o OS) Supported() bool { return o.PHP != "" }

// ErrNotRoot is preflight's one immediate stop.
var ErrNotRoot = errors.New("run as root: sudo bash installer/install.sh")

// DetectOS reads /etc/os-release.
func (e *Env) DetectOS() (OS, error) {
	raw, err := e.readFile("/etc/os-release")
	if err != nil {
		return OS{}, err
	}
	kv := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok || strings.HasPrefix(k, "#") {
			continue
		}
		if uq, err := strconv.Unquote(v); err == nil {
			v = uq
		} else {
			v = strings.Trim(v, `'"`)
		}
		kv[k] = v
	}
	o := OS{ID: kv["ID"], VersionID: kv["VERSION_ID"], Pretty: kv["PRETTY_NAME"]}
	switch o.ID + "-" + o.VersionID {
	case "ubuntu-24.04":
		o.PHP, o.PPA = "8.4", true // 24.04 ships 8.3
	case "ubuntu-26.04":
		o.PHP = "8.5"
	}
	return o, nil
}

// memTotalMB reads MemTotal from /proc/meminfo.
func (e *Env) memTotalMB() (int64, error) {
	raw, err := e.readFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	for _, l := range strings.Split(string(raw), "\n") {
		if f := strings.Fields(l); len(f) >= 2 && f[0] == "MemTotal:" {
			kb, err := strconv.ParseInt(f[1], 10, 64)
			if err != nil {
				return 0, err
			}
			return kb / 1024, nil
		}
	}
	return 0, fmt.Errorf("no MemTotal in /proc/meminfo")
}

// Preflight is what preflight found: the OS, whether this is an upgrade,
// and every problem that stops the install.
type Preflight struct {
	OS       OS
	Upgrade  bool
	Problems []string
	Summary  string
}

// Upgrade reports whether CloudGround is installed here: the agent token
// exists and the agent is enabled or active. Enabled counts: an agent that
// fails at start is not active, and the re-run that repairs it must not be
// refused for the ports its own nginx holds.
func (e *Env) upgrade(ctx context.Context) bool {
	if !e.exists("/etc/cloudground/agent.token") || e.Sd == nil || !e.Sd.Running() {
		return false
	}
	en, _ := e.Sd.Enabled(ctx, "cloudground-agent")
	ac, _ := e.Sd.Active(ctx, "cloudground-agent")
	return en || ac
}

// RunPreflight checks the machine (SPEC §2). In a container only the
// distribution is checked.
func (e *Env) RunPreflight(ctx context.Context, container bool) (Preflight, error) {
	var p Preflight
	if !container && e.EUID != 0 {
		return p, ErrNotRoot
	}
	o, err := e.DetectOS()
	switch {
	case err != nil:
		p.Problems = append(p.Problems, "cannot detect the OS (/etc/os-release missing)")
	case !o.Supported():
		pretty := o.Pretty
		if pretty == "" {
			pretty = "unknown"
		}
		p.Problems = append(p.Problems, fmt.Sprintf("Ubuntu 24.04 or 26.04 LTS required (found %s)", pretty))
	}
	p.OS = o
	p.Upgrade = e.upgrade(ctx)
	if container {
		p.Summary = fmt.Sprintf("Preflight (container): %s, PHP %s.", o.Pretty, o.PHP)
		return p, nil
	}
	if e.Arch != "x86_64" {
		p.Problems = append(p.Problems, fmt.Sprintf("amd64 required (found %s)", e.Arch))
	}
	// A "1 GB" instance reports ~950-990 MB: firmware and the kernel image
	// never reach the free pool. 900 lets it pass and stops a 512 MB one.
	mem, err := e.memTotalMB()
	if err == nil && mem < 900 {
		p.Problems = append(p.Problems, fmt.Sprintf("at least 1 GB of RAM required (found %d MB)", mem))
	}
	if e.FreeMB != nil {
		if free, err := e.FreeMB(e.path("/")); err == nil && free < 10000 {
			p.Problems = append(p.Problems, fmt.Sprintf("at least 10 GB free disk required (found %d MB)", free))
		}
	}
	if !p.Upgrade && e.Listening != nil {
		for _, port := range []int{80, 443} {
			if e.Listening(port) {
				p.Problems = append(p.Problems, fmt.Sprintf("port %d is already in use (%s) — CloudGround must own the web ports", port, e.portOwner(ctx, port)))
			}
		}
	}
	p.Summary = fmt.Sprintf("Preflight passed: %s, PHP %s, %d MB RAM, ports free.", o.Pretty, o.PHP, mem)
	return p, nil
}

// portOwner names the process listening on port, as ss shows it.
func (e *Env) portOwner(ctx context.Context, port int) string {
	out, err := e.Cmd.Run(ctx, "ss", "-ltnpH", fmt.Sprintf("sport = :%d", port))
	if err != nil {
		return "unknown"
	}
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) > 0 {
			return f[len(f)-1]
		}
	}
	return "unknown"
}
