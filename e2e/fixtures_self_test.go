//go:build e2e

package e2e

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestFixtureMadeOnce checks the fixture registry without a server: the
// sections that declare a fixture share one value, made once even when two
// of them ask at the same time, and the checks are counted per section.
func TestFixtureMadeOnce(t *testing.T) {
	h := &harness{root: t, canary: newCanary(t)}
	var made atomic.Int32
	mk := func(t *testing.T) (any, string) {
		made.Add(1)
		time.Sleep(50 * time.Millisecond)
		h.check(t, "woocommerce provisions", "active", "active")
		return "the site", ""
	}
	var wg sync.WaitGroup
	got := make([]any, 2)
	for i, name := range []string{"woocommerce", "woocommerce security"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t.Run(name, func(t *testing.T) {
				v, ok := h.fixture(t, fxWooSite, mk)
				if !ok {
					t.Errorf("fixture not made")
				}
				got[i] = v
			})
		}()
	}
	wg.Wait()
	if n := made.Load(); n != 1 {
		t.Errorf("made %d times, want once", n)
	}
	if got[0] != "the site" || got[1] != "the site" {
		t.Errorf("values %v", got)
	}
	if !h.fixtureMade(fxWooSite) {
		t.Error("fixtureMade = false after it was made")
	}
	t.Run("woocommerce teardown", func(t *testing.T) { h.dropFixture(t, fxWooSite) })
	if h.fixtureMade(fxWooSite) {
		t.Error("fixtureMade = true after a teardown section dropped it")
	}
	total := 0
	for name, st := range h.stats {
		if st.fail != 0 || (name != "woocommerce" && name != "woocommerce security") {
			t.Errorf("stats %q = %+v", name, *st)
		}
		total += st.pass
	}
	if total != 1 || h.pass != 1 {
		t.Errorf("the provisioning check was counted %d times (tally %d), want once", total, h.pass)
	}
}

// TestSectionTable: every section has a unique test name, and a fixture is
// used by the sections of at most one lane (plus exclusive ones), so two
// lanes never wait on each other for one.
func TestSectionTable(t *testing.T) {
	seen := map[string]bool{}
	lanesOf := map[string]map[string]bool{}
	for _, s := range sections {
		if seen[testName(s.name)] {
			t.Errorf("two sections are %q to go test", testName(s.name))
		}
		seen[testName(s.name)] = true
		if s.run == nil {
			t.Errorf("section %q runs nothing", s.name)
		}
		for _, f := range s.fixtures {
			if lanesOf[f] == nil {
				lanesOf[f] = map[string]bool{}
			}
			if s.lane != "" {
				lanesOf[f][s.lane] = true
			}
		}
	}
	for f, lanes := range lanesOf {
		if len(lanes) > 1 {
			t.Errorf("fixture %q is used from lanes %v", f, lanes)
		}
	}
}
