// Package blocklist is the server's block list: the addresses blocked by
// hand or for repeated failed sign-ins, the ones never blocked, the
// counting that decides a block, the parsers of the failures the agent
// sees, and the netfilter table that enforces the list. Pure: the panel
// keeps the list and decides, the agent reports and enforces. SPEC.md is
// the contract.
package blocklist

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

// Sources of a failure (SPEC §3).
const (
	SourceSSH       = "ssh"
	SourceWordPress = "wordpress"
	SourcePanel     = "panel"
	// SourceManual is a block an operator made.
	SourceManual = "manual"
)

// The widest networks a block may name (SPEC §1).
const (
	minBlockBits4 = 8
	minBlockBits6 = 16
)

// ParsePrefix reads an address or a CIDR, masked and unmapped (SPEC §1).
func ParsePrefix(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	bad := fmt.Errorf("%q is not an IP address or a network in CIDR form", s)
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil || p.Addr().Zone() != "" {
			return netip.Prefix{}, bad
		}
		a, bits := p.Addr(), p.Bits()
		if a.Is4In6() {
			if bits < 96 {
				return netip.Prefix{}, bad
			}
			a, bits = a.Unmap(), bits-96
		}
		return netip.PrefixFrom(a, bits).Masked(), nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil || a.Zone() != "" {
		return netip.Prefix{}, bad
	}
	a = a.Unmap()
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// CheckBlockWidth refuses a block wider than /8 (IPv4) or /16 (IPv6): one
// typo away from cutting off most of the Internet, the operator included.
func CheckBlockWidth(p netip.Prefix) error {
	if (p.Addr().Is4() && p.Bits() < minBlockBits4) || (p.Addr().Is6() && p.Bits() < minBlockBits6) {
		return fmt.Errorf("%s is wider than /8 (IPv4) or /16 (IPv6): too broad to block", p)
	}
	return nil
}

// Unit is what a failure counts against and a ban blocks: the address for
// IPv4, its /64 for IPv6 (SPEC §1).
func Unit(a netip.Addr) netip.Prefix {
	a = a.Unmap()
	if a.Is4() {
		return netip.PrefixFrom(a, 32)
	}
	return netip.PrefixFrom(a, 64).Masked()
}

// Exempt is the set of addresses never blocked: loopback, the server's own
// addresses and the allowlist (SPEC §1).
type Exempt struct {
	Own   []netip.Addr
	Allow []netip.Prefix
}

// Has reports whether a is exempt.
func (e Exempt) Has(a netip.Addr) bool {
	a = a.Unmap()
	if a.IsLoopback() || !a.IsValid() {
		return true
	}
	if slices.ContainsFunc(e.Own, func(o netip.Addr) bool { return o.Unmap() == a }) {
		return true
	}
	return slices.ContainsFunc(e.Allow, func(p netip.Prefix) bool { return p.Contains(a) })
}

// Overlaps reports whether p shares an address with an exempt one: a block
// that would cut off loopback, the server itself or an allowed address.
func (e Exempt) Overlaps(p netip.Prefix) bool {
	for _, lo := range []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")} {
		if lo.Overlaps(p) {
			return true
		}
	}
	if slices.ContainsFunc(e.Own, func(o netip.Addr) bool { return p.Contains(o.Unmap()) }) {
		return true
	}
	return slices.ContainsFunc(e.Allow, func(a netip.Prefix) bool { return a.Overlaps(p) })
}

// Settings is the automatic blocking's policy (SPEC §2).
type Settings struct {
	Enabled       bool `json:"enabled"`
	MaxFailures   int  `json:"max_failures"`
	WindowMinutes int  `json:"window_minutes"`
	BanMinutes    int  `json:"ban_minutes"`
}

// DefaultSettings is the shipped policy.
func DefaultSettings() Settings {
	return Settings{Enabled: true, MaxFailures: 6, WindowMinutes: 10, BanMinutes: 60}
}

// Validate refuses a value outside its bounds, naming the field.
func (s Settings) Validate() error {
	switch {
	case s.MaxFailures < 3 || s.MaxFailures > 100:
		return fmt.Errorf("max_failures must be 3–100")
	case s.WindowMinutes < 1 || s.WindowMinutes > 1440:
		return fmt.Errorf("window_minutes must be 1–1440")
	case s.BanMinutes < 1 || s.BanMinutes > 43200:
		return fmt.Errorf("ban_minutes must be 1–43200")
	}
	return nil
}
