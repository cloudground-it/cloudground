package api

import (
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/cloudground-it/cloudground/internal/accounts"
)

// clientIP is the address a request came from, for the sign-in limits and
// the audit log (SPEC.md §2.7).
//
// In production panel-api listens on a Unix socket only the panel's nginx
// can open, and nginx sets X-Real-IP from the connection, overwriting what
// the client sent: on that socket, and only there, the header is the client.
// A TCP connection, loopback included, may come from any local process (a
// site's PHP among them), so there the peer address is taken and the header
// ignored; believing it let a site rotate addresses past the sign-in limit
// and write any source it liked into the audit log.
func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	// No host:port: a Unix socket peer.
	if ip := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Real-IP"))); ip != nil {
		return ip.String()
	}
	return "unix"
}

// clientNetwork is what the per-address sign-in limits count for an address
// from clientIP: an IPv4 address itself, an IPv6 address's /64, which one
// host or customer is usually given whole (counting single addresses there
// would give an attacker 2^64 fresh ones). Anything else counts as itself.
func clientNetwork(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	addr = addr.Unmap()
	if addr.Is4() {
		return addr.String()
	}
	p, err := addr.WithZone("").Prefix(64)
	if err != nil {
		return ip
	}
	return p.String()
}

// clientWideNetwork is an IPv6 address's /48, which the limits count too at
// a higher bound (one allocation holds 2^16 /64s); "" for anything else.
func clientWideNetwork(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ""
	}
	addr = addr.Unmap()
	if !addr.Is6() {
		return ""
	}
	p, err := addr.WithZone("").Prefix(48)
	if err != nil {
		return ""
	}
	return p.String()
}

// clientOrigin is the request's address and the networks the limits count.
func clientOrigin(r *http.Request) accounts.Origin {
	ip := clientIP(r)
	return accounts.Origin{Address: ip, Network: clientNetwork(ip), Wide: clientWideNetwork(ip)}
}
