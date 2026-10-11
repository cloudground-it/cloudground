package guard

import (
	"context"
	"math"
	"math/rand"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Engine is Performance Guard's one measurement engine (SPEC.md).
type Engine struct {
	// Target is the scheme and authority requests go to, e.g. https://127.0.0.1.
	Target string
	// Rounds is the number of paired rounds per path (default 100).
	Rounds int
	// Workers is the PHP-FPM worker count of the sites measured; it sizes
	// the warm-up (default 1).
	Workers int
	// Seed drives round order, nonces and the bootstrap; 0 = time-based.
	Seed int64
	// Client replaces the shared client (tests only).
	Client *http.Client
}

const (
	defaultRounds = 200
	// verifyRequests is how many requests the post-promotion check sends
	// per path.
	verifyRequests = 20
	minWarmup      = 8
	// minPairs is the fewest measurable rounds a path is judged on.
	minPairs = 20
	// maxStatusMismatchPct of rounds answering differently is a status change.
	maxStatusMismatchPct = 10.0
	// noisyIntervalLog: an interval wider than 3× cannot tell anything apart.
	noisyIntervalLog = 1.0986122886681098 // ln 3
	minQueryDelta    = 5.0
	minMemDelta      = 32 << 20
)

// run holds one call's settings and randomness.
type run struct {
	target  string
	client  *http.Client
	workers int
	rng     *rand.Rand
}

func (e *Engine) start() (*run, error) {
	if _, err := requestURL(e.Target, "/", ""); err != nil {
		return nil, err
	}
	seed := e.Seed
	if seed == 0 {
		seed = time.Now().UnixNano()
	}
	r := &run{target: e.Target, client: e.Client, workers: max(1, e.Workers), rng: rand.New(rand.NewSource(seed))}
	if r.client == nil {
		r.client = sharedClient
	}
	return r, nil
}

func (r *run) nonce() string { return strconv.FormatInt(r.rng.Int63(), 36) }

func (r *run) get(ctx context.Context, ep Endpoint, path string) observation {
	return fetch(ctx, r.client, r.target, ep, path, r.nonce())
}

// warm sends max(8, 2×workers) discarded requests per side, workers at a
// time, alternating sides (SPEC.md §3).
func (r *run) warm(ctx context.Context, path string, sides ...Endpoint) {
	per := max(minWarmup, 2*r.workers)
	type job struct {
		ep    Endpoint
		nonce string
	}
	jobs := make([]job, 0, per*len(sides))
	for i := 0; i < per; i++ {
		for _, ep := range sides {
			jobs = append(jobs, job{ep, r.nonce()}) // nonces drawn here: the goroutines never touch rng
		}
	}
	slots := make(chan struct{}, r.workers)
	var wg sync.WaitGroup
	for _, j := range jobs {
		slots <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			fetch(ctx, r.client, r.target, j.ep, path, j.nonce)
		}()
	}
	wg.Wait()
}

// round is one paired measurement.
type round struct{ base, cand observation }

// Compare measures candidate against baseline on every path (SPEC.md §4).
func (e *Engine) Compare(ctx context.Context, baseline, candidate Endpoint, paths []string, t Thresholds) (Report, error) {
	r, err := e.start()
	if err != nil {
		return Report{}, err
	}
	n := e.Rounds
	if n <= 0 {
		n = defaultRounds
	}
	rep := newReport(t)
	for _, path := range paths {
		r.warm(ctx, path, baseline, candidate)
		rounds := make([]round, n)
		for i := range rounds {
			if err := ctx.Err(); err != nil {
				return rep, err
			}
			if r.rng.Intn(2) == 0 {
				rounds[i].base = r.get(ctx, baseline, path)
				rounds[i].cand = r.get(ctx, candidate, path)
			} else {
				rounds[i].cand = r.get(ctx, candidate, path)
				rounds[i].base = r.get(ctx, baseline, path)
			}
		}
		bs, cs, pc := judgeRounds(&rep, path, rounds, t, r.rng)
		rep.Baseline = append(rep.Baseline, bs)
		rep.Candidate = append(rep.Candidate, cs)
		if pc != nil {
			rep.Paths = append(rep.Paths, *pc)
		}
	}
	return rep, nil
}

