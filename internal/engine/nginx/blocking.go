package nginx

import (
	"fmt"
	"net/netip"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/cloudground-it/cloudground/internal/blocklist"
	"github.com/cloudground-it/cloudground/internal/engine"
	"github.com/cloudground-it/cloudground/internal/site"
)

// A site's refused visitors (SPEC §4.7, #25): one http-level file per site
// with the rules, and one `if` in the vhost.

// BlockingFile is where a site's rules go.
func (r Renderer) BlockingFile(id int64) string {
	return filepath.Join(orDefault(r.ConfDir, DefaultConfDir), fmt.Sprintf("cloudground-block-%d.conf", id))
}

// checkBlocking refuses what could not be pasted into the file as it is.
func checkBlocking(b *engine.Blocking) error {
	for _, u := range b.UserAgents {
		if !site.ValidUserAgentPattern(u) {
			return fmt.Errorf("invalid user agent pattern %q", u)
		}
	}
	for _, p := range append(slices.Clone(b.Addresses), b.CountryPrefixes...) {
		if !p.IsValid() {
			return fmt.Errorf("invalid blocked prefix")
		}
	}
	for _, c := range b.Countries {
		if !blocklist.ValidCountry(c) {
			return fmt.Errorf("invalid country %q", c)
		}
	}
	if b.AllowCountriesOnly && len(b.Countries) == 0 {
		return fmt.Errorf("serving only some countries needs a country")
	}
	return nil
}

// blockingFile is the site's rules (SPEC §4.7): a geo of addresses (with
// this machine's marked "x", never refused), a geo of the countries, a map
// of the user agents, and the map that combines them into $cg_block_<id>.
// Every lookup is a hash or radix-tree match but the user agents', one
// case-insensitive regex for all of them.
func (v *vhost) blockingFile() string {
	b, s := v.in.Blocking, v.in.Site
	var c conf
	c.putf(0, "# Written by CloudGround: the visitors %s (site %d) refuses (#25).", s.Domain, s.ID)
	if len(b.Countries) > 0 {
		c.put(0, "# Country data: IP Geolocation by DB-IP (https://db-ip.com), CC BY 4.0.")
	}
	exempt := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}
	for _, a := range b.Exempt {
		a = a.Unmap()
		exempt = append(exempt, netip.PrefixFrom(a, a.BitLen()))
	}
	exempt = blocklist.Minimal(exempt)
	keys := []string{"$cg_block_ip_%[1]d"}
	c.putf(0, "geo $cg_block_ip_%d {", s.ID)
	c.put(1, "default  0;")
	for _, p := range exempt {
		c.putf(1, "%s  x;", p)
	}
	for _, p := range blocklist.Minimal(b.Addresses) {
		if !slices.Contains(exempt, p) {
			c.putf(1, "%s  1;", p)
		}
	}
	c.put(0, "}")
	if len(b.Countries) > 0 {
		keys = append(keys, "$cg_block_cc_%[1]d")
		in, out := "1", "0"
		if b.AllowCountriesOnly {
			in, out = "0", "1"
		}
		codes := slices.Clone(b.Countries)
		slices.Sort(codes)
		what := "Countries refused"
		if b.AllowCountriesOnly {
			what = "The only countries served"
		}
		c.putf(0, "# %s: %s.", what, strings.Join(codes, " "))
		c.putf(0, "geo $cg_block_cc_%d {", s.ID)
		c.putf(1, "default  %s;", out)
		for _, p := range blocklist.Minimal(b.CountryPrefixes) {
			c.putf(1, "%s  %s;", p, in)
		}
		c.put(0, "}")
	}
	if len(b.UserAgents) > 0 {
		keys = append(keys, "$cg_block_ua_%[1]d")
		quoted := make([]string, len(b.UserAgents))
		for i, u := range b.UserAgents {
			quoted[i] = regexp.QuoteMeta(u)
		}
		c.putf(0, "map $http_user_agent $cg_block_ua_%d {", s.ID)
		c.put(1, "default  0;")
		c.putf(1, "\"~*(?:%s)\"  1;", strings.Join(quoted, "|"))
		c.put(0, "}")
	}
	// The combination: refused when any part says 1, unless the address
	// is this machine's (x). Exact keys, a hash lookup.
	c.putf(0, "map \"%s\" $cg_block_%d {", fmt.Sprintf(strings.Join(keys, ""), s.ID), s.ID)
	c.put(1, "default  1;")
	rest := len(keys) - 1
	c.putf(1, "%q  0;", strings.Repeat("0", len(keys)))
	for i := range 1 << rest {
		k := "x"
		for j := range rest {
			k += string("01"[(i>>(rest-1-j))&1])
		}
		c.putf(1, "%q  0;", k)
	}
	c.put(0, "}")
	return c.String()
}
