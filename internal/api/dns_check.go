package api

// The DNS readiness check (SPEC.md §3.14, issue #132): does a host name
// already point at this server, so that its certificate can be issued?
// The new-site wizard asks it per name while they are typed.
//
// It is a lookup, never a connection, and not a resolver for anything
// else: only a host name a site could have (site.ValidateDomain), never an
// address; the system resolver, A and AAAA, three seconds at most; per
// account and panel-wide limits; the answer names only public addresses
// (an inward one is said to be private, not which), compared with the
// public addresses of this machine's own interfaces.

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/throttle"
)

// The check's limits: lookups per account and for the whole panel a
// minute, and how long one check may wait for the resolver.
const (
	dnsChecksPerUser  = 30
	dnsChecksPerPanel = 120
	dnsCheckTimeout   = 3 * time.Second
)

// msgDNSTooMany is the 429's message.
const msgDNSTooMany = "too many DNS checks; try again in a minute"

// The verdicts (dnsCheck.Status).
const (
	dnsHere       = "here"       // every address is this server's
	dnsPartial    = "partial"    // some are, some are not (often the AAAA)
	dnsElsewhere  = "elsewhere"  // none is
	dnsProxied    = "proxied"    // none is, and all are a CDN's (its proxy hides the origin)
	dnsUnresolved = "unresolved" // the name has no A or AAAA record (yet)
	dnsUnknown    = "unknown"    // the lookup failed, or this server has no public address to compare with
)

// dnsCheck is the answer of GET /api/sites/dns-check.
type dnsCheck struct {
	Domain string `json:"domain"`
	Status string `json:"status"`
	// Addresses are what the name resolves to, A first.
	Addresses []dnsAddress `json:"addresses"`
	// Server is this machine's public addresses.
	Server []string `json:"server"`
	// CDN names the proxy every address belongs to ("cloudflare").
	CDN string `json:"cdn,omitempty"`
}

// dnsAddress is one address a name resolves to. A private one (loopback,
// RFC 1918, link-local, shared) is not named.
type dnsAddress struct {
	Family  int    `json:"family"` // 4 or 6
	Address string `json:"address,omitempty"`
	Private bool   `json:"private,omitempty"`
	Own     bool   `json:"own"`
}

// dnsGate holds the check's limits, made on first use.
type dnsGate struct {
	once        sync.Once
	user, panel *throttle.Window
}

func (g *dnsGate) admit(user int64) bool {
	g.once.Do(func() {
		g.user = throttle.New(dnsChecksPerUser, time.Minute)
		g.panel = throttle.New(dnsChecksPerPanel, time.Minute)
	})
	return g.user.Allow(strconv.FormatInt(user, 10)) && g.panel.Allow("panel")
}

// handleDNSCheck is GET /api/sites/dns-check?domain=<name> (manage).
func (s *Server) handleDNSCheck(w http.ResponseWriter, r *http.Request, c *call) {
	name := strings.TrimSuffix(cleanHost(r.URL.Query().Get("domain")), ".")
	if err := site.ValidateDomain(name); err != nil {
		respondErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := netip.ParseAddr(name); err == nil {
		respondErr(w, http.StatusBadRequest, "a host name is required, not an address")
		return
	}
	if !s.dnsChecks.admit(c.User.ID) {
		respondErr(w, http.StatusTooManyRequests, msgDNSTooMany)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), dnsCheckTimeout)
	defer cancel()
	respond(w, http.StatusOK, s.checkDNS(ctx, name))
}

// checkDNS resolves name and compares it with this machine's addresses.
func (s *Server) checkDNS(ctx context.Context, name string) dnsCheck {
	out := dnsCheck{Domain: name, Addresses: []dnsAddress{}, Server: []string{}}
	own := publicAddrs(s.ownAddrs())
	for _, a := range own {
		out.Server = append(out.Server, a.String())
	}
	lookup := s.dnsLookup
	if lookup == nil {
		lookup = func(ctx context.Context, network, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, network, host)
		}
	}
	// A and AAAA apart: one family failing (a resolver that times out
	// on AAAA) still answers the other.
	var (
		wg     sync.WaitGroup
		found  [2][]netip.Addr
		failed [2]bool
	)
	for i, network := range []string{"ip4", "ip6"} {
		wg.Go(func() {
			addrs, err := lookup(ctx, network, name)
			if err != nil && !notFound(err) {
				failed[i] = true
			}
			found[i] = addrs
		})
	}
	wg.Wait()

	var mine, others, cdn int
	for _, list := range found {
		for _, a := range sortedAddrs(list) {
			a = a.Unmap()
			d := dnsAddress{Family: 4, Own: slices.Contains(own, a)}
			if a.Is6() {
				d.Family = 6
			}
			if inwardAddr(a) && !d.Own {
				d.Private = true
			} else {
				d.Address = a.String()
			}
			out.Addresses = append(out.Addresses, d)
			switch {
			case d.Own:
				mine++
			default:
				others++
				if cloudflare(a) {
					cdn++
				}
			}
		}
	}
	switch {
	case len(out.Addresses) == 0 && (failed[0] || failed[1]):
		out.Status = dnsUnknown
	case len(out.Addresses) == 0:
		out.Status = dnsUnresolved
	case others == 0:
		out.Status = dnsHere
	case mine > 0:
		out.Status = dnsPartial
	case cdn == others:
		out.Status, out.CDN = dnsProxied, "cloudflare"
	case len(own) == 0:
		out.Status = dnsUnknown
	default:
		out.Status = dnsElsewhere
	}
	return out
}

// notFound reports whether err is the name (or this family of it) not
// existing, which is an answer, not a failure.
func notFound(err error) bool {
	var de *net.DNSError
	return errors.As(err, &de) && de.IsNotFound
}

// sortedAddrs is list sorted, without repeats.
func sortedAddrs(list []netip.Addr) []netip.Addr {
	out := slices.Clone(list)
	slices.SortFunc(out, func(a, b netip.Addr) int { return a.Compare(b) })
	return slices.Compact(out)
}

// publicAddrs are the addresses of list a visitor could reach.
func publicAddrs(list []netip.Addr) []netip.Addr {
	var out []netip.Addr
	for _, a := range sortedAddrs(list) {
		if a = a.Unmap(); !inwardAddr(a) {
			out = append(out, a)
		}
	}
	return out
}

var sharedPrefix = netip.MustParsePrefix("100.64.0.0/10")

// inwardAddr reports whether a is not a public unicast address: loopback,
// private, link-local, shared (100.64/10), multicast or unspecified.
func inwardAddr(a netip.Addr) bool {
	return !a.IsValid() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified() || sharedPrefix.Contains(a)
}

// cloudflareRanges are Cloudflare's published proxy ranges
// (https://www.cloudflare.com/ips/): a name proxied through it (the orange
// cloud) resolves to them, not to the origin.
var cloudflareRanges = func() []netip.Prefix {
	var out []netip.Prefix
	for _, p := range []string{
		"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22", "141.101.64.0/18",
		"108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20", "197.234.240.0/22", "198.41.128.0/17",
		"162.158.0.0/15", "104.16.0.0/13", "104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
		"2400:cb00::/32", "2606:4700::/32", "2803:f800::/32", "2405:b500::/32", "2405:8100::/32",
		"2a06:98c0::/29", "2c0f:f248::/32",
	} {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}()

// cloudflare reports whether a is in one of Cloudflare's proxy ranges.
func cloudflare(a netip.Addr) bool {
	for _, p := range cloudflareRanges {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
