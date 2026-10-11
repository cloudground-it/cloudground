package guard

import (
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"
)

// side is how one virtual host of the fake target behaves.
type side struct {
	medianMs float64
	status   int     // 0 = 200
	hitShare float64 // share of responses marked as cache HITs
	noFooter bool    // a site without the connector
}

// fakeTarget answers in-process, without a network, like a WordPress
// server: render times drawn from a log-normal (σ 0.25) with a 3× spike on
// 5% of requests, reported in the connector footer for the right probe key.
// Requests are measured one at a time, so a seed gives the same run every
// time.
type fakeTarget struct {
	mu    sync.Mutex
	rng   *rand.Rand
	sides map[string]side
	seen  []*http.Request
}

func newFake(seed int64, sides map[string]side) *fakeTarget {
	return &fakeTarget{rng: rand.New(rand.NewSource(seed)), sides: sides}
}

func (f *fakeTarget) client() *http.Client {
	return &http.Client{Transport: f, CheckRedirect: sharedClient.CheckRedirect}
}

func (f *fakeTarget) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	sd := f.sides[r.Host]
	ms := sd.medianMs * math.Exp(f.rng.NormFloat64()*0.25)
	if f.rng.Float64() < 0.05 {
		ms *= 3
	}
	hit := f.rng.Float64() < sd.hitShare
	f.seen = append(f.seen, r)
	f.mu.Unlock()

	if sd.noFooter {
		// Without the connector only the client's clock can tell: take a
		// hundredth of the render time for real.
		time.Sleep(time.Duration(ms*10) * time.Microsecond)
	}
	status := sd.status
	if status == 0 {
		status = 200
	}
	body := "<html>page</html>"
	if r.Header.Get("X-CloudGround-Probe") == "key" && !sd.noFooter && status == 200 {
		body += fmt.Sprintf("\n<!-- cloudground queries=42 time=%.4f mem=%d -->\n", ms/1000, 40<<20)
	}
	h := http.Header{}
	if hit {
		h.Set("X-CloudGround-Cache", "HIT")
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

var (
	prod = Endpoint{Host: "prod.test", ProbeKey: "key"}
	stg  = Endpoint{Host: "stg.test", ProbeKey: "key", InternalKey: "secret"}
)

func compare(f *fakeTarget, seed int64, t Thresholds, paths ...string) Report {
	if len(paths) == 0 {
		paths = []string{"/shop/"}
	}
	e := &Engine{Target: "https://127.0.0.1", Client: f.client(), Seed: seed, Workers: 2}
	r, err := e.Compare(contextT(), prod, stg, paths, t)
	if err != nil {
		panic(err)
	}
	return r
}
