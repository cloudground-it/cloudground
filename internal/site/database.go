package site

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/cloudground-it/cloudground/internal/state"
)

// A site's databases and database accounts are named by the person who
// creates them (Fase 9b). The names stay inside one safe shape, well under
// MariaDB's limits (64 characters for a database, 80 for an account):
// a lowercase letter, then lowercase letters, digits and underscores, 3 to
// 48 characters. Nothing in that shape needs quoting in a defaults file, a
// wp-config.php or a dump's file name.
var dbIdentRe = regexp.MustCompile(`^[a-z][a-z0-9_]{2,47}$`)

// panelDefaultRe is the shape of the names the panel gives a site itself
// (site_<id>, SR-R1): one site's default may never be claimed by another.
var panelDefaultRe = regexp.MustCompile(`^site_[1-9][0-9]{0,18}$`)

// reservedDBNames are the server's own schemas and accounts, and names
// tools and distributions create: never a site's, whatever its owner asks.
// mariadb.sys and other dotted names already fail the shape.
var reservedDBNames = map[string]bool{
	"mysql": true, "sys": true, "performance_schema": true, "information_schema": true,
	"root": true, "debian_sys_maint": true, "mariadb_sys": true, "mysql_sys": true,
	"mariadb": true, "mysqld": true, "admin": true, "test": true, "phpmyadmin": true,
	"cloudground": true, "replication": true, "repl": true, "monitor": true, "backup": true,
}

// reservedDBPrefixes are prefixes no site's name may start with: the
// agent's own scoped accounts and scratch databases (cgq_, cgv_, cgm_, cgh_
// and any cg<letter>_ to come), the server's (mysql_, sys_), and test_,
// which old MariaDB installs grant to every account.
var reservedDBPrefixes = []string{"mysql_", "sys_", "test_", "mariadb_"}

// CheckDBName says why name cannot be a site's database or account name,
// or returns nil. It does not know which site asks: PanelDefault names are
// checked by ClaimableBy.
func CheckDBName(name string) error {
	if !dbIdentRe.MatchString(name) {
		return fmt.Errorf("%q is not a valid name: use 3 to 48 lowercase letters, digits and underscores, starting with a letter", name)
	}
	if reservedDBNames[name] {
		return fmt.Errorf("%q is reserved", name)
	}
	if len(name) > 3 && name[0] == 'c' && name[1] == 'g' && name[3] == '_' {
		return fmt.Errorf("%q is reserved: names starting with cg?_ are the agent's own", name)
	}
	for _, p := range reservedDBPrefixes {
		if strings.HasPrefix(name, p) {
			return fmt.Errorf("%q is reserved: names starting with %s are the server's", name, p)
		}
	}
	return nil
}

// ValidDBName reports whether name has the shape of a site's database or
// account name and is not reserved.
func ValidDBName(name string) bool { return CheckDBName(name) == nil }

// ClaimableBy says why the site with this id may not take name, or nil: it
// must be a valid name, and a name of the panel's default shape (site_<n>)
// only for the site whose id is n.
func ClaimableBy(name string, siteID int64) error {
	if err := CheckDBName(name); err != nil {
		return err
	}
	if panelDefaultRe.MatchString(name) && name != PanelDefault(siteID) {
		return fmt.Errorf("%q is reserved for the site with that number", name)
	}
	if strings.HasPrefix(name, "site_") && !panelDefaultRe.MatchString(name) && isDigits(name[len("site_"):]) {
		return fmt.Errorf("%q is reserved for the panel's own names", name)
	}
	return nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// PanelDefault is the name the panel proposes for the database, and the
// account, of the site with this id: site_<id>.
func PanelDefault(id int64) string { return "site_" + strconv.FormatInt(id, 10) }

// PanelDefaultSite returns the site id a name of the panel's default shape
// names, or 0.
func PanelDefaultSite(name string) int64 {
	if !panelDefaultRe.MatchString(name) {
		return 0
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(name, "site_"), 10, 64)
	if err != nil {
		return 0
	}
	return id
}

// Grant levels of a database account on one of its site's databases.
const (
	GrantAll  = "all"
	GrantRead = "read"
)

// ValidGrant reports whether level is a grant level.
func ValidGrant(level string) bool { return level == GrantAll || level == GrantRead }

// checkDatabase checks the site's primary database, the one its
// application is configured with: both names valid and claimable by the
// site. Which databases a site owns is the agent's record (ops/db SPEC §2);
// an external database is the site's own business and is never
// administered here.
func checkDatabase(s state.Site) error {
	d := s.Config.Database
	if !d.Enabled || d.External {
		return nil
	}
	if s.ID <= 0 {
		return invalid("a site without an id has no database")
	}
	if err := ClaimableBy(d.Name, s.ID); err != nil {
		return invalid("the site's database: %s", err)
	}
	if err := ClaimableBy(d.User, s.ID); err != nil {
		return invalid("the site's database account: %s", err)
	}
	return nil
}
