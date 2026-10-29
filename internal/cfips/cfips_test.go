package cfips

import (
	"fmt"
	"net/netip"
	"strings"
	"testing"
)

func TestShippedPasses(t *testing.T) {
	r := Shipped()
	if len(r.V4) != 15 || len(r.V6) != 7 {
		t.Fatalf("shipped list: %d IPv4, %d IPv6", len(r.V4), len(r.V6))
	}
}

func TestParseRefuses(t *testing.T) {
	for _, c := range []struct {
		v4, v6 []string
		want   string
	}{
		{nil, []string{"2400:cb00::/32"}, "1 to 100 networks"},
		{[]string{"104.16.0.1"}, nil, "not a network in CIDR form"},
		{[]string{"junk"}, nil, "not a network in CIDR form"},
		{[]string{"2400:cb00::/32"}, nil, "wrong list"},
		{[]string{"104.16.0.0/13"}, []string{"104.24.0.0/14"}, "wrong list"},
		{[]string{"104.16.0.1/13"}, nil, "not a network address"},
		{[]string{"127.0.0.0/8"}, nil, "not a public network"},
		{[]string{"10.0.0.0/8"}, nil, "not a public network"},
		{[]string{"104.16.0.0/13"}, []string{"fe80::/64"}, "not a public network"},
		{[]string{"0.0.0.0/0"}, nil, "not a public network"},
		{[]string{"64.0.0.0/2"}, nil, "too wide"},
		{[]string{"104.16.0.0/13"}, []string{"2000::/3"}, "too wide"},
		// H-7-2: no wider than Cloudflare's widest, with a margin: /12, /29.
		{[]string{"104.0.0.0/11"}, nil, "too wide"},
		{[]string{"104.16.0.0/13"}, []string{"2a06:9800::/28"}, "too wide"},
	} {
		_, err := Parse(c.v4, c.v6)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Parse(%v, %v) = %v, want %q", c.v4, c.v6, err, c.want)
		}
	}
	many := make([]string, 101)
	for i := range many {
		many[i] = "104.16.0.0/13"
	}
	if _, err := Parse(many, nil); err == nil {
		t.Error("101 networks taken")
	}
}

func TestParseSortsAndContains(t *testing.T) {
	r, err := Parse([]string{" 172.64.0.0/13", "104.16.0.0/13", "104.16.0.0/13"}, []string{"2606:4700::/32"})
	if err != nil {
		t.Fatal(err)
	}
	v4, v6 := r.Strings()
	if strings.Join(v4, ",") != "104.16.0.0/13,172.64.0.0/13" || strings.Join(v6, ",") != "2606:4700::/32" {
		t.Fatalf("sorted: %v %v", v4, v6)
	}
	for a, want := range map[string]bool{
		"104.17.1.1": true, "::ffff:104.17.1.1": true, "2606:4700::1": true,
		"203.0.113.7": false, "127.0.0.1": false,
	} {
		if got := r.Contains(netip.MustParseAddr(a)); got != want {
			t.Errorf("Contains(%s) = %v", a, got)
		}
	}
	if p := r.Prefixes(); len(p) != 3 || !p[0].Addr().Is4() {
		t.Fatalf("Prefixes: %v", p)
	}
}

// The widest networks Cloudflare has, and a /12 and a /29, are taken; Diff
// names what a new list adds and drops.
func TestWidestAndDiff(t *testing.T) {
	a, err := Parse([]string{"104.16.0.0/12", "173.245.48.0/20"}, []string{"2a06:98c0::/29"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Parse([]string{"173.245.48.0/20", "131.0.72.0/22"}, []string{"2a06:98c0::/29"})
	if err != nil {
		t.Fatal(err)
	}
	added, removed := b.Diff(a)
	if fmt.Sprint(added) != "[131.0.72.0/22]" || fmt.Sprint(removed) != "[104.16.0.0/12]" {
		t.Errorf("Diff: added %v removed %v", added, removed)
	}
	if added, removed := a.Diff(a); len(added)+len(removed) != 0 {
		t.Errorf("a list differs from itself: %v %v", added, removed)
	}
}
