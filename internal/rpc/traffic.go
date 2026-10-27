package rpc

import (
	"errors"
	"fmt"
	"time"

	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/traffic"
)

// TrafficTarget is one site whose access log is read (#33): its log is
// <LogRoot>/<Domain>/access.log, its aggregates are kept under ID.
type TrafficTarget struct {
	ID     int64  `json:"id"`
	Domain string `json:"domain"`
}

func (t TrafficTarget) check() error {
	if t.ID <= 0 {
		return errors.New("invalid site id")
	}
	if err := site.ValidateDomain(t.Domain); err != nil {
		return fmt.Errorf("site %d: %w", t.ID, err)
	}
	return nil
}

// TrafficCollectParams are every site the panel has (ops/traffic SPEC §1).
type TrafficCollectParams struct {
	Sites []TrafficTarget `json:"sites"`
}

const maxTrafficTargets = 4096

// Validate checks the list (SPEC §3).
func (p *TrafficCollectParams) Validate() error {
	if len(p.Sites) > maxTrafficTargets {
		return errors.New("too many sites in one traffic read")
	}
	seen := map[int64]bool{}
	for _, t := range p.Sites {
		if err := t.check(); err != nil {
			return err
		}
		if seen[t.ID] {
			return fmt.Errorf("site %d is listed twice", t.ID)
		}
		seen[t.ID] = true
	}
	return nil
}

// TrafficCollected is one site's round.
type TrafficCollected struct {
	ID      int64  `json:"id"`
	Read    int64  `json:"read"`
	Lines   int64  `json:"lines"`
	Skipped int64  `json:"skipped"`
	Error   string `json:"error,omitempty"`
}

// TrafficCollectResult is every site's round.
type TrafficCollectResult struct {
	Sites []TrafficCollected `json:"sites"`
}

// TrafficStatsParams asks for a site's last Days days.
type TrafficStatsParams struct {
	TrafficTarget
	Days int `json:"days"`
}

// Validate checks the site and the range (SPEC §3).
func (p *TrafficStatsParams) Validate() error {
	if err := p.check(); err != nil {
		return err
	}
	if p.Days < 1 || p.Days > 90 {
		return errors.New("days must be 1 to 90")
	}
	return nil
}

// TrafficStatsResult is the range and when the log was last read.
type TrafficStatsResult struct {
	traffic.Stats
	CollectedAt *time.Time `json:"collected_at,omitempty"`
}
