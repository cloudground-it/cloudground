package host

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
)

// ErrUnavailable is a firewall tool that is not installed.
var ErrUnavailable = errors.New("firewall tool not available")

// FirewallRule is one ufw rule.
type FirewallRule struct {
	Port  int
	Proto string // tcp | udp
	From  string // "" for any, else an IP or CIDR
}

func (r FirewallRule) validate() error {
	if r.Port < 1 || r.Port > 65535 {
		return fmt.Errorf("port %d: %w", r.Port, ErrInvalidArgument)
	}
	if r.Proto != "tcp" && r.Proto != "udp" {
		return fmt.Errorf("protocol %q: %w", r.Proto, ErrInvalidArgument)
	}
	if r.From != "" && net.ParseIP(r.From) == nil {
		if _, _, err := net.ParseCIDR(r.From); err != nil {
			return fmt.Errorf("source %q: %w", r.From, ErrInvalidArgument)
		}
	}
	return nil
}

func (r FirewallRule) args(action string) []string {
	if r.From != "" {
		return []string{action, "from", r.From, "to", "any", "port", strconv.Itoa(r.Port), "proto", r.Proto}
	}
	return []string{action, fmt.Sprintf("%d/%s", r.Port, r.Proto)}
}

// FirewallStatus is `ufw status`; when ufw is inactive, the rules it has
// stored (`ufw show added`).
type FirewallStatus struct {
	Active bool
	Rules  []string
}

// Firewall manages ufw.
type Firewall interface {
	Status(ctx context.Context) (FirewallStatus, error)
	Allow(ctx context.Context, r FirewallRule) error
	Deny(ctx context.Context, r FirewallRule) error
	// Delete removes the allow and the deny rule for r, whichever exists.
	Delete(ctx context.Context, r FirewallRule) error
	// DeleteDeny removes only the deny rule for r, and reports whether
	// there was one.
	DeleteDeny(ctx context.Context, r FirewallRule) (bool, error)
	// Enable turns ufw on, with the rules it has stored.
	Enable(ctx context.Context) error
}

// UFW is the real Firewall.
type UFW struct{ Exec Exec }

// NewFirewall returns the real adapter.
func NewFirewall(e Exec) *UFW { return &UFW{Exec: e} }

// ufwEnv pins the C locale: ufw's status line is translated otherwise.
var ufwEnv = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}

func (u *UFW) run(ctx context.Context, args ...string) ([]byte, error) {
	res, err := u.Exec.Run(ctx, Cmd{Name: "ufw", Args: args, Env: ufwEnv})
	if errors.Is(err, exec.ErrNotFound) {
		return nil, fmt.Errorf("ufw: %w", ErrUnavailable)
	}
	return res.Stdout, err
}

func (u *UFW) Status(ctx context.Context) (FirewallStatus, error) {
	out, err := u.run(ctx, "status")
	if err != nil {
		return FirewallStatus{}, err
	}
	var st FirewallStatus
	lines := strings.Split(string(out), "\n")
	if len(lines) > 0 {
		st.Active = strings.TrimSpace(lines[0]) == "Status: active"
	}
	if !st.Active {
		// An inactive ufw prints no rules, though it keeps them and applies
		// them as soon as it is enabled: `ufw show added` lists them, one
		// command per line ("ufw allow 22/tcp").
		return u.storedRules(ctx, st)
	}
	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "To ") || strings.HasPrefix(line, "--") {
			continue
		}
		st.Rules = append(st.Rules, line)
	}
	return st, nil
}

// storedRules fills st.Rules from `ufw show added`, without its header,
// its "(None)" and the leading "ufw " of each rule.
func (u *UFW) storedRules(ctx context.Context, st FirewallStatus) (FirewallStatus, error) {
	out, err := u.run(ctx, "show", "added")
	if err != nil {
		return FirewallStatus{}, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "(None)" || strings.HasPrefix(line, "Added user rules") {
			continue
		}
		st.Rules = append(st.Rules, strings.TrimPrefix(line, "ufw "))
	}
	return st, nil
}

func (u *UFW) Allow(ctx context.Context, r FirewallRule) error {
	if err := r.validate(); err != nil {
		return err
	}
	_, err := u.run(ctx, r.args("allow")...)
	return err
}

func (u *UFW) Deny(ctx context.Context, r FirewallRule) error {
	if err := r.validate(); err != nil {
		return err
	}
	_, err := u.run(ctx, r.args("deny")...)
	return err
}

// Delete runs `ufw delete deny` then `ufw delete allow`. ufw exits 0 for a
// rule that does not exist, so both are always attempted and either error
// is reported.
func (u *UFW) Delete(ctx context.Context, r FirewallRule) error {
	if err := r.validate(); err != nil {
		return err
	}
	_, derr := u.run(ctx, append([]string{"delete"}, r.args("deny")...)...)
	_, aerr := u.run(ctx, append([]string{"delete"}, r.args("allow")...)...)
	return errors.Join(derr, aerr)
}

// DeleteDeny runs only `ufw delete deny` for r, and reports whether ufw
// found such a rule. ufw exits 0 either way: it says "Rule deleted" (an
// active ufw) or "Rules updated" (an inactive one, which only rewrites its
// stored rules) for each address family it removed the rule from, and
// "Could not delete non-existent rule" for each it did not find it in.
func (u *UFW) DeleteDeny(ctx context.Context, r FirewallRule) (bool, error) {
	if err := r.validate(); err != nil {
		return false, err
	}
	out, err := u.run(ctx, append([]string{"delete"}, r.args("deny")...)...)
	if err != nil {
		return false, err
	}
	return ufwRemoved(string(out)), nil
}

// ufwRemoved reports whether a `ufw delete` output says a rule was removed.
func ufwRemoved(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Rule deleted") || strings.HasPrefix(line, "Rules updated") {
			return true
		}
	}
	return false
}

// Enable runs `ufw --force enable`: without --force ufw asks on its
// terminal whether to go on, since enabling may cut SSH off. The caller
// allows what must stay reachable first (system.FirewallEnable).
func (u *UFW) Enable(ctx context.Context) error {
	_, err := u.run(ctx, "--force", "enable")
	return err
}

var _ Firewall = (*UFW)(nil)
