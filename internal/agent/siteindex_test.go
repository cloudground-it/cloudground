package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

func siteNo(a *Agent, id int64, domain string) state.Site {
	s := staticSite(a)
	s.ID, s.Domain, s.SystemUser = id, domain, fmt.Sprintf("cg-site-%d", id)
	s.RootPath = filepath.Join(a.Paths.SitesRoot, domain)
	return s
}

func readIndex(t *testing.T, a *Agent) map[string]string {
	t.Helper()
	b, err := os.ReadFile(a.Paths.SiteIndex)
	if err != nil {
		t.Fatal(err)
	}
	var f indexFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f.Sites
}

// A request whose site pairs one site's id (and user) with another site's
// domain is refused before anything runs; so is a user not named after the
// id. The index is written at the creation and forgets a deleted site.
func TestSiteIndexTiesIDToDomain(t *testing.T) {
	a, _ := testAgent(t)
	sa, sb := siteNo(a, 3, "a.example.com"), siteNo(a, 4, "b.example.com")
	for _, s := range []state.Site{sa, sb} {
		if _, err := rpcCall(a, agentrpc.CreateSite, rpc.CreateSiteParams{Site: s}); err != nil {
			t.Fatalf("create %s: %v", s.Domain, err)
		}
	}
	if got := readIndex(t, a); got["3"] != "a.example.com" || got["4"] != "b.example.com" {
		t.Fatalf("index after the creations: %v", got)
	}
	if fi, err := os.Stat(a.Paths.SiteIndex); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("index mode: %v %v", fi, err)
	}
	if _, err := rpcCall(a, agentrpc.CurrentRelease, rpc.CurrentReleaseParams{Site: sa}); err != nil {
		t.Fatalf("control: the true record is served: %v", err)
	}
	mixed := sb
	mixed.Domain, mixed.RootPath = sa.Domain, sa.RootPath // B's id and user, A's tree
	other := sa
	other.SystemUser = "cg-site-4" // A's id and tree, B's user
	unknownID := siteNo(a, 9, sa.Domain)
	for name, s := range map[string]state.Site{"B's id on A's domain": mixed, "B's user on A": other, "a new id on A's domain": unknownID} {
		_, err := rpcCall(a, agentrpc.CurrentRelease, rpc.CurrentReleaseParams{Site: s})
		if agentrpc.CodeOf(err) != agentrpc.CodeInvalid || !strings.Contains(err.Error(), "does not match") {
			t.Errorf("%s: %v, want a refusal", name, err)
		}
		_, err = rpcCall(a, agentrpc.CreateSite, rpc.CreateSiteParams{Site: s})
		if agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
			t.Errorf("create %s: %v, want a refusal", name, err)
		}
	}
	if _, err := rpcCall(a, agentrpc.DeleteSite, rpc.SiteRef{Site: sb}); err != nil {
		t.Fatalf("delete B: %v", err)
	}
	if got := readIndex(t, a); got["4"] != "" || got["3"] != "a.example.com" {
		t.Fatalf("index after B's delete: %v", got)
	}
	// B's domain is free again, for a new id.
	if _, err := rpcCall(a, agentrpc.CreateSite, rpc.CreateSiteParams{Site: siteNo(a, 5, "b.example.com")}); err != nil {
		t.Fatalf("a new site on a deleted site's domain: %v", err)
	}
}

// An agent upgraded from one without the index builds it from the site
// trees at its first start: a domain directory whose group is a site
// user's. Two trees of one user are both left out.
func TestSiteIndexRebuiltFromTrees(t *testing.T) {
	a, _ := testAgent(t)
	gid := os.Getgid()
	for _, d := range []string{"a.example.com", "b.example.com", ".cg-restore-x", "not a domain"} {
		if err := os.Mkdir(filepath.Join(a.Paths.SitesRoot, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	groups := map[int]string{gid: "cg-site-7"}
	if os.Getuid() == 0 {
		// Two trees, two site groups.
		if err := os.Chown(filepath.Join(a.Paths.SitesRoot, "b.example.com"), 0, 4343); err != nil {
			t.Fatal(err)
		}
		groups[4343] = "cg-site-8"
	} else {
		os.Remove(filepath.Join(a.Paths.SitesRoot, "b.example.com"))
	}
	a.LookupGroup = func(g int) (string, error) {
		if n, ok := groups[g]; ok {
			return n, nil
		}
		return "", fmt.Errorf("no group %d", g)
	}
	if err := a.LoadSiteIndex(); err != nil {
		t.Fatal(err)
	}
	got := readIndex(t, a)
	if got["7"] != "a.example.com" || len(got) != len(groups) || (os.Getuid() == 0 && got["8"] != "b.example.com") {
		t.Fatalf("rebuilt index: %v", got)
	}
	_, err := rpcCall(a, agentrpc.CurrentRelease, rpc.CurrentReleaseParams{Site: siteNo(a, 8, "a.example.com")})
	if agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
		t.Errorf("another id on a rebuilt site: %v", err)
	}
}

// Two trees with one site user's group: neither is recorded.
func TestSiteIndexRebuildSkipsAmbiguousTrees(t *testing.T) {
	a, _ := testAgent(t)
	for _, d := range []string{"a.example.com", "b.example.com"} {
		if err := os.Mkdir(filepath.Join(a.Paths.SitesRoot, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	a.LookupGroup = func(int) (string, error) { return "cg-site-7", nil }
	if err := a.LoadSiteIndex(); err != nil {
		t.Fatal(err)
	}
	if got := readIndex(t, a); len(got) != 0 {
		t.Fatalf("ambiguous trees recorded: %v", got)
	}
}

// A creation that fails and leaves no tree leaves the index too.
func TestSiteIndexForgetsAFailedCreation(t *testing.T) {
	a, h := testAgent(t)
	s := siteNo(a, 6, "c.example.com")
	// The user exists already: refused before any tree is made.
	if err := h.accounts.CreateUser(context.Background(), host.UserSpec{Name: s.SystemUser}); err != nil {
		t.Fatal(err)
	}
	if _, err := rpcCall(a, agentrpc.CreateSite, rpc.CreateSiteParams{Site: s}); err == nil {
		t.Fatal("the creation succeeded")
	}
	if _, err := os.Lstat(s.RootPath); !os.IsNotExist(err) {
		t.Fatalf("the failed creation left a tree: %v", err)
	}
	if got := readIndex(t, a); got["6"] != "" {
		t.Fatalf("index after a failed creation: %v", got)
	}
}
