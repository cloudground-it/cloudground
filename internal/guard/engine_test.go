package guard

import (
	"context"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func contextT() context.Context { return context.Background() }

// SPEC §5: identical code on both sides is blocked in fewer than 5% of 50
// comparisons.
func TestAAComparisonsRarelyBlock(t *testing.T) {
	blocks := 0
	for i := int64(1); i <= 50; i++ {
		f := newFake(1000+i, map[string]side{"prod.test": {medianMs: 300}, "stg.test": {medianMs: 300}})
		if compare(f, i, DefaultThresholds()).Verdict == VerdictBlock {
			blocks++
		}
	}
	if blocks > 0 { // the SPEC bound is < 5%; the shipped policy gives none
		t.Fatalf("identical code blocked %d of 50 times", blocks)
	}
	t.Logf("A/A: %d of 50 blocked", blocks)
}

// SPEC §5: a 30% slowdown is blocked in at least 95% of 50 comparisons.
func TestThirtyPercentRegressionIsBlocked(t *testing.T) {
	caught := 0
	for i := int64(1); i <= 50; i++ {
		f := newFake(2000+i, map[string]side{"prod.test": {medianMs: 300}, "stg.test": {medianMs: 390}})
		if compare(f, i, DefaultThresholds()).Verdict == VerdictBlock {
			caught++
		}
	}
	if caught*100 < 95*50 {
		t.Fatalf("a 30%% regression was blocked only %d of 50 times", caught)
	}
	t.Logf("30%%: %d of 50 blocked", caught)
}

// The same seed gives the same report: the fake target and the engine are
// deterministic, so the reliability numbers above are reproducible.
func TestComparisonIsDeterministic(t *testing.T) {
	run := func() PathComparison {
		f := newFake(7, map[string]side{"prod.test": {medianMs: 300}, "stg.test": {medianMs: 330}})
		return compare(f, 7, DefaultThresholds()).Paths[0]
	}
	if a, b := run(), run(); a != b {
		t.Fatalf("two runs with one seed differ:\n%+v\n%+v", a, b)
	}
}

func TestSlowdownBelowTheAbsoluteFloorPasses(t *testing.T) {
	// 50% slower, but only 10ms: below min_slowdown_ms.
	f := newFake(3, map[string]side{"prod.test": {medianMs: 20}, "stg.test": {medianMs: 30}})
	r := compare(f, 3, DefaultThresholds())
	if r.Verdict != VerdictPass {
		t.Fatalf("a 10ms slowdown got %s: %v", r.Verdict, r.Reasons)
	}
	if pc := r.Paths[0]; pc.CILow <= 1.1 {
		t.Fatalf("the slowdown itself should be measured: %+v", pc)
	}
	// Without the floor the same run blocks.
	th := DefaultThresholds()
	th.MinSlowdownMs = 0
	f = newFake(3, map[string]side{"prod.test": {medianMs: 20}, "stg.test": {medianMs: 30}})
	if r := compare(f, 3, th); r.Verdict != VerdictBlock {
		t.Fatalf("without a floor, a measured 50%% slowdown got %s", r.Verdict)
	}
}

func TestFasterCandidatePasses(t *testing.T) {
	f := newFake(4, map[string]side{"prod.test": {medianMs: 400}, "stg.test": {medianMs: 200}})
	r := compare(f, 4, DefaultThresholds())
	if r.Verdict != VerdictPass || r.Paths[0].CIHigh >= 1 {
		t.Fatalf("a faster candidate: %s %+v %v", r.Verdict, r.Paths, r.Reasons)
	}
}

// A 4xx on the candidate where production answers 200 is an error and a
// status change, never a fast page.
func TestCandidateClientErrorBlocks(t *testing.T) {
	f := newFake(5, map[string]side{"prod.test": {medianMs: 300}, "stg.test": {medianMs: 300, status: 404}})
	r := compare(f, 5, DefaultThresholds())
	if r.Verdict != VerdictBlock {
		t.Fatalf("a candidate answering 404 got %s", r.Verdict)
	}
	if r.Candidate[0].Errors != r.Candidate[0].Requests {
		t.Fatalf("every 404 should count as an error: %+v", r.Candidate[0])
	}
	if !strings.Contains(r.Reasons[0], "HTTP 404 where production answers HTTP 200") {
		t.Fatalf("reason: %v", r.Reasons)
	}
}

// The same 4xx on both sides is the page's answer, not an error.
func TestSameClientErrorOnBothSidesIsNotAnError(t *testing.T) {
	f := newFake(6, map[string]side{"prod.test": {medianMs: 300, status: 410}, "stg.test": {medianMs: 300, status: 410}})
	r := compare(f, 6, DefaultThresholds())
	if r.Candidate[0].Errors != 0 || r.Verdict == VerdictBlock {
		t.Fatalf("a 410 on both sides: %s %+v %v", r.Verdict, r.Candidate[0], r.Reasons)
	}
}

func TestServerErrorsBlock(t *testing.T) {
	f := newFake(8, map[string]side{"prod.test": {medianMs: 300}, "stg.test": {medianMs: 300, status: 503}})
	if r := compare(f, 8, DefaultThresholds()); r.Verdict != VerdictBlock {
		t.Fatalf("a candidate answering 503 got %s", r.Verdict)
	}
}

// Only the PHP path is measured: cache HITs are discarded, and the render
// time comes from the connector, not from the client.
func TestCacheHitsAreDiscarded(t *testing.T) {
	f := newFake(9, map[string]side{"prod.test": {medianMs: 300, hitShare: 0.3}, "stg.test": {medianMs: 300, hitShare: 0.3}})
	r := compare(f, 9, DefaultThresholds())
	pc := r.Paths[0]
	if pc.Pairs >= defaultRounds*7/10 || pc.Pairs < defaultRounds*3/10 {
		t.Fatalf("with 30%% HITs per side about half the rounds should remain, got %d", pc.Pairs)
	}
	if !pc.ServerTimed {
		t.Fatal("the comparison should use the connector's render time")
	}
}

func TestAllCacheHitsIsInconclusive(t *testing.T) {
	f := newFake(10, map[string]side{"prod.test": {medianMs: 300, hitShare: 1}, "stg.test": {medianMs: 300, hitShare: 1}})
	r := compare(f, 10, DefaultThresholds())
	if r.Verdict != VerdictInconclusive || r.Paths[0].Pairs != 0 {
		t.Fatalf("nothing measurable: %s %+v", r.Verdict, r.Paths)
	}
}

// The verdict rests on the server's render time: the fake answers in
// microseconds, so client time would show no slowdown at all.
func TestRenderTimeComesFromTheConnector(t *testing.T) {
	f := newFake(11, map[string]side{"prod.test": {medianMs: 300}, "stg.test": {medianMs: 600}})
	r := compare(f, 11, DefaultThresholds())
	if pc := r.Paths[0]; !pc.ServerTimed || pc.MedianRatio < 1.7 || r.Verdict != VerdictBlock {
		t.Fatalf("a 2× render time: %s %+v", r.Verdict, pc)
	}
	if r.Baseline[0].P50Millis < 200 || r.Baseline[0].P50Millis > 400 {
		t.Fatalf("baseline p50 should be the render time: %+v", r.Baseline[0])
	}
}

// Without the connector on either side, client time is the fallback and
// the comparison says so.
func TestWithoutConnectorClientTimeIsUsed(t *testing.T) {
	f := newFake(12, map[string]side{"prod.test": {medianMs: 300, noFooter: true}, "stg.test": {medianMs: 300, noFooter: true}})
	r := compare(f, 12, DefaultThresholds())
	if pc := r.Paths[0]; pc.ServerTimed || pc.Pairs != defaultRounds {
		t.Fatalf("client-timed comparison: %+v", pc)
	}
}

func TestQueryIncreaseBlocks(t *testing.T) {
	rounds := make([]round, 40)
	for i := range rounds {
		rounds[i] = round{
			base: observation{status: 200, footer: true, serverMs: 100, clientMs: 100, queries: 40},
			cand: observation{status: 200, footer: true, serverMs: 100, clientMs: 100, queries: 90},
		}
	}
	rep := newReport(DefaultThresholds())
	judgeRounds(&rep, "/", rounds, DefaultThresholds(), rand.New(rand.NewSource(1)))
	if rep.Verdict != VerdictBlock || !strings.Contains(strings.Join(rep.Reasons, " "), "queries 40 → 90") {
		t.Fatalf("+125%% queries: %s %v", rep.Verdict, rep.Reasons)
	}
}

func TestEscalateWarnsAtHalfAndBlocksAboveTheLimit(t *testing.T) {
	for _, c := range []struct {
		inc  float64
		want Verdict
	}{{-40, VerdictPass}, {0, VerdictPass}, {10, VerdictPass}, {15, VerdictWarn}, {30, VerdictBlock}} {
		r := newReport(Thresholds{})
		escalate(&r, c.inc, 25, "x")
		if r.Verdict != c.want {
			t.Errorf("escalate(%v, 25) = %s, want %s", c.inc, r.Verdict, c.want)
		}
	}
}

func TestWorstVerdictWins(t *testing.T) {
	r := newReport(Thresholds{})
	r.raise(VerdictBlock, "a")
	r.raise(VerdictWarn, "b")
	r.raise(VerdictInconclusive, "c")
	if r.Verdict != VerdictBlock || len(r.Reasons) != 3 || !strings.HasPrefix(r.Reasons[1], "WARN: ") {
		t.Fatalf("%s %v", r.Verdict, r.Reasons)
	}
}

// SPEC §3: warm-up is at least 2× the workers per side, and §2: every
// request carries the endpoint's headers and a fresh cache-busting nonce.
func TestWarmupAndRequestShape(t *testing.T) {
	f := newFake(13, map[string]side{"prod.test": {medianMs: 300}, "stg.test": {medianMs: 300}})
	e := &Engine{Target: "https://127.0.0.1:8443/ignored?x=1", Client: f.client(), Seed: 1, Workers: 6, Rounds: 30}
	if _, err := e.Compare(contextT(), prod, stg, []string{"/shop/?a=b"}, DefaultThresholds()); err != nil {
		t.Fatal(err)
	}
	if want := 2*12 + 2*30; len(f.seen) != want {
		t.Fatalf("%d requests, want %d (12 warm-up per side for 6 workers, 30 rounds)", len(f.seen), want)
	}
	nonces := map[string]bool{}
	for _, r := range f.seen {
		if r.URL.Host != "127.0.0.1:8443" || r.URL.Path != "/shop/" || r.URL.Query().Get("a") != "b" {
			t.Fatalf("request URL %s", r.URL)
		}
		n := r.URL.Query().Get("cg-guard")
		if n == "" || nonces[n] {
			t.Fatalf("missing or repeated nonce in %s", r.URL)
		}
		nonces[n] = true
		if r.Header.Get("X-CloudGround-Probe") != "key" {
			t.Fatal("probe key not sent")
		}
		if got := r.Header.Get("X-CloudGround-Internal"); (r.Host == "stg.test") != (got == "secret") {
			t.Fatalf("internal key %q sent to %s", got, r.Host)
		}
	}
}

func TestVerify(t *testing.T) {
	baseline := []Sample{{Path: "/", Requests: 100, Status: 200}, {Path: "/gone", Requests: 100, Status: 404}}
	cases := []struct {
		name   string
		status int
		want   Verdict
	}{
		{"healthy", 200, VerdictPass},
		{"now a 500", 500, VerdictBlock},
		{"now a 403", 403, VerdictBlock},
	}
	for _, c := range cases {
		f := newFake(14, map[string]side{"prod.test": {medianMs: 300, status: c.status}})
		e := &Engine{Target: "https://127.0.0.1", Client: f.client(), Seed: 1}
		r, err := e.Verify(contextT(), prod, baseline[:1], []string{"/", "/new"}, DefaultThresholds())
		if err != nil {
			t.Fatal(err)
		}
		if r.Verdict != c.want {
			t.Errorf("%s: %s %v", c.name, r.Verdict, r.Reasons)
		}
		if len(r.Candidate) != 2 || r.Candidate[0].Requests != verifyRequests {
			t.Errorf("%s: live samples %+v", c.name, r.Candidate)
		}
	}
	// A 404 the baseline also had is the page's answer.
	f := newFake(15, map[string]side{"prod.test": {medianMs: 300, status: 404}})
	e := &Engine{Target: "https://127.0.0.1", Client: f.client(), Seed: 1}
	r, _ := e.Verify(contextT(), prod, baseline[1:], []string{"/gone"}, DefaultThresholds())
	if r.Verdict != VerdictPass || r.Candidate[0].Errors != 0 {
		t.Fatalf("a 404 both before and after: %s %+v", r.Verdict, r.Candidate)
	}
}

func TestInvalidTargetIsAnError(t *testing.T) {
	for _, target := range []string{"", "127.0.0.1", "ftp://127.0.0.1", "https://"} {
		e := &Engine{Target: target}
		if _, err := e.Compare(contextT(), prod, stg, []string{"/"}, DefaultThresholds()); err == nil {
			t.Errorf("target %q accepted", target)
		}
	}
}

// The shared client never follows a redirect the site under test chooses,
// and one engine run uses that one client (and its one transport).
func TestSharedClientDoesNotFollowRedirects(t *testing.T) {
	var followed bool
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed = true }))
	defer other.Close()
	site := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/internal", http.StatusFound)
	}))
	defer site.Close()
	o := fetch(contextT(), sharedClient, site.URL, prod, "/", "n")
	if followed || o.status != http.StatusFound {
		t.Fatalf("followed=%v status=%d", followed, o.status)
	}
	r, _ := (&Engine{Target: site.URL}).start()
	if r.client != sharedClient {
		t.Fatal("an engine without an override must use the shared client")
	}
}

