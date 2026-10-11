package blocklist

import (
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"
)

// SPEC §1: entries are masked and unmapped, and a block is not wider than
// /8 or /16.
func TestParsePrefix(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.9":             "203.0.113.9/32",
		" 203.0.113.9 ":           "203.0.113.9/32",
		"203.0.113.9/24":          "203.0.113.0/24",
		"::ffff:198.51.100.7":     "198.51.100.7/32",
		"::ffff:198.51.100.0/120": "198.51.100.0/24",
		"2001:db8::1":             "2001:db8::1/128",
		"2001:db8:1:2:3::/48":     "2001:db8:1::/48",
	} {
		p, err := ParsePrefix(in)
		if err != nil || p.String() != want {
			t.Errorf("ParsePrefix(%q) = %v, %v; want %s", in, p, err, want)
		}
	}
	for _, in := range []string{"", "x", "1.2.3", "1.2.3.4/33", "fe80::1%eth0", "1.2.3.4/x", "::ffff:1.2.3.4/90"} {
		if p, err := ParsePrefix(in); err == nil {
			t.Errorf("ParsePrefix(%q) = %v", in, p)
		}
	}
	for in, ok := range map[string]bool{"10.0.0.0/8": true, "10.0.0.0/7": false, "0.0.0.0/0": false, "2001:db8::/16": true, "2001::/15": false, "::/0": false} {
		if err := CheckBlockWidth(netip.MustParsePrefix(in)); (err == nil) != ok {
			t.Errorf("CheckBlockWidth(%s) = %v", in, err)
		}
	}
}

func TestUnitAndExempt(t *testing.T) {
	if u := Unit(netip.MustParseAddr("2001:db8:1:2:aaaa::9")); u.String() != "2001:db8:1:2::/64" {
		t.Errorf("IPv6 unit = %s", u)
	}
	if u := Unit(netip.MustParseAddr("::ffff:203.0.113.9")); u.String() != "203.0.113.9/32" {
		t.Errorf("mapped unit = %s", u)
	}
	ex := Exempt{Own: []netip.Addr{netip.MustParseAddr("192.0.2.10")}, Allow: []netip.Prefix{netip.MustParsePrefix("198.51.100.0/24")}}
	for a, want := range map[string]bool{"127.0.0.5": true, "::1": true, "192.0.2.10": true, "198.51.100.77": true, "203.0.113.9": false} {
		if ex.Has(netip.MustParseAddr(a)) != want {
			t.Errorf("Has(%s) = %v", a, !want)
		}
	}
	for p, want := range map[string]bool{"127.0.0.0/16": true, "192.0.2.0/24": true, "198.51.0.0/16": true, "198.51.100.8/32": true, "203.0.113.0/24": false} {
		if ex.Overlaps(netip.MustParsePrefix(p)) != want {
			t.Errorf("Overlaps(%s) = %v", p, !want)
		}
	}
}

func TestSettingsBounds(t *testing.T) {
	if err := DefaultSettings().Validate(); err != nil {
		t.Fatal(err)
	}
	for _, s := range []Settings{
		{MaxFailures: 2, WindowMinutes: 10, BanMinutes: 60},
		{MaxFailures: 101, WindowMinutes: 10, BanMinutes: 60},
		{MaxFailures: 5, WindowMinutes: 0, BanMinutes: 60},
		{MaxFailures: 5, WindowMinutes: 1441, BanMinutes: 60},
		{MaxFailures: 5, WindowMinutes: 10, BanMinutes: 0},
		{MaxFailures: 5, WindowMinutes: 10, BanMinutes: 43201},
	} {
		if s.Validate() == nil {
			t.Errorf("%+v accepted", s)
		}
	}
}

