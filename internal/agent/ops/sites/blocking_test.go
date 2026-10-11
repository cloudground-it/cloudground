package sites

import (
	"context"
	"net/netip"
	"os"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// #25: a site's rules are written to their own file, with the countries'
// addresses from the country data, named by the vhost; rules taken away
// take the file with them, and so does the site's deletion. Country data
// that cannot be read fails the render and changes nothing.
func TestBlockingFile(t *testing.T) {
	e := newEnv(t)
	var asked []string
	e.CountryPrefixes = func(_ context.Context, codes []string) ([]netip.Prefix, error) {
		asked = codes
		return []netip.Prefix{netip.MustParsePrefix("1.0.1.0/24")}, nil
	}
	s := e.site(41, "guarded.example.com", state.SiteStatic)
	s.Config.Blocking = state.SiteBlocking{Addresses: []string{"203.0.113.0/24"}, UserAgents: []string{"GPTBot"}, Countries: []string{"CN"}}
	e.create(t, s)
	file := e.blockingPath(s)
	b, err := os.ReadFile(file)
	if err != nil || !strings.Contains(string(b), "203.0.113.0/24  1;") || !strings.Contains(string(b), "1.0.1.0/24  1;") || !strings.Contains(string(b), "GPTBot") {
		t.Fatalf("rules file = %q, %v", b, err)
	}
	if strings.Join(asked, ",") != "CN" {
		t.Fatalf("countries asked: %v", asked)
	}
	vhost, _ := os.ReadFile(e.vhostPath(s))
	if !strings.Contains(string(vhost), "if ($cg_block_41) {") {
		t.Fatalf("the vhost does not refuse the blocked:\n%s", vhost)
	}

	// The country data failing: the render fails, the file stays as it was.
	e.CountryPrefixes = func(context.Context, []string) ([]netip.Prefix, error) { return nil, os.ErrNotExist }
	other := s
	other.Config.Blocking.Countries = []string{"RU"}
	if _, err := e.Apply(context.Background(), rpc.SiteRef{Site: other}); err == nil {
		t.Fatal("a render without the country data succeeded")
	}
	if after, _ := os.ReadFile(file); string(after) != string(b) {
		t.Fatal("a failed render changed the rules file")
	}

	s.Config.Blocking = state.SiteBlocking{}
	if _, err := e.Apply(context.Background(), rpc.SiteRef{Site: s}); err != nil {
		t.Fatal(err)
	}
	if exists(file) {
		t.Fatal("the rules file outlived the rules")
	}
	vhost, _ = os.ReadFile(e.vhostPath(s))
	if strings.Contains(string(vhost), "cg_block_") {
		t.Fatal("the vhost still refuses visitors")
	}

	s.Config.Blocking = state.SiteBlocking{UserAgents: []string{"GPTBot"}}
	if _, err := e.Apply(context.Background(), rpc.SiteRef{Site: s}); err != nil || !exists(file) {
		t.Fatalf("rules again: %v", err)
	}
	if _, err := e.Delete(context.Background(), rpc.SiteRef{Site: s}); err != nil {
		t.Fatal(err)
	}
	if exists(file) {
		t.Fatal("the rules file outlived the site")
	}
}
