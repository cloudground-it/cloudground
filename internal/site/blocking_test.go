package site

import (
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/state"
)

// #25: a site's rules are pasted into nginx's configuration.
func TestCheckBlocking(t *testing.T) {
	ok := state.SiteBlocking{Addresses: []string{"203.0.113.0/24", "2001:db8::/48"}, UserAgents: []string{"GPTBot", "Mozilla/5.0 (compatible; X)"}, Countries: []string{"CN", "RU"}}
	if err := CheckBlocking(ok); err != nil {
		t.Fatal(err)
	}
	if err := CheckBlocking(state.SiteBlocking{Countries: []string{"IT"}, AllowCountriesOnly: true}); err != nil {
		t.Fatal(err)
	}
	many := make([]string, MaxBlockCountries+1)
	for i := range many {
		many[i] = "A" + string(rune('A'+i%26))
	}
	for name, b := range map[string]state.SiteBlocking{
		"not an address":     {Addresses: []string{"x"}},
		"not canonical":      {Addresses: []string{"203.0.113.9/24"}},
		"too wide":           {Addresses: []string{"10.0.0.0/7"}},
		"agent with a quote": {UserAgents: []string{`a"b`}},
		"agent with braces":  {UserAgents: []string{"a{1}"}},
		"agent too long":     {UserAgents: []string{strings.Repeat("a", 101)}},
		"agent not ascii":    {UserAgents: []string{"bøt"}},
		"lower-case country": {Countries: []string{"cn"}},
		"too many countries": {Countries: many},
		"only, no country":   {AllowCountriesOnly: true},
		"named twice":        {UserAgents: []string{"GPTBot", "GPTBot"}},
	} {
		if err := CheckBlocking(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
