package site

import "github.com/cloudground-it/cloudground/internal/state"

// SupportsPostgres reports whether a site of this type can use a PostgreSQL
// database (internal/api SPEC §6.5.2): PHP and Laravel sites. WordPress and
// WooCommerce need MySQL or MariaDB; a static site or a proxy runs no code.
func SupportsPostgres(t state.SiteType) bool {
	return t == state.SitePHP || t == state.SiteLaravel
}
