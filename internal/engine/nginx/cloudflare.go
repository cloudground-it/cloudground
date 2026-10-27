package nginx

import (
	"cmp"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"

	"github.com/cloudground-it/cloudground/internal/cfips"
)

// The two files the sites behind Cloudflare share (SPEC §2, §4.10). The
// agent writes them (ops/sites SPEC §10), not SiteFiles: they belong to no
// one site.
const (
	cloudflareIncludeName = "cloudground-cloudflare.inc"
	bannedName            = "cloudground-banned.conf"
)

// CloudflareIncludeFile is where Cloudflare's ranges are kept for the
// vhosts to include. Not .conf: conf.d/*.conf would load it at http level.
func (r Renderer) CloudflareIncludeFile() string {
	return filepath.Join(orDefault(r.ConfDir, DefaultConfDir), cloudflareIncludeName)
}

// BannedFile is the server's block list as an http-level geo.
func (r Renderer) BannedFile() string {
	return filepath.Join(orDefault(r.ConfDir, DefaultConfDir), bannedName)
}

// CloudflareInclude is the include of the ranges nginx takes
// CF-Connecting-IP from: one set_real_ip_from each, and nothing else.
func CloudflareInclude(r cfips.Ranges) string {
	var b strings.Builder
	b.WriteString("# Written by CloudGround: Cloudflare's ranges, the only addresses whose\n")
	b.WriteString("# CF-Connecting-IP header is taken as the visitor's (#22).\n")
	for _, p := range r.Prefixes() {
		b.WriteString("set_real_ip_from " + p.String() + ";\n")
	}
	return b.String()
}

// Banned is the block list as the geo $cg_banned: 1 for a blocked
// address, 0 for an allowed one. A block inside an allowed prefix is left
// out: the longest prefix wins in a geo, and in netfilter the allowlist
// wins over every block.
func Banned(block, allow []netip.Prefix) string {
	type entry struct {
		p   netip.Prefix
		val string
	}
	var list []entry
	for _, a := range allow {
		list = append(list, entry{a.Masked(), "0"})
	}
	for _, p := range block {
		p = p.Masked()
		if slices.ContainsFunc(allow, func(a netip.Prefix) bool { return a.Bits() <= p.Bits() && a.Contains(p.Addr()) }) {
			continue
		}
		list = append(list, entry{p, "1"})
	}
	slices.SortFunc(list, func(a, b entry) int {
		return cmp.Or(a.p.Addr().Compare(b.p.Addr()), cmp.Compare(a.p.Bits(), b.p.Bits()), cmp.Compare(a.val, b.val))
	})
	list = slices.CompactFunc(list, func(a, b entry) bool { return a.p == b.p })
	var b strings.Builder
	b.WriteString("# Written by CloudGround: the server's block list, for the sites behind\n")
	b.WriteString("# Cloudflare, whose visitors netfilter cannot see (#22).\n")
	b.WriteString("geo $cg_banned {\n    default  0;\n")
	for _, e := range list {
		b.WriteString("    " + e.p.String() + "  " + e.val + ";\n")
	}
	b.WriteString("}\n")
	return b.String()
}
