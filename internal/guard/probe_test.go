package guard

import (
	"math"
	"math/rand"
	"testing"
)

func TestValidateProbePaths(t *testing.T) {
	for _, ok := range [][]string{nil, {"/"}, {"/shop/", "/?s=x", "/a%20b"}} {
		if err := ValidateProbePaths(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "shop", "//evil.example/", "/@evil", "/a b", "/a\\b", "/a\nb", "/" + string(make([]byte, 600))} {
		if err := ValidateProbePaths([]string{bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if err := ValidateProbePaths(make([]string, MaxProbePaths+1)); err == nil {
		t.Error("too many paths accepted")
	}
}

func TestRequestURLKeepsTheTargetHost(t *testing.T) {
	cases := map[string]string{
		"/":          "https://127.0.0.1/?cg-guard=n",
		"/shop/?a=b": "https://127.0.0.1/shop/?a=b&cg-guard=n",
	}
	for path, want := range cases {
		got, err := requestURL("https://127.0.0.1/base?q=1", path, "n")
		if err != nil || got != want {
			t.Errorf("%s → %q %v, want %q", path, got, err, want)
		}
	}
	for _, bad := range []string{"//evil/", "http://evil/", "evil"} {
		if _, err := requestURL("https://127.0.0.1", bad, "n"); err == nil {
			t.Errorf("path %q accepted", bad)
		}
	}
}

func TestProbeKeyFor(t *testing.T) {
	// hex(hmac_sha256(key="site-token", "guard-probe")), as PHP's
	// hash_hmac('sha256', 'guard-probe', 'site-token') computes it.
	if got := ProbeKeyFor("site-token"); got != "9a84aa7e8d94a43b7b2d4b6d88bd18ac87d0247bb88ec36ca7a9ff0b8866f1b8" {
		t.Fatalf("probe key %q", got)
	}
	if ProbeKeyFor("") != "" {
		t.Fatal("an empty token must give no key")
	}
}

func TestQuantileAndMedian(t *testing.T) {
	xs := []float64{40, 10, 30, 20}
	if got := median(xs); got != 25 {
		t.Fatalf("median %v", got)
	}
	if xs[0] != 40 {
		t.Fatal("median must not reorder its input")
	}
	if got := quantile([]float64{10, 20, 30, 40}, 0.95); math.Abs(got-38.5) > 1e-9 {
		t.Fatalf("p95 %v", got)
	}
	if quantile(nil, 0.5) != 0 || quantile([]float64{7}, 0.95) != 7 {
		t.Fatal("edge cases")
	}
}

func TestMedianIntervalCoversTheMedian(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	xs := make([]float64, 100)
	for i := range xs {
		xs[i] = rng.NormFloat64()
	}
	lo, hi := medianInterval(xs, rng)
	if m := median(xs); !(lo < m && m < hi) || hi-lo > 1 {
		t.Fatalf("interval [%v, %v] around %v", lo, hi, m)
	}
}

func TestModal(t *testing.T) {
	if modal([]int{0, 0, 200, 404, 200}) != 200 || modal([]int{301, 200}) != 200 || modal(nil) != 0 {
		t.Fatal("modal")
	}
}
