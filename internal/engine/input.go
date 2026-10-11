// Package engine is the contract between the agent and the web server it
// configures: an Input describing one site goes in, the files the server
// reads come out. SPEC.md in this directory is the specification; nginx is
// the one engine there is (internal/engine/nginx).
package engine

import (
	"net/netip"

	"github.com/cloudground-it/cloudground/internal/state"
	"github.com/cloudground-it/cloudground/internal/velocity"
)

// Input is one site as the engine renders it (SPEC §1). The agent fills in
// the paths of this machine; the engine touches no file.
type Input struct {
	Site   state.Site
	Policy velocity.CachePolicy

	// DocRoot is served: <root>/current, plus the public root if any.
	DocRoot string
	// SymlinkFrom, when set, is where nginx's disable_symlinks check starts
	// instead of the document root: the release (<root>/current) of a site
	// with a public root, so a link inside the public root is checked too.
	SymlinkFrom string
	// PHPSocket is the site's PHP-FPM socket, "" for static and proxy sites.
	PHPSocket string
	CacheDir  string
	LogDir    string

	// The site's certificate, and whether it exists yet: until it does the
	// fallback is served, so the vhost loads before issuance.
	CertAvailable bool
	CertFullchain string
	CertKey       string
	FallbackCert  string
	FallbackKey   string
	ACMEWebroot   string
	// ClientMaxBody is nginx's request body limit; "" is 128m.
	ClientMaxBody string

	// What this nginx build can do, read from `nginx -V`: QUIC (the v3
	// module; Ubuntu 24.04's 1.24 has none, 26.04's has it), and the
	// "http2 on;" directive that 1.25.1 introduced in place of the listen
	// parameter it deprecated.
	HTTP3          bool
	HTTP2Directive bool

	// Staging: the password file nginx asks against, and the header value
	// that passes it for the panel's and the agent's own requests.
	BasicAuthFile string
	InternalKey   string

	// What a custom routing block must not reach (SPEC §7.3): the ports
	// system services listen on here, this machine's own addresses, and a
	// resolver for proxy host names. The agent fills them in at every
	// render; nil leaves only the fixed rules.
	SystemPorts []int
	LocalAddrs  []netip.Addr
	Resolve     func(host string) ([]netip.Addr, error)
}

// RoutingView is a site's routing as the editor shows it (SPEC §7.5).
type RoutingView struct {
	// Default is the site type's routing block; Current the stored one
	// ("" while the default is used).
	Default string `json:"default"`
	Current string `json:"current"`
	// Before and After are the rendered vhost around the block: the
	// panel's part, read-only.
	Before string `json:"before"`
	After  string `json:"after"`
}

// Renderer is an engine.
type Renderer interface {
	// SiteFiles is one site's files, absolute path → content.
	SiteFiles(in Input) (map[string]string, error)
	// GlobalFiles are the files every site shares.
	GlobalFiles() map[string]string
	// Routing is the site's routing block and the vhost around it.
	Routing(in Input) (RoutingView, error)
	Name() state.Engine
}
