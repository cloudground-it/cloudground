package system

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

// FirewallStatus is ufw's state and its rule lines (SPEC.md §4).
func (o *Ops) FirewallStatus(ctx context.Context) (rpc.FirewallStatusResult, error) {
	st, err := o.Firewall.Status(ctx)
	if errors.Is(err, host.ErrUnavailable) {
		return rpc.FirewallStatusResult{Rules: []string{"ufw is not available"}}, nil
	}
	if err != nil {
		return rpc.FirewallStatusResult{}, err
	}
	return rpc.FirewallStatusResult{Enabled: st.Active, Rules: st.Rules}, nil
}

// EssentialPorts are the ports whose closing takes the server off the air:
// every SSH port, the sites' 80 and 443, and the panel's own.
func (o *Ops) EssentialPorts(ctx context.Context) map[int]bool {
	ports := map[int]bool{22: true, 80: true, 443: true, PanelPort: true}
	for _, p := range SSHPorts(ctx, o.Exec, o.Paths.SSHDConfig) {
		ports[p] = true
	}
	return ports
}

// FirewallRule allows, denies or deletes one rule, and refuses anything that
// would close an essential port.
func (o *Ops) FirewallRule(ctx context.Context, p rpc.FirewallRuleParams) (map[string]string, error) {
	r := host.FirewallRule{Port: p.Port, Proto: p.Proto, From: p.From}
	if r.Proto != "udp" {
		r.Proto = "tcp"
	}
	if r.Port < 1 || r.Port > 65535 {
		return nil, agentrpc.Invalid("invalid port %d", r.Port)
	}
	if r.From != "" && net.ParseIP(r.From) == nil {
		if _, _, err := net.ParseCIDR(r.From); err != nil {
			return nil, agentrpc.Invalid("invalid source address %q", r.From)
		}
	}
	var apply func(context.Context, host.FirewallRule) error
	switch p.Action {
	case "allow":
		apply = o.Firewall.Allow
	case "deny":
		apply = o.Firewall.Deny
	case "delete":
		apply = o.Firewall.Delete
	default:
		return nil, agentrpc.Invalid("invalid action %q", p.Action)
	}
	essential := o.EssentialPorts(ctx)[r.Port]
	if essential && p.Action == "deny" && everySource(r.From) {
		return nil, agentrpc.Invalid("refusing to close port %d: it carries SSH, the sites or the panel, and closing it would cut this server off", r.Port)
	}
	if essential && p.Action == "delete" {
		// Removing an allow rule locks out as surely as a deny (ufw's
		// incoming default is deny), so on an essential port only the deny
		// rule of that shape is removed, which can only widen access. The
		// allow rules stay.
		removed, err := o.Firewall.DeleteDeny(ctx, r)
		if err != nil {
			return nil, err
		}
		if !removed {
			return nil, agentrpc.Invalid("refusing to remove the allow rule of port %d: it carries SSH, the sites or the panel, and closing it would cut this server off (only a deny rule can be deleted on this port, and there is none of that shape)", r.Port)
		}
		return map[string]string{"rule": fmt.Sprintf("delete deny %d/%s", r.Port, r.Proto)}, nil
	}
	if err := apply(ctx, r); err != nil {
		return nil, err
	}
	return map[string]string{"rule": fmt.Sprintf("%s %d/%s", p.Action, r.Port, r.Proto)}, nil
}

// everySource reports whether a rule's source is every address: none, or a
// CIDR of prefix length 0 (0.0.0.0/0, ::/0), which ufw reads as "any". A
// deny from it on an essential port is a port-wide deny.
func everySource(from string) bool {
	if from == "" {
		return true
	}
	_, n, err := net.ParseCIDR(from)
	if err != nil {
		return false
	}
	ones, _ := n.Mask.Size()
	return ones == 0
}
