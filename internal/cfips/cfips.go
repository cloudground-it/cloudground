// Package cfips holds Cloudflare's proxy ranges (#22, SPEC.md): the
// addresses nginx takes a visitor's address from, the ones the block list
// never blocks, and the ones the DNS check calls Cloudflare. Pure: no
// network, no files.
package cfips

import (
	"cmp"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

// shippedV4 and shippedV6 are https://www.cloudflare.com/ips/ as published
// when this release was made (SPEC §1).
var (
	shippedV4 = []string{
		"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22", "141.101.64.0/18",
		"108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20", "197.234.240.0/22", "198.41.128.0/17",
		"162.158.0.0/15", "104.16.0.0/13", "104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
	}
	shippedV6 = []string{
		"2400:cb00::/32", "2606:4700::/32", "2803:f800::/32", "2405:b500::/32", "2405:8100::/32",
		"2a06:98c0::/29", "2c0f:f248::/32",
	}
)

// Bounds of a list (SPEC §2).
const (
	maxPerFamily = 100
	minBits4     = 8
	minBits6     = 20
)

// Ranges is a checked list: each family sorted, without repeats.
type Ranges struct {
	V4 []netip.Prefix
	V6 []netip.Prefix
}

// Shipped is the list this release carries (SPEC §1).
func Shipped() Ranges {
	r, err := Parse(shippedV4, shippedV6)
	if err != nil {
		panic("cfips: the shipped list does not pass its own check: " + err.Error())
	}
	return r
}

// Parse checks the two lists as Cloudflare publishes them (SPEC §2).
func Parse(v4, v6 []string) (Ranges, error) {
	if len(v4) == 0 || len(v4) > maxPerFamily || len(v6) > maxPerFamily {
		return Ranges{}, errors.New("a list of Cloudflare's ranges has 1 to 100 networks of each family")
	}
	four, err := parseFamily(v4, true)
	if err != nil {
		return Ranges{}, err
	}
	six, err := parseFamily(v6, false)
	if err != nil {
		return Ranges{}, err
	}
	return Ranges{V4: four, V6: six}, nil
}

func parseFamily(list []string, want4 bool) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(list))
	for _, raw := range list {
		s := strings.TrimSpace(raw)
		p, err := netip.ParsePrefix(s)
		if err != nil || p.Addr().Zone() != "" {
			return nil, fmt.Errorf("%q is not a network in CIDR form", raw)
		}
		if p.Addr().Is4In6() {
			return nil, fmt.Errorf("%s is in the wrong list", p)
		}
		if p.Addr().Is4() != want4 {
			return nil, fmt.Errorf("%s is in the wrong list", p)
		}
		if p.Masked() != p {
			return nil, fmt.Errorf("%s is not a network address", p)
		}
		a := p.Addr()
		if !a.IsGlobalUnicast() || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() {
			return nil, fmt.Errorf("%s is not a public network", p)
		}
		if (want4 && p.Bits() < minBits4) || (!want4 && p.Bits() < minBits6) {
			return nil, fmt.Errorf("%s is too wide to be one of Cloudflare's networks", p)
		}
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b netip.Prefix) int {
		return cmp.Or(a.Addr().Compare(b.Addr()), cmp.Compare(a.Bits(), b.Bits()))
	})
	return slices.Compact(out), nil
}

// Contains reports whether a is in one of the ranges.
func (r Ranges) Contains(a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range r.Prefixes() {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Prefixes is every range, IPv4 first.
func (r Ranges) Prefixes() []netip.Prefix {
	return slices.Concat(r.V4, r.V6)
}

// Strings is each family as text.
func (r Ranges) Strings() (v4, v6 []string) {
	str := func(ps []netip.Prefix) []string {
		out := make([]string, len(ps))
		for i, p := range ps {
			out[i] = p.String()
		}
		return out
	}
	return str(r.V4), str(r.V6)
}
