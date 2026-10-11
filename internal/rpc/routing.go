package rpc

import "github.com/cloudground-it/cloudground/internal/state"

// RoutingView is a site's routing block as the editor shows it
// (internal/engine SPEC §7.5): the type's default, the stored block ("" while
// the default is used) and the panel's rendered vhost around it.
type RoutingView struct {
	Default string `json:"default"`
	Current string `json:"current"`
	Before  string `json:"before"`
	After   string `json:"after"`
}

// RoutingParams asks for a site's routing block to be replaced: "" (or the
// type's default) restores the default.
type RoutingParams struct {
	Site    state.Site `json:"site"`
	Routing string     `json:"routing"`
}

// Sites lists the site whose routing changes.
func (p *RoutingParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// RoutingResult is what the agent applied: the block to store ("" for the
// default) and the files the render wrote.
type RoutingResult struct {
	Routing string        `json:"routing"`
	Files   []ManagedFile `json:"files"`
}
