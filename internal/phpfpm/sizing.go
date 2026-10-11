package phpfpm

import (
	"runtime"

	"github.com/cloudground-it/cloudground/internal/state"
)

// CPUCount caps the worker count (SPEC §3.1). A variable so tests can pick
// a machine.
var CPUCount = runtime.NumCPU()

// Sizing constants of SPEC §3.1.
const (
	workerMB      = 80 // resident size of one WordPress worker
	workersPerCPU = 3  // past this the CPU-bound uncached path only slows down
	minWorkers    = 2
	maxWorkers    = 64
	recycleAfter  = 1000 // pm.max_requests
)

// Workers is the process manager's sizing for one pool.
type Workers struct {
	Max         int
	StartServe  int
	MinSpare    int
	MaxSpare    int
	MaxRequests int
}

// ceilDiv is ⌈n/d⌉ for positive d, never below 1.
func ceilDiv(n, d int) int { return max((n+d-1)/d, 1) }

// SizeWorkers sizes a pool from its memory budget, or takes requested when
// it is positive; either way within 2…64 (SPEC §3.1).
func SizeWorkers(budgetMB, requested int) Workers {
	n := requested
	if n <= 0 {
		n = budgetMB / workerMB
		if limit := CPUCount * workersPerCPU; limit > 0 {
			n = min(n, limit)
		}
	}
	n = min(max(n, minWorkers), maxWorkers)
	start := ceilDiv(n, 4)
	return Workers{
		Max:         n,
		StartServe:  start,
		MinSpare:    ceilDiv(n, 8),
		MaxSpare:    max(ceilDiv(n, 2), start),
		MaxRequests: recycleAfter,
	}
}

// SiteCaches is a master's shared-memory sizing: master-wide settings, set
// on its command line (SPEC §3.2).
type SiteCaches struct {
	OPcacheMB  int
	InternedMB int
	MaxFiles   int
	JITMB      int
	APCuMB     int
}

// SizeSiteCaches gives a store twice the code and object cache of any other
// site (SPEC §3.2).
func SizeSiteCaches(t state.SiteType) SiteCaches {
	if t == state.SiteWooCommerce {
		return SiteCaches{OPcacheMB: 128, InternedMB: 16, MaxFiles: 20000, JITMB: 32, APCuMB: 64}
	}
	return SiteCaches{OPcacheMB: 64, InternedMB: 8, MaxFiles: 10000, JITMB: 32, APCuMB: 32}
}
