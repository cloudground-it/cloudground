package blocklist

import (
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

// sshdFailure matches the sshd lines that count as a failure (SPEC §4.1).
// "Failed … for invalid user" does not: sshd logged "Invalid user" for the
// same attempt already.
var sshdFailure = regexp.MustCompile(`^(?:` +
	`Failed (?:password|keyboard-interactive/pam) for (?:[^ ]+) from ([0-9A-Fa-f.:]+) port \d+` +
	`|Invalid user (?:.*) from ([0-9A-Fa-f.:]+) port \d+` +
	`|Connection closed by authenticating user (?:[^ ]+) ([0-9A-Fa-f.:]+) port \d+ \[preauth\]` +
	`)`)

// SSHFailure reads one sshd journal message (SPEC §4.1).
func SSHFailure(msg string) (netip.Addr, bool) {
	if strings.HasPrefix(msg, "Failed ") && strings.Contains(msg, " for invalid user ") {
		return netip.Addr{}, false
	}
	m := sshdFailure.FindStringSubmatch(msg)
	if m == nil {
		return netip.Addr{}, false
	}
	for _, s := range m[1:] {
		if s == "" {
			continue
		}
		a, err := netip.ParseAddr(s)
		if err != nil {
			return netip.Addr{}, false
		}
		return a.Unmap(), true
	}
	return netip.Addr{}, false
}

// nginxTag is the syslog tag of the vhosts' failure lines.
const nginxTag = " cloudground: "

// NginxFailure reads one syslog datagram of the vhosts' failure log
// (SPEC §4.2): the address and how many failures the response reported.
func NginxFailure(datagram []byte) (netip.Addr, int, bool) {
	s := strings.TrimRight(string(datagram), "\r\n\x00")
	i := strings.Index(s, nginxTag)
	if !strings.HasPrefix(s, "<") || i < 0 {
		return netip.Addr{}, 0, false
	}
	f := strings.Fields(s[i+len(nginxTag):])
	if len(f) != 2 {
		return netip.Addr{}, 0, false
	}
	a, err := netip.ParseAddr(f[0])
	if err != nil || a.Zone() != "" {
		return netip.Addr{}, 0, false
	}
	n := 1
	switch {
	case f[1] == "failed":
	case strings.HasPrefix(f[1], "failed="):
		v, err := strconv.Atoi(strings.TrimPrefix(f[1], "failed="))
		if err != nil || v < 1 {
			return netip.Addr{}, 0, false
		}
		n = min(v, maxCount)
	default:
		return netip.Addr{}, 0, false
	}
	return a.Unmap(), n, true
}
