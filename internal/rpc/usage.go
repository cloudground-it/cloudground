package rpc

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/cloudground-it/cloudground/internal/site"
)

// UsageTarget is one site whose usage is read: its slice is named after
// SystemUser, its directories after Domain.
type UsageTarget struct {
	ID         int64  `json:"id"`
	SystemUser string `json:"system_user"`
	Domain     string `json:"domain"`
	// Disk also walks the site's directories, which costs far more than the
	// counters (api SPEC §11.5).
	Disk bool `json:"disk,omitempty"`
}

// SiteUsageParams asks for the usage of Sites.
type SiteUsageParams struct {
	Sites []UsageTarget `json:"sites"`
}

// maxUsageTargets bounds one call.
const maxUsageTargets = 4096

// Validate checks every target is a site's own: the user is the one its id
// names, the domain a valid domain.
func (p *SiteUsageParams) Validate() error {
	if len(p.Sites) > maxUsageTargets {
		return errors.New("too many sites in one usage read")
	}
	for _, t := range p.Sites {
		id, ok := strings.CutPrefix(t.SystemUser, "cg-site-")
		if !site.ValidSystemUser(t.SystemUser) || !ok || id != strconv.FormatInt(t.ID, 10) {
			return fmt.Errorf("site %d: invalid system user", t.ID)
		}
		if err := site.ValidateDomain(t.Domain); err != nil {
			return fmt.Errorf("site %d: %w", t.ID, err)
		}
	}
	return nil
}

// SiteUsage is one site's counters, read from its slice's cgroup: CPU time
// and IO bytes since the slice started (the reader takes differences), the
// memory and tasks now. A site whose slice is not running has Running
// false and zero counters.
type SiteUsage struct {
	ID      int64 `json:"id"`
	Running bool  `json:"running"`
	CPUUsec int64 `json:"cpu_usec"`
	// MemoryBytes is memory.current; MemoryMaxBytes is memory.max, 0 for
	// none.
	MemoryBytes    int64 `json:"memory_bytes"`
	MemoryMaxBytes int64 `json:"memory_max_bytes"`
	IOReadBytes    int64 `json:"io_read_bytes"`
	IOWriteBytes   int64 `json:"io_write_bytes"`
	Tasks          int64 `json:"tasks"`
	// OOMKills counts the processes the kernel killed at MemoryMax.
	OOMKills int64 `json:"oom_kills"`
	// DiskBytes and DiskInodes are the site's tree and logs, -1 when the
	// disk was not asked for or could not be read.
	DiskBytes  int64 `json:"disk_bytes"`
	DiskInodes int64 `json:"disk_inodes"`
}

// SiteUsageResult is one SiteUsage per target, in order.
type SiteUsageResult struct {
	Sites []SiteUsage `json:"sites"`
}
