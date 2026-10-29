package blocklist

import (
	"net/netip"
	"strings"
	"testing"
)

// What the parsers take from a journal line or a datagram: a valid address
// that the input really names, and a count of 1 to 100.
func FuzzSSHFailure(f *testing.F) {
	for _, s := range []string{
		"Failed password for root from 203.0.113.7 port 22 ssh2",
		"Invalid user admin from 2001:db8::1 port 4242",
		"Connection closed by authenticating user root 198.51.100.9 port 1 [preauth]",
		"Failed password for invalid user x from 203.0.113.7 port 22 ssh2",
		"Failed keyboard-interactive/pam for x from ::ffff:192.0.2.1 port 2 ssh2",
		"Invalid user a from 1.2.3.4 port 1 from 5.6.7.8 port 2",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, msg string) {
		a, ok := SSHFailure(msg)
		if !ok {
			return
		}
		if !a.IsValid() || a.Is4In6() || a.Zone() != "" {
			t.Fatalf("%q: address %v", msg, a)
		}
		named := false
		for _, w := range strings.Fields(msg) {
			if b, err := netip.ParseAddr(w); err == nil && b.Unmap() == a {
				named = true
			}
		}
		if !named {
			t.Fatalf("%q: %v is not in the line", msg, a)
		}
	})
}

func FuzzNginxFailure(f *testing.F) {
	for _, s := range []string{
		"<190>Oct 29 10:00:00 cloudground: 203.0.113.7 failed=3",
		"<190>Oct 29 10:00:00 cloudground: 2001:db8::1 failed",
		"<190>x cloudground: 203.0.113.7 failed=0",
		"<190>x cloudground: fe80::1%eth0 failed=2",
		"cloudground: 1.2.3.4 failed=999999999999999999999",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, d []byte) {
		a, n, ok := NginxFailure(d)
		if !ok {
			return
		}
		if !a.IsValid() || a.Is4In6() || a.Zone() != "" || n < 1 || n > maxCount {
			t.Fatalf("%q: %v %d", d, a, n)
		}
	})
}