func TestFetchReadsTheFooterAndCacheHeader(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-CloudGround-Cache", "hit")
		w.Write([]byte("<p>x</p>\n<!-- cloudground queries=17 time=0.2500 mem=41943040 -->\n"))
	}))
	defer site.Close()
	o := fetch(contextT(), site.Client(), site.URL, prod, "/", "n")
	if !o.footer || o.queries != 17 || math.Abs(o.serverMs-250) > 1e-9 || o.mem != 40<<20 || !o.cached || o.failed {
		t.Fatalf("%+v", o)
	}
}

// A page's own content can print a fake footer; the connector's is the last.
func TestFetchTakesTheLastFooter(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<p><!-- cloudground queries=1 time=0.0010 mem=1 --></p>\n<!-- cloudground queries=30 time=0.4000 mem=2048 -->\n"))
	}))
	defer site.Close()
	o := fetch(contextT(), site.Client(), site.URL, prod, "/", "n")
	if o.queries != 30 || math.Abs(o.serverMs-400) > 1e-9 || o.mem != 2048 {
		t.Fatalf("%+v", o)
	}
}

// Every response the cache served is discarded, not only a HIT.
func TestServedFromCache(t *testing.T) {
	for status, cached := range map[string]bool{
		"": false, "MISS": false, "BYPASS": false, " miss ": false,
		"HIT": true, "hit": true, "STALE": true, "UPDATING": true, "REVALIDATED": true, "EXPIRED": true, "WHATEVER": true,
	} {
		if got := servedFromCache(status); got != cached {
			t.Errorf("%q: cached=%v, want %v", status, got, cached)
		}
	}
}
