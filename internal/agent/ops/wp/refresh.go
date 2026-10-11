package wp

import (
	"context"
	"errors"

	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Refresh makes an out-of-band change to a site's database or files (wp-cli,
// an import, a restore, an update) visible to the live site, touching only
// that site (SPEC §7).
//
// OPcache holds the old bytecode; a reload clears it. APCu holds WordPress's
// object cache, and neither a reload (the master and its shared memory
// survive) nor wp-cli (its own segment) can clear it directly. The drop-in
// keys every entry with a generation read from a file both processes see,
// so a flush from wp-cli writes a new one and PHP-FPM's next request reads
// none of the old entries (connector/SPEC.md §3.4). When that fails,
// restarting the site's master clears both, for this site alone.
func (o *Ops) Refresh(ctx context.Context, s state.Site) error {
	if s.PHPVersion == "" {
		return nil
	}
	if isWordPress(s.Type) {
		// Core only: a plugin or theme that breaks wp-cli must not stop the
		// flush; the drop-in is loaded either way.
		_, err := o.Run(ctx, s, "cache", "flush", "--skip-plugins", "--skip-themes")
		if err == nil {
			return o.PHP.Reload(ctx, s)
		}
		o.log().Warn("object-cache flush failed; restarting the site's PHP", "site", s.Domain, "err", err)
	}
	return o.PHP.Restart(ctx, s)
}

// CacheStats reports the object cache's numbers (SPEC §8).
func (o *Ops) CacheStats(ctx context.Context, p rpc.WPParams) (rpc.CacheStatsResult, error) {
	res := rpc.CacheStatsResult{Backend: "none"}
	on, err := o.dropinLinked(p.Site)
	if err != nil || !on {
		return res, err
	}
	res.Backend = "apcu"
	var st struct {
		Hits     int64  `json:"hits"`
		Misses   int64  `json:"misses"`
		Entries  int64  `json:"entries"`
		MemUsed  int64  `json:"mem_used"`
		MemTotal int64  `json:"mem_total"`
		Error    string `json:"error"`
	}
	if err := o.callConnector(ctx, p.Site, "cloudground_stats", "X-CloudGround-Token", &st); err != nil {
		return rpc.CacheStatsResult{}, err
	}
	if st.Error != "" {
		return rpc.CacheStatsResult{}, errors.New("object-cache statistics: " + st.Error)
	}
	res.Hits, res.Misses, res.Entries, res.MemUsed, res.MemTotal = st.Hits, st.Misses, st.Entries, st.MemUsed, st.MemTotal
	if n := res.HitsPlusMisses(); n > 0 {
		res.HitRatePct = int(res.Hits * 100 / n)
	}
	return res, nil
}
