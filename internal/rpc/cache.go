package rpc

import "github.com/cloudground-it/cloudground/internal/state"

// PurgeParams removes URLs from a site's page cache; no URL at all empties
// the site's whole cache.
type PurgeParams struct {
	Site state.Site `json:"site"`
	URLs []string   `json:"urls"`
}

// Sites lists the site purged.
func (p *PurgeParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// PurgeResult counts the cache entries removed.
type PurgeResult struct {
	Removed int `json:"removed"`
}

// WarmParams fills a site's page cache from its sitemap, at most MaxURLs
// pages (0: the warmer's default).
type WarmParams struct {
	Site    state.Site `json:"site"`
	MaxURLs int        `json:"max_urls"`
}

// Sites lists the site warmed.
func (p *WarmParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// WarmResult counts the pages requested, and those that then answered from
// the cache.
type WarmResult struct {
	Warmed int `json:"warmed"`
	Cached int `json:"cached"`
}

// CacheStatsResult is the object cache's effectiveness. Backend is "apcu",
// "redis" or "none".
type CacheStatsResult struct {
	Backend    string `json:"backend"`
	Hits       int64  `json:"hits"`
	Misses     int64  `json:"misses"`
	Entries    int64  `json:"entries"`
	MemUsed    int64  `json:"mem_used"`
	MemTotal   int64  `json:"mem_total"`
	HitRatePct int    `json:"hit_rate_pct"`
}

// HitsPlusMisses counts the lookups sampled.
func (c CacheStatsResult) HitsPlusMisses() int64 { return c.Hits + c.Misses }
