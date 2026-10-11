package api

import (
	"net/http/httptest"
	"testing"
)

// X-Real-IP is believed on the Unix socket only nginx opens, and nowhere
// else: a TCP peer, loopback included, could be any local process (SPEC.md
// §2.7).
func TestClientAddress(t *testing.T) {
	for _, tc := range []struct {
		name, peer, header, want string
	}{
		{"socket, header from nginx", "@", "203.0.113.9", "203.0.113.9"},
		{"socket, ipv6 header", "", "2001:db8::1", "2001:db8::1"},
		{"socket, no header", "", "", "unix"},
		{"socket, header not an address", "@", "203.0.113.9, 10.0.0.1", "unix"},
		{"loopback claiming another address", "127.0.0.1:41234", "203.0.113.9", "127.0.0.1"},
		{"ipv6 loopback claiming another address", "[::1]:41234", "203.0.113.9", "::1"},
		{"direct peer claiming another address", "198.51.100.7:44321", "203.0.113.9", "198.51.100.7"},
		{"direct peer", "198.51.100.7:44321", "", "198.51.100.7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/api/login", nil)
			r.RemoteAddr = tc.peer
			if tc.header != "" {
				r.Header.Set("X-Real-IP", tc.header)
			}
			if got := clientIP(r); got != tc.want {
				t.Fatalf("clientIP = %q, want %q", got, tc.want)
			}
		})
	}
}
