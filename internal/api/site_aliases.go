package api

// Host names are unique across the panel: no name is a domain or an alias
// of two sites (SPEC.md §3.4), nor a site's and a function's, the functions
// domain or the panel's own (§17.4). Every write that can add a name — a
// create, a staging copy, a patch of aliases, the put-back of a failed or
// canceled one, a function's domains, the functions domain — checks and
// writes under aliasesMu, one lock for the whole panel, so two of them
// cannot both take the same name.

import (
	"slices"
	"strings"
	"sync"

	"github.com/cloudground-it/cloudground/internal/state"
)

var aliasesMu sync.Mutex

// nameOwners maps each of names already used by a site other than self (as
// its domain or one of its aliases) to that site's domain, and each used by
// a function, the functions domain or the panel to a description of it (a
// text with a space, which no domain has: siteOwner).
func nameOwners(st *state.Store, names []string, self int64) (map[string]string, error) {
	return hostOwners(st, names, nameSelf{site: self})
}

// siteOwner reports whether an owner nameOwners gave is a site's domain.
func siteOwner(owner string) bool { return !strings.Contains(owner, " ") }

// nameSelf is what a check leaves out: the site, the function or the
// functions domain whose own names they are.
type nameSelf struct {
	site            int64
	function        string
	functionsDomain bool
}

// hostOwners maps each of names already used anywhere but by self to its
// owner (nameOwners).
func hostOwners(st *state.Store, names []string, self nameSelf) (map[string]string, error) {
	taken := map[string]string{}
	if len(names) == 0 {
		return taken, nil
	}
	all, err := st.ListSites()
	if err != nil {
		return nil, err
	}
	for _, other := range all {
		if other.ID == self.site && self.site != 0 {
			continue
		}
		for _, n := range names {
			if n == other.Domain || slices.Contains(other.Aliases, n) {
				taken[n] = other.Domain
			}
		}
	}
	doms, err := st.FunctionDomains()
	if err != nil {
		return nil, err
	}
	ps, err := st.PanelSettings()
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		if fn, ok := doms[n]; ok && fn != self.function {
			taken[n] = "the function " + fn
		}
		if n == ps.FunctionsDomain && !self.functionsDomain {
			taken[n] = "the functions domain"
		}
		if n == ps.PanelDomain {
			taken[n] = "the panel's own address"
		}
	}
	return taken, nil
}
