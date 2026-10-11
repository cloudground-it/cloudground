package rpc

import "github.com/cloudground-it/cloudground/internal/state"

// CreateSiteParams asks for a whole site to be provisioned: its user, its
// tree, its database, its PHP master, its vhost and, for WordPress, the
// installation itself.
type CreateSiteParams struct {
	Site state.Site `json:"site"`
	// DBPassword is the password of the site's own database account.
	DBPassword string `json:"db_password,omitempty"`
	// ConnectorToken is what the site's connector presents to the panel's
	// purge endpoint.
	ConnectorToken string `json:"connector_token,omitempty"`

	// The first administrator and the title of a WordPress site; other
	// site types ignore them.
	AdminEmail    string `json:"admin_email,omitempty"`
	AdminUser     string `json:"admin_user,omitempty"`
	AdminPassword string `json:"admin_password,omitempty"`
	SiteTitle     string `json:"site_title,omitempty"`
}

// Sites lists the site to create.
func (p *CreateSiteParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// CreateSiteResult is what the agent made for the new site.
type CreateSiteResult struct {
	SystemUser   string        `json:"system_user"`
	RootPath     string        `json:"root_path"`
	DatabaseName string        `json:"database_name,omitempty"`
	DatabaseUser string        `json:"database_user,omitempty"`
	Files        []ManagedFile `json:"files"`
}

// ManagedFile is a file the agent rendered for the panel, with the SHA-256
// of what it wrote. The API records it and later asks whether it drifted.
type ManagedFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// SiteRef names an existing site: DeleteSite and ApplySiteConfig.
type SiteRef struct {
	Site state.Site `json:"site"`
	// ReloadOnlyIfChanged, for ApplySiteConfig, skips the PHP master's and
	// nginx's reload when every file rendered for them is byte-identical to
	// the one on disk. A reload would restart every PHP worker and empty
	// OPcache for nothing.
	ReloadOnlyIfChanged bool `json:"reload_only_if_changed,omitempty"`
}

// Sites lists the site named.
func (p *SiteRef) Sites() []*state.Site { return []*state.Site{&p.Site} }

// ApplyResult lists the managed files a render (re)wrote.
type ApplyResult struct {
	Files []ManagedFile `json:"files"`
}