// SPEC §3: failures count in the window, a ban resets the unit, an IPv6
// /64 counts as one, exempt addresses and a disabled policy count nothing.
func TestTracker(t *testing.T) {
	tr := NewTracker()
	s := Settings{Enabled: true, MaxFailures: 3, WindowMinutes: 10, BanMinutes: 60}
	t0 := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	a := netip.MustParseAddr("203.0.113.9")
	if _, ban := tr.Fail(a, 1, t0, s, Exempt{}); ban {
		t.Fatal("banned at 1")
	}
	// The first failure leaves the window before the third arrives.
	if _, ban := tr.Fail(a, 1, t0.Add(5*time.Minute), s, Exempt{}); ban {
		t.Fatal("banned at 2")
	}
	if _, ban := tr.Fail(a, 1, t0.Add(11*time.Minute), s, Exempt{}); ban {
		t.Fatal("a failure outside the window counted")
	}
	unit, ban := tr.Fail(a, 1, t0.Add(12*time.Minute), s, Exempt{})
	if !ban || unit.String() != "203.0.113.9/32" {
		t.Fatalf("not banned at 3 in the window: %v %v", unit, ban)
	}
	if tr.Len() != 0 {
		t.Fatal("a banned unit kept counting")
	}
	// One report of several failures (an XML-RPC multicall) counts them all.
	if _, ban := tr.Fail(a, 5, t0, s, Exempt{}); !ban {
		t.Fatal("a count of 5 did not ban")
	}
	// Two addresses of one /64 are one unit.
	b1, b2 := netip.MustParseAddr("2001:db8::1"), netip.MustParseAddr("2001:db8::2:3")
	tr.Fail(b1, 2, t0, s, Exempt{})
	if unit, ban := tr.Fail(b2, 1, t0, s, Exempt{}); !ban || unit.String() != "2001:db8::/64" {
		t.Fatalf("the /64 was not one unit: %v %v", unit, ban)
	}
	// Exempt and disabled count nothing.
	ex := Exempt{Allow: []netip.Prefix{netip.MustParsePrefix("198.51.100.0/24")}}
	c := netip.MustParseAddr("198.51.100.7")
	if _, ban := tr.Fail(c, 50, t0, s, ex); ban || tr.Len() != 0 {
		t.Fatal("an allowed address was counted")
	}
	if _, ban := tr.Fail(netip.MustParseAddr("127.0.0.1"), 50, t0, s, Exempt{}); ban {
		t.Fatal("loopback was counted")
	}
	off := s
	off.Enabled = false
	if _, ban := tr.Fail(a, 50, t0, off, Exempt{}); ban || tr.Len() != 0 {
		t.Fatal("counted with automatic blocking off")
	}
}

// SPEC §3: the units are bounded; a full tracker first drops the units with
// nothing in the window.
func TestTrackerBound(t *testing.T) {
	tr := NewTracker()
	tr.max = 3
	s := Settings{Enabled: true, MaxFailures: 5, WindowMinutes: 1, BanMinutes: 60}
	t0 := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	for i := range 5 {
		tr.Fail(netip.AddrFrom4([4]byte{203, 0, 113, byte(i)}), 1, t0, s, Exempt{})
	}
	if tr.Len() != 3 {
		t.Fatalf("units = %d, want the bound 3", tr.Len())
	}
	tr.Fail(netip.MustParseAddr("192.0.2.1"), 1, t0.Add(2*time.Minute), s, Exempt{})
	if tr.Len() != 1 {
		t.Fatalf("after the window units = %d, want 1", tr.Len())
	}
}

// SPEC §4.1.
func TestSSHFailure(t *testing.T) {
	counted := map[string]string{
		"Failed password for root from 203.0.113.9 port 51234 ssh2":                       "203.0.113.9",
		"Failed keyboard-interactive/pam for admin from 2001:db8::7 port 22 ssh2":         "2001:db8::7",
		"Invalid user oracle from 198.51.100.4 port 40000":                                "198.51.100.4",
		"Invalid user  from 198.51.100.5 port 40000":                                      "198.51.100.5",
		"Connection closed by authenticating user root 203.0.113.10 port 41022 [preauth]": "203.0.113.10",
		"Failed password for cg-site-3 from ::ffff:203.0.113.11 port 1 ssh2":              "203.0.113.11",
	}
	for msg, want := range counted {
		a, ok := SSHFailure(msg)
		if !ok || a.String() != want {
			t.Errorf("%q = %v %v, want %s", msg, a, ok, want)
		}
	}
	for _, msg := range []string{
		"Failed password for invalid user oracle from 198.51.100.4 port 40000 ssh2",
		"Accepted publickey for root from 203.0.113.9 port 1 ssh2",
		"Connection closed by 203.0.113.9 port 1 [preauth]",
		"Received disconnect from 203.0.113.9 port 1:11: Bye Bye [preauth]",
		"Failed password for root from not-an-ip port 1 ssh2",
		"Server listening on 0.0.0.0 port 22.",
	} {
		if a, ok := SSHFailure(msg); ok {
			t.Errorf("%q counted as %v", msg, a)
		}
	}
}

