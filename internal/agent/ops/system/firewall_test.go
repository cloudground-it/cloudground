package system

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

func ufwCalls(f *fixture) []string {
	var out []string
	for _, c := range f.exec.Calls() {
		if c.Name == "ufw" {
			out = append(out, c.Line())
		}
	}
	return out
}

// ufwCallsSince is ufwCalls from the mark-th call of the fixture on.
func ufwCallsSince(f *fixture, mark int) []string {
	var out []string
	for _, c := range f.exec.Calls()[mark:] {
		if c.Name == "ufw" {
			out = append(out, c.Line())
		}
	}
	return out
}

func TestSSHDPorts(t *testing.T) {
	for _, c := range []struct {
		text string
		want []int
	}{
		{"port 22\naddressfamily any\nlistenaddress [::]:22\nlistenaddress 0.0.0.0:22\n", []int{22}},
		{"port 2222\nport 22\n", []int{2222, 22}},
		// A ListenAddress with its own port: sshd listens there, not on Port.
		{"port 22\nlistenaddress 0.0.0.0:2200\nlistenaddress [::]:2200\n", []int{2200, 22}},
		// sshd_config spelling, comments and bare addresses.
		{"#Port 2022\nPort 2023\nListenAddress 10.0.0.1\nListenAddress ::1\nPORT 70000\nport x\n", []int{2023}},
		{"", nil},
	} {
		if got := sshdPorts(c.text); !reflect.DeepEqual(got, c.want) {
			t.Errorf("sshdPorts(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

// sshd -T is the source; its failure falls back to the config files.
func TestSSHPortsFallBackToConfigFiles(t *testing.T) {
	f := newFixture(t)
	f.respond(map[string]host.FakeReply{"sshd": {Stdout: "port 2222\nlistenaddress 0.0.0.0:2222\n"}})
	if got := SSHPorts(context.Background(), f.exec, f.Paths.SSHDConfig); !reflect.DeepEqual(got, []int{2222}) {
		t.Fatalf("from sshd -T: %v", got)
	}
	write(t, f.Paths.SSHDConfig, "Include /etc/ssh/sshd_config.d/*.conf\nPort 22\n")
	write(t, filepath.Join(filepath.Dir(f.Paths.SSHDConfig), "sshd_config.d", "50-port.conf"), "Port 4422\n")
	f.respond(map[string]host.FakeReply{"sshd": {Code: 255, Stderr: "sshd: no hostkeys available -- exiting."}})
	if got := SSHPorts(context.Background(), f.exec, f.Paths.SSHDConfig); !reflect.DeepEqual(got, []int{22, 4422}) {
		t.Fatalf("from the files: %v", got)
	}
	if p := SSHPort(context.Background(), f.exec, filepath.Join(f.dir, "none")); p != 22 {
		t.Fatalf("no configuration at all: port %d", p)
	}
	f.respond(map[string]host.FakeReply{"sshd": {Stdout: "port 2222\nlistenaddress 0.0.0.0:2200\n"}})
	if p := SSHPort(context.Background(), f.exec, f.Paths.SSHDConfig); p != 2200 {
		t.Fatalf("SFTP port %d, want the one sshd listens on", p)
	}
}

// No essential port can be closed: every SSH port, 80, 443 and the panel's
// 8443. The old list missed 8443 (`deny 8443` was accepted and run) and the
// port of a ListenAddress.
func TestFirewallKeepsEssentialPortsOpen(t *testing.T) {
	f := newFixture(t)
	f.respond(map[string]host.FakeReply{"sshd": {Stdout: "port 2222\nport 22\nlistenaddress 0.0.0.0:2200\naddressfamily any\n"}})
	ctx := context.Background()
	for _, p := range []rpc.FirewallRuleParams{
		{Action: "deny", Port: 2222},
		{Action: "deny", Port: 2200},
		{Action: "deny", Port: 22, Proto: "udp"},
		{Action: "deny", Port: 443},
		{Action: "deny", Port: 8443},
		// A source of every address is a port-wide deny.
		{Action: "deny", Port: 22, From: "0.0.0.0/0"},
		{Action: "deny", Port: 8443, From: "::/0"},
	} {
		if _, err := f.FirewallRule(ctx, p); codeOf(err) != agentrpc.CodeInvalid {
			t.Errorf("%s %d from %q: err = %v", p.Action, p.Port, p.From, err)
		}
	}
	if c := ufwCalls(f); len(c) != 0 {
		t.Fatalf("ufw ran for a refused rule: %v", c)
	}
	// A delete on an essential port never removes an allow rule: only the
	// deny of that shape is tried, and with none there it is refused.
	for _, p := range []rpc.FirewallRuleParams{
		{Action: "delete", Port: 22},
		{Action: "delete", Port: 80},
		{Action: "delete", Port: 8443},
		{Action: "delete", Port: 22, From: "203.0.113.9"},
	} {
		if _, err := f.FirewallRule(ctx, p); codeOf(err) != agentrpc.CodeInvalid {
			t.Errorf("%s %d from %q: err = %v", p.Action, p.Port, p.From, err)
		}
	}
	for _, c := range ufwCalls(f) {
		if !strings.HasPrefix(c, "ufw delete deny ") {
			t.Fatalf("an essential port's allow rule was touched: %q", c)
		}
	}
	f = newFixture(t)
	f.respond(map[string]host.FakeReply{"sshd": {Stdout: "port 22\n"}})
	for _, p := range []rpc.FirewallRuleParams{
		{Action: "allow", Port: 22, Proto: "tcp"},
		{Action: "allow", Port: 8443},
		{Action: "deny", Port: 443, From: "203.0.113.9"},
		{Action: "deny", Port: 8080},
		{Action: "delete", Port: 8080},
	} {
		if _, err := f.FirewallRule(ctx, p); err != nil {
			t.Errorf("%s %d from %q was refused: %v", p.Action, p.Port, p.From, err)
		}
	}
	want := []string{
		"ufw allow 22/tcp",
		"ufw allow 8443/tcp",
		"ufw deny from 203.0.113.9 to any port 443 proto tcp",
		"ufw deny 8080/tcp",
		"ufw delete deny 8080/tcp",
		"ufw delete allow 8080/tcp",
	}
	if got := ufwCalls(f); !reflect.DeepEqual(got, want) {
		t.Fatalf("ufw calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A deny for one source on an essential port narrows access, so it can be
// added and deleted again; deleting it removes the deny only. The old guard
// refused every delete on 8443, so the deny it had just accepted stayed.
func TestFirewallDeletesADenyOnAnEssentialPort(t *testing.T) {
	f := newFixture(t)
	f.exec.Respond = func(c host.FakeCall) host.FakeReply {
		if c.Name == "ufw" && strings.HasPrefix(c.Line(), "ufw delete deny ") {
			return host.FakeReply{Stdout: "Rule deleted\n"}
		}
		return host.FakeReply{}
	}
	ctx := context.Background()
	deny := rpc.FirewallRuleParams{Action: "deny", Port: 8443, Proto: "tcp", From: "203.0.113.4"}
	if _, err := f.FirewallRule(ctx, deny); err != nil {
		t.Fatalf("deny for one source: %v", err)
	}
	deny.Action = "delete"
	res, err := f.FirewallRule(ctx, deny)
	if err != nil {
		t.Fatalf("delete of that deny: %v", err)
	}
	if res["rule"] != "delete deny 8443/tcp" {
		t.Fatalf("result %v", res)
	}
	// The port-wide deny rule (were one there) can go too: it only widens.
	if _, err := f.FirewallRule(ctx, rpc.FirewallRuleParams{Action: "delete", Port: 8443}); err != nil {
		t.Fatalf("delete of a port-wide deny: %v", err)
	}
	want := []string{
		"ufw deny from 203.0.113.4 to any port 8443 proto tcp",
		"ufw delete deny from 203.0.113.4 to any port 8443 proto tcp",
		"ufw delete deny 8443/tcp",
	}
	if got := ufwCalls(f); !reflect.DeepEqual(got, want) {
		t.Fatalf("ufw calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// Deleting a source-limited rule deletes that rule. The old code dropped the
// source and deleted the port's global allow and deny rules instead.
func TestFirewallDeleteKeepsTheSource(t *testing.T) {
	f := newFixture(t)
	res, err := f.FirewallRule(context.Background(), rpc.FirewallRuleParams{Action: "delete", Port: 3306, Proto: "tcp", From: "198.51.100.0/24"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"ufw delete deny from 198.51.100.0/24 to any port 3306 proto tcp",
		"ufw delete allow from 198.51.100.0/24 to any port 3306 proto tcp",
	}
	if got := ufwCalls(f); !reflect.DeepEqual(got, want) {
		t.Fatalf("ufw calls = %v", got)
	}
	if res["rule"] != "delete 3306/tcp" {
		t.Fatalf("result = %v", res)
	}
}

func TestFirewallRuleValidation(t *testing.T) {
	f := newFixture(t)
	for _, p := range []rpc.FirewallRuleParams{
		{Action: "allow", Port: 0},
		{Action: "allow", Port: 65536},
		{Action: "allow", Port: 8080, From: "evil; rm -rf /"},
		{Action: "reject", Port: 8080},
		{Action: "", Port: 8080},
	} {
		if _, err := f.FirewallRule(context.Background(), p); codeOf(err) != agentrpc.CodeInvalid {
			t.Errorf("%+v: err = %v", p, err)
		}
	}
	if len(f.exec.Calls()) != 0 {
		t.Fatalf("calls = %v", f.exec.Calls())
	}
	// Any protocol but udp is tcp, as before.
	if res, err := f.FirewallRule(context.Background(), rpc.FirewallRuleParams{Action: "allow", Port: 8080, Proto: "sctp"}); err != nil || res["rule"] != "allow 8080/tcp" {
		t.Fatalf("res %v err %v", res, err)
	}
	// A failing ufw is reported (the old delete ignored `delete deny`).
	f.respond(map[string]host.FakeReply{"ufw": {Code: 1, Stderr: "ERROR: problem running"}})
	if _, err := f.FirewallRule(context.Background(), rpc.FirewallRuleParams{Action: "delete", Port: 8080}); err == nil {
		t.Fatal("a failed ufw delete was swallowed")
	}
}

func TestFirewallStatus(t *testing.T) {
	f := newFixture(t)
	f.respond(map[string]host.FakeReply{"ufw": {Stdout: "Status: active\n\nTo                         Action      From\n--                         ------      ----\n22/tcp                     ALLOW       Anywhere\n443/tcp                    ALLOW       Anywhere\n"}})
	res, err := f.FirewallStatus(context.Background())
	if err != nil || !res.Enabled || !reflect.DeepEqual(res.Rules, []string{"22/tcp                     ALLOW       Anywhere", "443/tcp                    ALLOW       Anywhere"}) {
		t.Fatalf("status = %+v %v", res, err)
	}
	f.respond(map[string]host.FakeReply{"ufw": {Err: exec.ErrNotFound}})
	res, err = f.FirewallStatus(context.Background())
	if err != nil || res.Enabled || !reflect.DeepEqual(res.Rules, []string{"ufw is not available"}) {
		t.Fatalf("no ufw = %+v %v", res, err)
	}
	f.respond(map[string]host.FakeReply{"ufw": {Err: errors.New("ufw: context deadline exceeded")}})
	if _, err := f.FirewallStatus(context.Background()); err == nil {
		t.Fatal("a failed ufw read as inactive")
	}
}

// Turning ufw on from the panel (#14): the status of an inactive ufw carries
// the plan, SSH on the port sshd really listens on; enabling refuses a plan
// that is not the one shown, and otherwise allows every rule of it before
// `ufw --force enable`, never after.
func TestFirewallEnableAllowsThePlanFirst(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	active := false
	f.exec.Respond = func(c host.FakeCall) host.FakeReply {
		switch {
		case c.Name == "sshd":
			return host.FakeReply{Stdout: "port 2222\nlistenaddress 0.0.0.0:2222\n"}
		case c.Name == "ufw" && c.Args[0] == "status" && active:
			return host.FakeReply{Stdout: "Status: active\n\nTo                         Action      From\n--                         ------      ----\n2222/tcp                   ALLOW       Anywhere\n"}
		case c.Name == "ufw" && c.Args[0] == "status":
			return host.FakeReply{Stdout: "Status: inactive\n"}
		case c.Name == "ufw" && c.Args[0] == "show":
			return host.FakeReply{Stdout: "Added user rules (see 'ufw status' for running firewall):\n(None)\n"}
		case c.Name == "ufw" && c.Args[0] == "--force":
			active = true
		}
		return host.FakeReply{}
	}
	st, err := f.FirewallStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var plan []string
	for _, r := range st.Plan {
		plan = append(plan, r.String())
	}
	want := []string{"2222/tcp", "80/tcp", "443/tcp", "443/udp", "8443/tcp"}
	if st.Enabled || !reflect.DeepEqual(plan, want) || st.Plan[0].Service != "ssh" {
		t.Fatalf("status = %+v, plan %v, want %v", st, plan, want)
	}

	// A plan other than the agent's (sshd moved since it was shown).
	_, err = f.FirewallEnable(ctx, rpc.FirewallEnableParams{Expect: []string{"22/tcp", "80/tcp", "443/tcp", "443/udp", "8443/tcp"}})
	if codeOf(err) != agentrpc.CodeConflict {
		t.Fatalf("stale plan: err = %v, want conflict", err)
	}
	for _, c := range ufwCalls(f) {
		if strings.Contains(c, "allow") || strings.Contains(c, "enable") {
			t.Fatalf("a refused enable ran %q", c)
		}
	}

	mark := len(f.exec.Calls())
	res, err := f.FirewallEnable(ctx, rpc.FirewallEnableParams{Expect: want})
	if err != nil || !res.Enabled || res.Plan != nil {
		t.Fatalf("enable = %+v, %v", res, err)
	}
	var changes []string
	for _, c := range ufwCallsSince(f, mark) {
		if !strings.HasPrefix(c, "ufw status") && !strings.HasPrefix(c, "ufw show") {
			changes = append(changes, c)
		}
	}
	if w := []string{"ufw allow 2222/tcp", "ufw allow 80/tcp", "ufw allow 443/tcp", "ufw allow 443/udp", "ufw allow 8443/tcp", "ufw --force enable"}; !reflect.DeepEqual(changes, w) {
		t.Fatalf("ufw ran %v, want %v", changes, w)
	}

	// Already on: nothing to do, the status back.
	mark = len(f.exec.Calls())
	if res, err := f.FirewallEnable(ctx, rpc.FirewallEnableParams{Expect: want}); err != nil || !res.Enabled {
		t.Fatalf("enable again = %+v, %v", res, err)
	}
	if calls := ufwCallsSince(f, mark); len(calls) != 1 || calls[0] != "ufw status" {
		t.Fatalf("enable of an active ufw ran %v", calls)
	}
}

// A rule ufw refuses stops the enable before ufw is on.
func TestFirewallEnableStopsOnAFailedRule(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.exec.Respond = func(c host.FakeCall) host.FakeReply {
		switch {
		case c.Name == "sshd":
			return host.FakeReply{Stdout: "port 22\n"}
		case c.Name == "ufw" && c.Args[0] == "status":
			return host.FakeReply{Stdout: "Status: inactive\n"}
		case c.Name == "ufw" && c.Args[0] == "allow" && c.Args[1] == "443/udp":
			return host.FakeReply{Code: 1, Stderr: "ERROR: boom"}
		}
		return host.FakeReply{}
	}
	if _, err := f.FirewallEnable(ctx, rpc.FirewallEnableParams{Expect: []string{"22/tcp", "80/tcp", "443/tcp", "443/udp", "8443/tcp"}}); err == nil {
		t.Fatal("a failed rule must fail the enable")
	}
	for _, c := range ufwCalls(f) {
		if strings.Contains(c, "enable") || strings.Contains(c, "8443") {
			t.Fatalf("ran %q after a failed rule", c)
		}
	}
}
