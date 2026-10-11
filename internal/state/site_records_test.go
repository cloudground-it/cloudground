package state

import (
	"errors"
	"testing"
)

func plainSite(domain string) Site {
	return Site{Domain: domain, Type: SiteStatic, Profile: ProfileBalanced, Engine: EngineNginx,
		SystemUser: "cg-site-0", RootPath: "/srv/sites/" + domain, Status: SiteActive}
}

// R-4.3: a taken domain is a typed error, not a driver message to parse.
func TestCreateSiteDomainTaken(t *testing.T) {
	s := testStore(t)
	first, err := s.CreateSite(plainSite("taken.example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if first.DesiredGeneration != 1 || first.AppliedGeneration != 0 || first.CreatedAt.IsZero() {
		t.Fatalf("new row = %+v", first)
	}
	if _, err := s.CreateSite(plainSite("taken.example.com")); !errors.Is(err, ErrDomainTaken) {
		t.Fatalf("second insert = %v, want ErrDomainTaken", err)
	}
}

func TestMutateSiteBookkeeping(t *testing.T) {
	s := testStore(t)
	site, err := s.CreateSite(plainSite("gen.example.com"))
	if err != nil {
		t.Fatal(err)
	}
	mine := errors.New("refused by the caller")
	if _, err := s.MutateSite(site.ID, func(*Site) error { return mine }); err != mine {
		t.Fatalf("fn's error = %v, want it unwrapped", err)
	}
	// Status alone is not configuration; an empty alias list is none.
	got, err := s.MutateSite(site.ID, func(st *Site) error {
		st.Status = SiteError
		st.Aliases = []string{}
		st.ID, st.DesiredGeneration = 99, 42
		return nil
	})
	if err != nil || got.ID != site.ID || got.DesiredGeneration != 1 {
		t.Fatalf("status change = %+v %v", got, err)
	}
	if err := s.MarkApplied(site.ID, 1, nil); err != nil {
		t.Fatal(err)
	}
	got, _ = s.MutateSiteApplied(site.ID, func(st *Site) error { st.Config.WildcardCert = true; return nil })
	if got.DesiredGeneration != 2 || got.AppliedGeneration != 2 {
		t.Fatalf("applied directly = %d/%d", got.AppliedGeneration, got.DesiredGeneration)
	}
	// A report about an older generation is ignored.
	if err := s.MarkApplied(site.ID, 1, errors.New("late")); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetSite(site.ID); got.LastApplyError != "" || got.AppliedGeneration != 2 {
		t.Fatalf("an old report changed the record: %+v", got)
	}
	if _, err := s.MutateSite(12345, func(*Site) error { return nil }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing site = %v", err)
	}
	if err := s.DeleteSite(12345); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing = %v", err)
	}
}
