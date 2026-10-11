//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// Fixtures are the sites more than one section works on. Each is made once
// per run, by the first section that asks for it (whichever sections the run
// selected), and deleted at the end of the run by the safety net every
// created site has (apiClient.createSite), unless a teardown section deleted
// it first. A section may only ask for the fixtures its entry in sections
// declares, so e2e/SECTIONS.md, which lists them, cannot drift from the code
// (TestSectionsDocumented checks the file against the declarations).
//
// A fixture that cannot be made fails every section that asks for it, with
// t.Errorf on the section itself: never as a check subtest, which a
// -test.run filter on check names could hide.
const (
	fxWooSite        = "woocommerce site"
	fxFeatureSite    = "feature site"
	fxFeatureStaging = "feature staging"
)

type fixture struct {
	mu        sync.Mutex // held while the fixture is being made
	made      bool
	val       any
	err       string
	by        string // section that made (or failed to make) it
	droppedBy string
}

type fixtureSet struct {
	mu sync.Mutex
	m  map[string]*fixture
}

func (s *fixtureSet) get(name string) *fixture {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]*fixture{}
	}
	f := s.m[name]
	if f == nil {
		f = &fixture{}
		s.m[name] = f
	}
	return f
}

// sectionOf is the section a (sub)test runs in: the second element of its
// name, as `go test` rewrote it (spaces to underscores).
func sectionOf(t *testing.T) (section, bool) {
	parts := strings.SplitN(t.Name(), "/", 3)
	if len(parts) < 2 {
		return section{}, false
	}
	s, ok := sectionByTestName[parts[1]]
	return s, ok
}

// sectionByTestName indexes sections by testName. It is filled in init:
// the section functions reach sectionOf, so an initialiser referring to
// sections would be a cycle.
var sectionByTestName = map[string]section{}

func init() {
	for _, s := range sections {
		sectionByTestName[testName(s.name)] = s
	}
}

// testName is a name as `go test` shows and matches it.
func testName(s string) string { return strings.Join(strings.Fields(s), "_") }

// fixture returns the named fixture, making it with mk the first time any
// section asks. mk records its own checks on t (the asking section's) and
// returns the value, or a reason it could not make it.
func (h *harness) fixture(t *testing.T, name string, mk func(t *testing.T) (any, string)) (any, bool) {
	t.Helper()
	sec, ok := sectionOf(t)
	if !ok || !sec.uses(name) {
		t.Fatalf("section %q asks for fixture %q without declaring it in sections (e2e_test.go) and e2e/SECTIONS.md", sec.name, name)
	}
	f := h.fixtures.get(name)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case f.droppedBy != "":
		h.fixtureFail(t, name, "deleted earlier in this run by %q", f.droppedBy)
		return nil, false
	case f.made && f.err != "":
		h.fixtureFail(t, name, "%s (when %q made it)", f.err, f.by)
		return nil, false
	case f.made:
		return f.val, true
	}
	start := time.Now()
	t.Logf("fixture %q: making it", name)
	f.val, f.err = mk(t)
	f.made, f.by = true, sec.name
	if f.err != "" {
		h.fixtureFail(t, name, "%s", f.err)
		return nil, false
	}
	t.Logf("fixture %q: ready in %s", name, time.Since(start).Round(time.Second))
	return f.val, true
}

// fixtureMade reports whether the named fixture exists (made and not
// dropped).
func (h *harness) fixtureMade(name string) bool {
	f := h.fixtures.get(name)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.made && f.err == "" && f.droppedBy == ""
}

// dropFixture records that the asking section is deleting the fixture: a
// later section that asks for it fails instead of being handed a deleted site.
func (h *harness) dropFixture(t *testing.T, name string) {
	sec, _ := sectionOf(t)
	f := h.fixtures.get(name)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.droppedBy = sec.name
}

// fixtureFail fails the asking section on the section itself and counts it
// in the RESULT tally.
func (h *harness) fixtureFail(t *testing.T, name, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf("fixture %q: ", name) + fmt.Sprintf(format, args...)
	h.record(t, "fixture: "+name, false)
	t.Error(msg)
}

// ------------------------------------------------------- the fixtures --

// wooFx is the WooCommerce site: made with the commerce profile and the
// object cache, and kept until the "woocommerce teardown" section.
type wooFx struct {
	id         int64
	dom, user  string
	root, path string // site root, current release
}

