package api

// Host names are unique across the panel: no name is a domain or an alias
// of two sites (SPEC.md §3.4). Every write that can add a name — a create,
// a staging copy, a patch of aliases, the put-back of a failed or canceled
// one — checks and writes under aliasesMu, one lock for the whole panel, so
// two of them cannot both take the same name.

import (
	"slices"
	"sync"

	"github.com/cloudground-it/cloudground/internal/state"
)

var aliasesMu sync.Mutex

// nameOwners maps each of names already used by a site other than self
// (as its domain or one of its aliases) to that site's domain.
func nameOwners(st *state.Store, names []string, self int64) (map[string]string, error) {
	taken := map[string]string{}
	if len(names) == 0 {
		return taken, nil
	}
	all, err := st.ListSites()
	if err != nil {
		return nil, err
	}
	for _, other := range all {
		if other.ID == self {
			continue
		}
		for _, n := range names {
			if n == other.Domain || slices.Contains(other.Aliases, n) {
				taken[n] = other.Domain
			}
		}
	}
	return taken, nil
}
