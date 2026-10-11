// Package guard is Performance Guard, the gate of a staging safe-push: it
// measures staging against production on the same server, refuses a
// candidate that is slower, heavier or broken, and checks production again
// after the promotion. The behaviour is specified in SPEC.md.
package guard

import (
	"fmt"
	"time"
)

// Thresholds is the policy a comparison is judged by (SPEC.md §5). A zero
// field turns its check off.
type Thresholds struct {
	// MaxSlowdownPct blocks when the lower bound of the 95% interval of the
	// median slowdown is above it.
	MaxSlowdownPct float64 `json:"max_slowdown_pct"`
	// MinSlowdownMs ignores a slowdown whose median per-round difference is
	// smaller, whatever its ratio.
	MinSlowdownMs       float64 `json:"min_slowdown_ms"`
	MaxQueryIncreasePct float64 `json:"max_query_increase_pct"`
	MaxMemIncreasePct   float64 `json:"max_mem_increase_pct"`
	MaxErrorRatePct     float64 `json:"max_error_rate_pct"`
}

// DefaultThresholds is the shipped policy.
func DefaultThresholds() Thresholds {
	return Thresholds{
		MaxSlowdownPct:      10,
		MinSlowdownMs:       25,
		MaxQueryIncreasePct: 50,
		MaxMemIncreasePct:   30,
		MaxErrorRatePct:     1,
	}
}

// Verdict is the gate's decision.
type Verdict string

const (
	VerdictPass Verdict = "pass"
	VerdictWarn Verdict = "warn"
	// VerdictInconclusive: the measurement cannot tell the candidate from
	// the noise. Not a regression, and never reported as one.
	VerdictInconclusive Verdict = "inconclusive"
	VerdictBlock        Verdict = "block"
)

// severity orders verdicts so a report keeps the worst one.
func (v Verdict) severity() int {
	switch v {
	case VerdictWarn:
		return 1
	case VerdictInconclusive:
		return 2
	case VerdictBlock:
		return 3
	}
	return 0
}

// Sample summarises one side's requests to one path.
type Sample struct {
	Path         string  `json:"path"`
	Requests     int     `json:"requests"`
	Errors       int     `json:"errors"`
	P50Millis    float64 `json:"p50_ms"`
	P95Millis    float64 `json:"p95_ms"`
	AvgQueries   float64 `json:"avg_queries"`
	PeakMemBytes int64   `json:"peak_mem_bytes"`
	// Status is the most common HTTP status.
	Status int `json:"status,omitempty"`
}

// PathComparison is the paired statistics for one path (SPEC.md §4.3).
type PathComparison struct {
	Path          string  `json:"path"`
	MedianRatio   float64 `json:"median_ratio"`
	CILow         float64 `json:"ci_low"`
	CIHigh        float64 `json:"ci_high"`
	MedianDeltaMs float64 `json:"median_delta_ms"`
	Pairs         int     `json:"pairs"`
	ServerTimed   bool    `json:"server_timed"`
}

// Report is a comparison's or a check's outcome, stored on the deployment.
type Report struct {
	Verdict    Verdict          `json:"verdict"`
	Reasons    []string         `json:"reasons,omitempty"`
	Baseline   []Sample         `json:"baseline"`
	Candidate  []Sample         `json:"candidate"`
	Paths      []PathComparison `json:"paths,omitempty"`
	Thresholds Thresholds       `json:"thresholds"`
	MeasuredAt time.Time        `json:"measured_at"`
}

func newReport(t Thresholds) Report {
	return Report{Verdict: VerdictPass, Thresholds: t, MeasuredAt: time.Now().UTC()}
}

// raise records a finding and keeps the worse of the two verdicts.
func (r *Report) raise(v Verdict, format string, args ...any) {
	if v.severity() > r.Verdict.severity() {
		r.Verdict = v
	}
	prefix := map[Verdict]string{VerdictWarn: "WARN: ", VerdictInconclusive: "INCONCLUSIVE: ", VerdictBlock: "BLOCK: "}[v]
	r.Reasons = append(r.Reasons, prefix+fmt.Sprintf(format, args...))
}
