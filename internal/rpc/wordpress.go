package rpc

import "github.com/cloudground-it/cloudground/internal/state"

// WPParams names a WordPress site for a wp-cli method. What is the update
// target of WPUpdate ("core", "plugins", "themes" or "all") and the backend
// of WPObjectCache ("" or "apcu"); Enable turns the object cache on or off.
type WPParams struct {
	Site   state.Site `json:"site"`
	What   string     `json:"what,omitempty"`
	Enable bool       `json:"enable,omitempty"`
}

// Sites lists the WordPress site.
func (p *WPParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// WPLoginResult is a one-time login link.
type WPLoginResult struct {
	URL string `json:"url"`
}

// WPPluginsResult is the installed plugins and themes.
type WPPluginsResult struct {
	Plugins []WPPlugin `json:"plugins"`
	Themes  []WPPlugin `json:"themes"`
}

// WPPlugin is one plugin or theme, as wp-cli lists it; Update says whether
// a newer version is available.
type WPPlugin struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Version string `json:"version"`
	Update  string `json:"update"`
}
