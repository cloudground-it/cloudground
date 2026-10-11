package blocklist

import (
	"net/netip"
	"slices"
	"strings"
)

// NftTable is the netfilter table the block list lives in (SPEC §5).
const NftTable = "cloudground"

// NftScript is the `nft -f` input that replaces the table with one
// enforcing block and allow (SPEC §5).
func NftScript(block, allow []netip.Prefix) string {
	var b strings.Builder
	b.WriteString("table inet " + NftTable + "\n")
	b.WriteString("delete table inet " + NftTable + "\n")
	b.WriteString("table inet " + NftTable + " {\n")
	allow4, allow6 := byFamily(allow)
	block4, block6 := byFamily(block)
	set(&b, "allow4", "ipv4_addr", allow4)
	set(&b, "allow6", "ipv6_addr", allow6)
	set(&b, "block4", "ipv4_addr", block4)
	set(&b, "block6", "ipv6_addr", block6)
	b.WriteString("\tchain input {\n" +
		"\t\ttype filter hook input priority -10; policy accept;\n" +
		"\t\tiif \"lo\" accept\n" +
		"\t\tip saddr @allow4 accept\n" +
		"\t\tip6 saddr @allow6 accept\n" +
		"\t\tip saddr @block4 drop\n" +
		"\t\tip6 saddr @block6 drop\n" +
		"\t}\n}\n")
	return b.String()
}

func set(b *strings.Builder, name, typ string, elems []netip.Prefix) {
	b.WriteString("\tset " + name + " {\n\t\ttype " + typ + "; flags interval;\n")
	if len(elems) > 0 {
		s := make([]string, len(elems))
		for i, p := range elems {
			s[i] = p.String()
		}
		b.WriteString("\t\telements = { " + strings.Join(s, ", ") + " }\n")
	}
	b.WriteString("\t}\n")
}

// byFamily splits ps by family, each sorted, without a prefix another one
// already covers: an interval set refuses overlapping elements.
func byFamily(ps []netip.Prefix) (v4, v6 []netip.Prefix) {
	for _, p := range ps {
		if !p.IsValid() {
			continue
		}
		p = p.Masked()
		if p.Addr().Is4() {
			v4 = append(v4, p)
		} else {
			v6 = append(v6, p)
		}
	}
	return Minimal(v4), Minimal(v6)
}

// Minimal sorts ps and leaves out every prefix another one covers.
func Minimal(ps []netip.Prefix) []netip.Prefix {
	ps = slices.Clone(ps)
	slices.SortFunc(ps, func(a, b netip.Prefix) int {
		if c := a.Addr().Compare(b.Addr()); c != 0 {
			return c
		}
		return a.Bits() - b.Bits() // the wider first
	})
	var out []netip.Prefix
	for _, p := range ps {
		// Sorted by start address, a covering prefix is the last kept one
		// whenever there is one.
		if n := len(out); n > 0 && out[n-1].Bits() <= p.Bits() && out[n-1].Contains(p.Addr()) {
			continue
		}
		out = append(out, p)
	}
	return out
}