// Verify checks production after a promotion against the baseline the
// comparison measured (SPEC.md §6).
func (e *Engine) Verify(ctx context.Context, production Endpoint, baseline []Sample, paths []string, t Thresholds) (Report, error) {
	r, err := e.start()
	if err != nil {
		return Report{}, err
	}
	rep := newReport(t)
	rep.Baseline = baseline
	before := map[string]Sample{}
	for _, s := range baseline {
		before[s.Path] = s
	}
	for _, path := range paths {
		r.warm(ctx, path, production)
		obs := make([]observation, verifyRequests)
		for i := range obs {
			if err := ctx.Err(); err != nil {
				return rep, err
			}
			obs[i] = r.get(ctx, production, path)
		}
		b, known := before[path]
		errs := make([]bool, len(obs))
		for i, o := range obs {
			errs[i] = o.failed || (known && isClientError(o.status) && o.status != b.Status)
		}
		live := summarise(path, obs, errs, anyFooter(obs))
		rep.Candidate = append(rep.Candidate, live)
		if known {
			judgeLive(&rep, b, live, t)
		}
	}
	return rep, nil
}

func judgeLive(rep *Report, b, l Sample, t Thresholds) {
	if b.Status != 0 && l.Status != 0 && b.Status != l.Status {
		rep.raise(VerdictBlock, "%s: production now answers HTTP %d (was HTTP %d)", l.Path, l.Status, b.Status)
		return
	}
	le, be := share(l.Errors, l.Requests), share(b.Errors, b.Requests)
	if t.MaxErrorRatePct > 0 && le > be && le > t.MaxErrorRatePct {
		rep.raise(VerdictBlock, "%s: error rate %.1f%% after promotion (baseline %.1f%%)", l.Path, le, be)
	}
}

func isClientError(status int) bool { return status >= 400 && status < 500 }

func anyFooter(obs []observation) bool {
	for _, o := range obs {
		if o.footer {
			return true
		}
	}
	return false
}

// judgeRounds applies SPEC.md §4.2–4.4 to one path.
func judgeRounds(rep *Report, path string, rounds []round, t Thresholds, rng *rand.Rand) (Sample, Sample, *PathComparison) {
	base := make([]observation, len(rounds))
	cand := make([]observation, len(rounds))
	baseErr := make([]bool, len(rounds))
	candErr := make([]bool, len(rounds))
	serverTimed := false
	answered, differ := 0, 0
	for i, rd := range rounds {
		base[i], cand[i] = rd.base, rd.cand
		baseErr[i] = rd.base.failed
		candErr[i] = rd.cand.failed || (isClientError(rd.cand.status) && rd.cand.status != rd.base.status)
		if rd.base.footer && rd.cand.footer {
			serverTimed = true
		}
		if rd.base.status != 0 && rd.cand.status != 0 {
			answered++
			if rd.base.status != rd.cand.status {
				differ++
			}
		}
	}
	bs := summarise(path, base, baseErr, serverTimed)
	cs := summarise(path, cand, candErr, serverTimed)

	if answered > 0 && share(differ, answered) > maxStatusMismatchPct {
		rep.raise(VerdictBlock, "%s: candidate answers HTTP %d where production answers HTTP %d", path, cs.Status, bs.Status)
		return bs, cs, nil
	}
	if ce, be := share(cs.Errors, cs.Requests), share(bs.Errors, bs.Requests); t.MaxErrorRatePct > 0 && ce > be && ce > t.MaxErrorRatePct {
		rep.raise(VerdictBlock, "%s: error rate %.1f%% (baseline %.1f%%)", path, ce, be)
		return bs, cs, nil
	}

	pc := &PathComparison{Path: path, ServerTimed: serverTimed}
	var logs, deltas []float64
	for i := range rounds {
		b, c := base[i], cand[i]
		if baseErr[i] || candErr[i] || b.cached || c.cached {
			continue
		}
		bt, ct := timeOf(b, serverTimed), timeOf(c, serverTimed)
		if bt <= 0 || ct <= 0 {
			continue
		}
		logs = append(logs, math.Log(ct/bt))
		deltas = append(deltas, ct-bt)
	}
	pc.Pairs = len(logs)
	if t.MaxSlowdownPct > 0 {
		judgeTiming(rep, pc, logs, deltas, t, rng)
	}
	judgeResources(rep, path, bs, cs, t)
	return bs, cs, pc
}

