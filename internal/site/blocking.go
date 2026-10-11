package site

import (
	"slices"

	"github.com/cloudground-it/cloudground/internal/blocklist"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Bounds of a site's blocking rules (#25, internal/engine SPEC §4.7).
const (
	MaxBlockAddresses  = 500
	MaxBlockUserAgents = 50
	MaxBlockCountries  = 50
	maxUserAgentLen    = 100
)

// ValidUserAgentPattern reports whether p may be matched against a
// visitor's User-Agent: 2–100 printable ASCII characters, none of which a
// quoted nginx string would read as syntax (" \ $ { }).
func ValidUserAgentPattern(p string) bool {
	if len(p) < 2 || len(p) > maxUserAgentLen {
		return false
	}
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c < 0x20 || c > 0x7e || c == '"' || c == '\\' || c == '$' || c == '{' || c == '}' {
			return false
		}
	}
	return true
}

// CheckBlocking refuses rules that could not be rendered as they are: each
// is pasted into nginx's configuration.
func CheckBlocking(b state.SiteBlocking) error {
	switch {
	case len(b.Addresses) > MaxBlockAddresses:
		return invalid("at most %d blocked addresses", MaxBlockAddresses)
	case len(b.UserAgents) > MaxBlockUserAgents:
		return invalid("at most %d blocked user agents", MaxBlockUserAgents)
	case len(b.Countries) > MaxBlockCountries:
		return invalid("at most %d countries", MaxBlockCountries)
	case b.AllowCountriesOnly && len(b.Countries) == 0:
		return invalid("serving only some countries needs at least one country")
	}
	for _, a := range b.Addresses {
		p, err := blocklist.ParsePrefix(a)
		if err != nil {
			return invalid("%s", err.Error())
		}
		if err := blocklist.CheckBlockWidth(p); err != nil {
			return invalid("%s", err.Error())
		}
		if p.String() != a {
			return invalid("blocked address %q is not in its canonical form %s", a, p)
		}
	}
	for _, u := range b.UserAgents {
		if !ValidUserAgentPattern(u) {
			return invalid("blocked user agent %q: 2–100 printable characters, without \" \\ $ { }", u)
		}
	}
	for _, c := range b.Countries {
		if !blocklist.ValidCountry(c) {
			return invalid("%q is not a country code (two capital letters, ISO 3166-1)", c)
		}
	}
	for _, list := range [][]string{b.Addresses, b.UserAgents, b.Countries} {
		sorted := slices.Clone(list)
		slices.Sort(sorted)
		if len(slices.Compact(sorted)) != len(list) {
			return invalid("a blocking rule is named twice")
		}
	}
	return nil
}
