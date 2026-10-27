package rpc

import "github.com/cloudground-it/cloudground/internal/cfips"

// CloudflareRangesParams are Cloudflare's ranges as published, the only
// addresses nginx takes CF-Connecting-IP from (#22, ops/sites SPEC §10).
type CloudflareRangesParams struct {
	V4 []string `json:"v4"`
	V6 []string `json:"v6"`
}

// Validate is cfips.Parse (SPEC §3).
func (p *CloudflareRangesParams) Validate() error {
	_, err := cfips.Parse(p.V4, p.V6)
	return err
}

// CloudflareRangesResult says whether the agent's file changed.
type CloudflareRangesResult struct {
	Changed bool `json:"changed"`
}
