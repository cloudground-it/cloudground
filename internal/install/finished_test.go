package install

import (
	"errors"
	"strings"
	"testing"
)

// The end of a run says when ufw is off, and how to turn it on without
// closing SSH (on the ports sshd really uses), the sites or the panel.
func TestFirewallOffRules(t *testing.T) {
	m := newMachine(t, "24.04")
	status, sshd := "Status: inactive\n", "port 2222\nport 22\nlistenaddress 0.0.0.0:2222\n"
	var sshdErr error
	m.cmd.handle = func(line string) (string, error, bool) {
		switch line {
		case "ufw status":
			return status, nil, true
		case "sshd -T":
			return sshd, sshdErr, true
		}
		return m.answer(line)
	}
	m.cmd.tools["ufw"] = false
	if got := FirewallOffRules(bg, m.env); got != nil {
		t.Errorf("no ufw: %v", got)
	}
	m.cmd.tools["ufw"] = true
	if got, want := strings.Join(FirewallOffRules(bg, m.env), " "), "2222/tcp 22/tcp 80/tcp 443/tcp 8443/tcp 443/udp"; got != want {
		t.Errorf("inactive, sshd on 2222 and 22: %q, want %q", got, want)
	}
	sshd = "port 2222\n"
	if got, want := strings.Join(FirewallOffRules(bg, m.env), " "), "2222/tcp 80/tcp 443/tcp 8443/tcp 443/udp"; got != want {
		t.Errorf("sshd on 2222 only: %q, want %q", got, want)
	}
	sshdErr = errors.New("exit status 255")
	if got, want := strings.Join(FirewallOffRules(bg, m.env), " "), "22/tcp 80/tcp 443/tcp 8443/tcp 443/udp"; got != want {
		t.Errorf("sshd -T failing: %q, want %q", got, want)
	}
	status = "Status: active\n"
	if got := FirewallOffRules(bg, m.env); got != nil {
		t.Errorf("active: %v", got)
	}
	status = "ERROR: You need to be root to run this script\n"
	if got := FirewallOffRules(bg, m.env); got != nil {
		t.Errorf("unreadable state: %v", got)
	}
}

func TestFinishedFirewallLine(t *testing.T) {
	rules := []string{"2222/tcp", "80/tcp", "443/tcp", "8443/tcp", "443/udp"}
	head := "The firewall (ufw) is off. To turn it on with SSH, the sites and the panel open:"
	cmds := "ufw allow 2222/tcp && ufw allow 80/tcp && ufw allow 443/tcp && ufw allow 8443/tcp && ufw allow 443/udp && ufw enable"
	const link, log = "https://203.0.113.7:8443/setup/tok", "/var/log/cloudground-install.log"

	var out strings.Builder
	(&UI{Out: &out}).Finished(link, log, 0, rules)
	if s := out.String(); !strings.Contains(s, "\n     "+head+"\n     "+cmds+"\n") || strings.Contains(s, "\033") {
		t.Errorf("non-TTY: %q", s)
	}

	out.Reset()
	(&UI{Out: &out, TTY: true}).Finished(link, log, 0, rules)
	if s := out.String(); !strings.Contains(s, "     "+ansiDim+head+ansiReset+"\n") || !strings.Contains(s, "     "+ansiDim+cmds+ansiReset+"\n") {
		t.Errorf("TTY: %q", s)
	}

	for _, tty := range []bool{false, true} {
		out.Reset()
		(&UI{Out: &out, TTY: tty}).Finished(link, log, 0, nil)
		if strings.Contains(out.String(), "ufw") {
			t.Errorf("tty=%v, no rules: %q", tty, out.String())
		}
	}
}
