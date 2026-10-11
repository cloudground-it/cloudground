package db

import (
	"context"
	"fmt"
	"sort"

	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Adopt rebuilds the agent's record from the panel's claims and the server
// (SPEC §2, "A lost record"): `panel-agent adopt-databases`, run by root on
// the box with the agent stopped, never reachable through the API. A claim
// is adopted when the name passes the site's checks (ClaimableBy: never a
// system or reserved name, never another site's site_<n>), the server has
// it, no other site claims it (in the claims or in the record), and, for a
// grant, the database is adopted for the same site. Nothing is created,
// dropped or granted on the server: only the record is written. What is
// already recorded for the same site stays. dryRun writes nothing.
func (o *Ops) Adopt(ctx context.Context, claims state.DatabaseClaims, dryRun bool) (AdoptReport, error) {
	var rep AdoptReport
	dbs, users, err := o.scanServer(ctx)
	if err != nil {
		return rep, fmt.Errorf("list the server's databases and accounts: %w", err)
	}
	onServer := func(list []string) map[string]bool {
		m := map[string]bool{}
		for _, n := range list {
			m[n] = true
		}
		return m
	}
	haveDB, haveUser := onServer(dbs), onServer(users)

	// A name two sites claim is nobody's.
	claimants := func(names []string, sites []int64) map[string]bool {
		owner, twice := map[string]int64{}, map[string]bool{}
		for i, n := range names {
			if prev, ok := owner[n]; ok && prev != sites[i] {
				twice[n] = true
			}
			owner[n] = sites[i]
		}
		return twice
	}
	var dn, un []string
	var ds, us []int64
	for _, c := range claims.Databases {
		dn, ds = append(dn, c.Name), append(ds, c.SiteID)
	}
	for _, c := range claims.Users {
		un, us = append(un, c.Name), append(us, c.SiteID)
	}
	dbTwice, userTwice := claimants(dn, ds), claimants(un, us)

	apply := o.registry().update
	if dryRun {
		apply = o.registry().dryUpdate
	}
	err = apply(ctx, func(f *registryFile) error {
		adoptedDB := map[string]int64{}
		for _, c := range claims.Databases {
			why := ""
			switch {
			case c.SiteID <= 0:
				why = "no site"
			case site.ClaimableBy(c.Name, c.SiteID) != nil:
				why = site.ClaimableBy(c.Name, c.SiteID).Error()
			case dbTwice[c.Name]:
				why = "two sites claim it"
			case f.Databases[c.Name] != 0 && f.Databases[c.Name] != c.SiteID:
				why = fmt.Sprintf("the record gives it to site %d", f.Databases[c.Name])
			case !haveDB[c.Name]:
				why = "the server has no database of that name"
			}
			if why != "" {
				rep.Refused = append(rep.Refused, fmt.Sprintf("database %s (site %d): %s", c.Name, c.SiteID, why))
				continue
			}
			adoptedDB[c.Name] = c.SiteID
			if f.Databases[c.Name] == c.SiteID {
				rep.Kept = append(rep.Kept, fmt.Sprintf("database %s (site %d)", c.Name, c.SiteID))
				continue
			}
			f.Databases[c.Name] = c.SiteID
			rep.Adopted = append(rep.Adopted, fmt.Sprintf("database %s for site %d", c.Name, c.SiteID))
		}
		for _, c := range claims.Users {
			why := ""
			switch {
			case c.SiteID <= 0:
				why = "no site"
			case site.ClaimableBy(c.Name, c.SiteID) != nil:
				why = site.ClaimableBy(c.Name, c.SiteID).Error()
			case userTwice[c.Name]:
				why = "two sites claim it"
			case f.Users[c.Name].Site != 0 && f.Users[c.Name].Site != c.SiteID:
				why = fmt.Sprintf("the record gives it to site %d", f.Users[c.Name].Site)
			case !haveUser[c.Name]:
				why = "the server has no account of that name"
			}
			if why != "" {
				rep.Refused = append(rep.Refused, fmt.Sprintf("user %s (site %d): %s", c.Name, c.SiteID, why))
				continue
			}
			u, known := f.Users[c.Name]
			if !known {
				u = registryUser{Site: c.SiteID, Grants: map[string]string{}}
			}
			var granted []string
			for _, g := range c.Grants {
				if adoptedDB[g.Database] != c.SiteID || !site.ValidGrant(g.Level) {
					rep.Refused = append(rep.Refused, fmt.Sprintf("grant of %s on %s: the database is not adopted for site %d", c.Name, g.Database, c.SiteID))
					continue
				}
				u.Grants[g.Database] = g.Level
				granted = append(granted, g.Database+":"+g.Level)
			}
			sort.Strings(granted)
			f.Users[c.Name] = u
			line := fmt.Sprintf("user %s for site %d (grants %v)", c.Name, c.SiteID, granted)
			if known {
				rep.Kept = append(rep.Kept, line)
			} else {
				rep.Adopted = append(rep.Adopted, line)
			}
		}
		if dryRun {
			return errDryRun
		}
		return nil
	})
	if err == errDryRun {
		return rep, nil
	}
	return rep, err
}

// errDryRun stops the record's update without an error to report.
var errDryRun = fmt.Errorf("dry run")

// AdoptReport is what Adopt did, one line per name.
type AdoptReport struct {
	Adopted []string
	Kept    []string
	Refused []string
}