// SPEC §4.2.
func TestNginxFailure(t *testing.T) {
	for in, want := range map[string]string{
		"<190>Oct 11 21:00:00 cloudground: 203.0.113.9 failed=1":     "203.0.113.9 1",
		"<190>Oct 11 21:00:00 cloudground: 203.0.113.9 failed=3\n":   "203.0.113.9 3",
		"<190>Oct 11 21:00:00 cloudground: 2001:db8::1 failed":       "2001:db8::1 1",
		"<190>Oct  1 01:02:03 cloudground: 203.0.113.9 failed=99999": "203.0.113.9 100",
	} {
		a, n, ok := NginxFailure([]byte(in))
		if got := a.String() + " " + strconv.Itoa(n); !ok || got != want {
			t.Errorf("%q = %s %v, want %s", in, got, ok, want)
		}
	}
	for _, in := range []string{
		"",
		"cloudground: 203.0.113.9 failed=1",
		"<190>Oct 11 21:00:00 cloudground: 203.0.113.9",
		"<190>Oct 11 21:00:00 cloudground: 203.0.113.9 ok",
		"<190>Oct 11 21:00:00 cloudground: 203.0.113.9 failed=0",
		"<190>Oct 11 21:00:00 cloudground: 203.0.113.9 failed=x",
		"<190>Oct 11 21:00:00 cloudground: nope failed=1",
		"<190>Oct 11 21:00:00 other: 203.0.113.9 failed=1",
		"<190>Oct 11 21:00:00 cloudground: 203.0.113.9 failed=1 extra",
	} {
		if a, n, ok := NginxFailure([]byte(in)); ok {
			t.Errorf("%q = %v %d", in, a, n)
		}
	}
}

// SPEC §5, byte for byte.
func TestNftScript(t *testing.T) {
	got := NftScript(
		[]netip.Prefix{netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("203.0.113.9/32"), netip.MustParsePrefix("192.0.2.1/32"), netip.MustParsePrefix("2001:db8::/64")},
		[]netip.Prefix{netip.MustParsePrefix("198.51.100.0/24")},
	)
	want := `table inet cloudground
delete table inet cloudground
table inet cloudground {
	set allow4 {
		type ipv4_addr; flags interval;
		elements = { 198.51.100.0/24 }
	}
	set allow6 {
		type ipv6_addr; flags interval;
	}
	set block4 {
		type ipv4_addr; flags interval;
		elements = { 192.0.2.1/32, 203.0.113.0/24 }
	}
	set block6 {
		type ipv6_addr; flags interval;
		elements = { 2001:db8::/64 }
	}
	chain input {
		type filter hook input priority -10; policy accept;
		iif "lo" accept
		ip saddr @allow4 accept
		ip6 saddr @allow6 accept
		ip saddr @block4 drop
		ip6 saddr @block6 drop
	}
}
`
	if got != want {
		t.Fatalf("script:\n%s\nwant:\n%s", got, want)
	}
}

func TestMinimal(t *testing.T) {
	var in []netip.Prefix
	for _, s := range []string{"10.1.2.3/32", "10.0.0.0/8", "10.1.0.0/16", "11.0.0.1/32", "11.0.0.0/31", "11.0.0.2/32", "11.0.0.2/32"} {
		in = append(in, netip.MustParsePrefix(s))
	}
	var got []string
	for _, p := range Minimal(in) {
		got = append(got, p.String())
	}
	if strings.Join(got, " ") != "10.0.0.0/8 11.0.0.0/31 11.0.0.2/32" {
		t.Fatalf("Minimal = %v", got)
	}
}