func (h *harness) wooSite(t *testing.T) (*wooFx, bool) {
	t.Helper()
	v, ok := h.fixture(t, fxWooSite, func(t *testing.T) (any, string) {
		woo := h.dom("shop")
		wid := h.api.createSite(h.wpSiteJSON(woo, "woocommerce", `,"profile":"commerce","object_cache":true`))
		if wid == 0 {
			return nil, "create rejected"
		}
		st := h.api.waitActive(wid, 180*time.Second)
		h.check(t, "woocommerce provisions", st, "active")
		if st != "active" {
			return nil, "did not provision: " + st
		}
		return &wooFx{id: wid, dom: woo, user: h.api.site(wid).SystemUser,
			root: "/srv/sites/" + woo, path: "/srv/sites/" + woo + "/current"}, ""
	})
	if !ok {
		return nil, false
	}
	return v.(*wooFx), true
}

// featureSite is the WordPress site the "wordpress …" sections share. It is
// created plain (no object cache); each section that needs a setting puts it
// in place itself (feat.ensureObjectCache).
func (h *harness) featureSite(t *testing.T) (*feat, bool) {
	t.Helper()
	v, ok := h.fixture(t, fxFeatureSite, func(t *testing.T) (any, string) {
		fs := h.dom("feat")
		fid := h.api.createSite(h.wpSiteJSON(fs, "wordpress", ""))
		if fid == 0 {
			return nil, "create rejected"
		}
		st := h.api.waitActive(fid, 120*time.Second)
		h.check(t, "feature-site provisions", st, "active")
		if st != "active" {
			return nil, "did not provision: " + st
		}
		return &feat{h: h, fs: fs, fid: fid, fp: "/srv/sites/" + fs + "/current", fsu: h.api.site(fid).SystemUser}, ""
	})
	if !ok {
		return nil, false
	}
	return v.(*feat), true
}

// featureStaging is the feature site's staging copy, cloned once. The
// "wordpress staging" section makes it (with the layout probes around the
// clone); any other section that needs it makes it on demand. It returns the
// staging site as it is now: a staging recreate replaces it.
func (h *harness) featureStaging(t *testing.T) (*feat, siteInfo, bool) {
	t.Helper()
	f, ok := h.featureSite(t)
	if !ok {
		return nil, siteInfo{}, false
	}
	if _, ok := h.fixture(t, fxFeatureStaging, func(t *testing.T) (any, string) {
		_, b := h.api.post(f.api("/staging"), `{}`)
		stgtask := taskID(b)
		h.assert(t, "staging clone accepted", stgtask != 0, "staging clone accepted")
		// W3: mid-clone the copy's tree is partial; the diff listed most of
		// production as removed. It is refused until the clone is done.
		c, db := h.api.get(f.api("/staging/diff"))
		h.assert(t, "W3: no staging diff while the clone runs", c == 409, "HTTP %d: %s", c, head(db, 300))
		st := h.api.waitTask(stgtask, 300*time.Second)
		h.check(t, "staging clone completes", st, "succeeded")
		if h.api.stagingOf(f.fid).ID == 0 {
			return nil, "no staging site after the clone (task " + st + ")"
		}
		return true, ""
	}); !ok {
		return nil, siteInfo{}, false
	}
	stg := h.api.stagingOf(f.fid)
	if stg.ID == 0 {
		h.fixtureFail(t, fxFeatureStaging, "the feature site has no staging site any more")
		return nil, siteInfo{}, false
	}
	return f, stg, true
}

// withFeature runs fn on the feature site, or fails the section when the
// fixture could not be made.
func (h *harness) withFeature(t *testing.T, fn func(f *feat)) {
	t.Helper()
	if f, ok := h.featureSite(t); ok {
		fn(f)
	}
}

// ensureObjectCache turns the feature site's object cache on when it is not
// (stored off, or its drop-in missing), as a control check named after what
// needs it. In a full run the earlier sections have left it on and this does
// nothing; a section run on its own gets the state the full run gives it.
func (f *feat) ensureObjectCache(t *testing.T, what string) {
	if f.h.api.site(f.fid).Config.ObjectCache && exists(f.fp+"/wp-content/object-cache.php") {
		return
	}
	f.objectCacheSet(t, true, "control: the object cache is turned on before "+what)
}
