package blocklist

import (
	"net/netip"
	"strconv"
	"strings"
	"testing"
)

// SPEC §6: a range becomes exactly the prefixes that cover it.
func TestRangePrefixes(t *testing.T) {
	for _, c := range []struct{ lo, hi, want string }{
		{"1.0.0.0", "1.0.0.255", "1.0.0.0/24"},
		{"1.0.0.1", "1.0.0.6", "1.0.0.1/32 1.0.0.2/31 1.0.0.4/31 1.0.0.6/32"},
		{"0.0.0.0", "255.255.255.255", "0.0.0.0/0"},
		{"255.255.255.255", "255.255.255.255", "255.255.255.255/32"},
		{"1.0.1.0", "1.0.3.255", "1.0.1.0/24 1.0.2.0/23"},
		{"2001:db8::", "2001:db8:0:ffff:ffff:ffff:ffff:ffff", "2001:db8::/48"},
		{"::", "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", "::/0"},
		{"2001:db8::1", "2001:db8::2", "2001:db8::1/128 2001:db8::2/128"},
		{"1.0.0.5", "1.0.0.4", ""},
		{"1.0.0.5", "::1", ""},
	} {
		var got []string
		for _, p := range RangePrefixes(netip.MustParseAddr(c.lo), netip.MustParseAddr(c.hi)) {
			got = append(got, p.String())
		}
		if strings.Join(got, " ") != c.want {
			t.Errorf("%s-%s = %v, want %s", c.lo, c.hi, got, c.want)
		}
	}
}

// SPEC §6: the CSV gives each wanted country its prefixes; a malformed row
// or a file that is not the whole database is refused.
func TestCountryPrefixes(t *testing.T) {
	rows := []string{"0.0.0.0,0.255.255.255,ZZ", "1.0.0.0,1.0.0.255,AU", "1.0.1.0,1.0.3.255,CN", "2001:db8::,2001:db8:ffff:ffff:ffff:ffff:ffff:ffff,CN"}
	for i := range 1000 {
		rows = append(rows, "10.0."+itoa(i/256)+"."+itoa(i%256)+",10.0."+itoa(i/256)+"."+itoa(i%256)+",IT")
	}
	got, err := CountryPrefixes(strings.NewReader(strings.Join(rows, "\n")+"\n"), map[string]bool{"CN": true})
	if err != nil {
		t.Fatal(err)
	}
	var s []string
	for _, p := range got["CN"] {
		s = append(s, p.String())
	}
	if strings.Join(s, " ") != "1.0.1.0/24 1.0.2.0/23 2001:db8::/32" || len(got) != 1 {
		t.Fatalf("CN = %v (%d countries)", s, len(got))
	}
	if _, err := CountryPrefixes(strings.NewReader(strings.Join(rows[:4], "\n")), map[string]bool{"CN": true}); err == nil {
		t.Fatal("a file of four rows was taken for the database")
	}
	bad := append([]string{"1.0.0.0,1.0.0.255"}, rows...)
	if _, err := CountryPrefixes(strings.NewReader(strings.Join(bad, "\n")), map[string]bool{"CN": true}); err == nil {
		t.Fatal("a row without a country was accepted")
	}
	worse := append([]string{"1.0.0.9,1.0.0.1,CN"}, rows...)
	if _, err := CountryPrefixes(strings.NewReader(strings.Join(worse, "\n")), map[string]bool{"CN": true}); err == nil {
		t.Fatal("a backwards range was accepted")
	}
	for c, ok := range map[string]bool{"IT": true, "it": false, "ITA": false, "": false, "I1": false} {
		if ValidCountry(c) != ok {
			t.Errorf("ValidCountry(%q) = %v", c, !ok)
		}
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