// timeOf is the time a pair is judged on: the connector's render time when
// the path is server-timed, the client's otherwise.
func timeOf(o observation, serverTimed bool) float64 {
	if serverTimed {
		if !o.footer {
			return 0
		}
		return o.serverMs
	}
	return o.clientMs
}

func judgeTiming(rep *Report, pc *PathComparison, logs, deltas []float64, t Thresholds, rng *rand.Rand) {
	if len(logs) < minPairs {
		rep.raise(VerdictInconclusive, "%s: only %d measurable paired requests (cache hits, errors or no connector timing)", pc.Path, len(logs))
		return
	}
	med := median(logs)
	lo, hi := medianInterval(logs, rng)
	pc.MedianRatio, pc.CILow, pc.CIHigh = math.Exp(med), math.Exp(lo), math.Exp(hi)
	pc.MedianDeltaMs = median(deltas)
	limit := math.Log(1 + t.MaxSlowdownPct/100)
	what := "time"
	if pc.ServerTimed {
		what = "render time"
	}
	desc := func() []any {
		return []any{pc.Path, what, pc.MedianRatio * 100, pc.CILow * 100, pc.CIHigh * 100, pc.MedianDeltaMs, pc.Pairs}
	}
	const format = "%s: candidate %s %.0f%% of production (95%% CI %.0f–%.0f%%, median %+.0fms over %d pairs)"
	switch {
	case pc.MedianDeltaMs < t.MinSlowdownMs:
	case lo > limit:
		rep.raise(VerdictBlock, format, desc()...)
	case hi-lo > noisyIntervalLog && med > limit:
		rep.raise(VerdictInconclusive, format, desc()...)
	case lo > 0 && med > limit:
		rep.raise(VerdictWarn, format, desc()...)
	}
}

func judgeResources(rep *Report, path string, bs, cs Sample, t Thresholds) {
	if t.MaxQueryIncreasePct > 0 && bs.AvgQueries > 0 && cs.AvgQueries-bs.AvgQueries >= minQueryDelta {
		inc := 100 * (cs.AvgQueries - bs.AvgQueries) / bs.AvgQueries
		escalate(rep, inc, t.MaxQueryIncreasePct, "%s: queries %.0f → %.0f (+%.0f%%)", path, bs.AvgQueries, cs.AvgQueries, inc)
	}
	if t.MaxMemIncreasePct > 0 && bs.PeakMemBytes > 0 && cs.PeakMemBytes-bs.PeakMemBytes >= minMemDelta {
		inc := 100 * float64(cs.PeakMemBytes-bs.PeakMemBytes) / float64(bs.PeakMemBytes)
		escalate(rep, inc, t.MaxMemIncreasePct, "%s: peak memory %dMB → %dMB (+%.0f%%)", path, bs.PeakMemBytes>>20, cs.PeakMemBytes>>20, inc)
	}
}

// escalate blocks an increase above limit and warns above half of it.
func escalate(rep *Report, inc, limit float64, format string, args ...any) {
	switch {
	case inc > limit:
		rep.raise(VerdictBlock, format, args...)
	case inc > limit/2:
		rep.raise(VerdictWarn, format, args...)
	}
}

// summarise is one side's Sample over a path's observations.
func summarise(path string, obs []observation, errs []bool, serverTimed bool) Sample {
	s := Sample{Path: path, Requests: len(obs)}
	var times, queries []float64
	statuses := make([]int, 0, len(obs))
	for i, o := range obs {
		statuses = append(statuses, o.status)
		if errs[i] {
			s.Errors++
			continue
		}
		if tm := timeOf(o, serverTimed); tm > 0 && !o.cached {
			times = append(times, tm)
		}
		if o.footer {
			queries = append(queries, o.queries)
			s.PeakMemBytes = max(s.PeakMemBytes, o.mem)
		}
	}
	s.Status = modal(statuses)
	s.P50Millis = quantile(times, 0.5)
	s.P95Millis = quantile(times, 0.95)
	s.AvgQueries = median(queries)
	return s
}
