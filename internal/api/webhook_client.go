package api

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"

	"github.com/cloudground-it/cloudground/internal/backuprepo"
)

// The alert webhook's HTTP client (SPEC.md §11.3). An admin types the URL,
// but the panel sends to it from the server, so it must not be a way to
// reach what only the server can: the client follows no redirect, and its
// dialer refuses every address that is not public (backuprepo.PublicAddr:
// loopback, link-local, private, shared, multicast, unspecified, the other
// special ranges, and an IPv6 form of one) and the server's own, checked on
// the address actually dialled (after DNS), so a name that resolves inward
// is refused too.

// webhookHTTP is the client posts go through.
func (s *Server) webhookHTTP() *http.Client {
	if s.webhookClient != nil {
		return s.webhookClient
	}
	return guardedWebhookClient
}

var guardedWebhookClient = &http.Client{
	Timeout:       10 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Transport: &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second, Control: refuseInward}).DialContext,
		TLSHandshakeTimeout: 5 * time.Second,
		MaxIdleConns:        4,
		IdleConnTimeout:     30 * time.Second,
	},
}

// refuseInward is the dialer's check of the address it is about to dial.
func refuseInward(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || inwardIP(ip) {
		return fmt.Errorf("refused to post to %s: not a public address", host)
	}
	return nil
}

// webhookTargetProblem is the save-time check of a webhook's host, with the
// dialer's rule (inwardIP): a literal address, or any address the name
// resolves to now, that the dialer would refuse is refused with a clear
// 400, instead of an alert silently dropped at send time. A name that does
// not resolve yet is accepted: the dialer still checks every address it
// dials.
func (s *Server) webhookTargetProblem(ctx context.Context, host string) error {
	refuse := func(ip net.IP) error {
		return decline(http.StatusBadRequest, "notify_webhook must reach a public address: %s is %s (loopback, private, link-local, shared, multicast or this server's own), and the panel never posts there", host, ip)
	}
	if ip := net.ParseIP(host); ip != nil {
		if inwardIP(ip) {
			return refuse(ip)
		}
		return nil
	}
	lookup := s.webhookLookup
	if lookup == nil {
		lookup = func(ctx context.Context, host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(ctx, "ip", host)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	ips, err := lookup(ctx, host)
	if err != nil {
		return nil
	}
	for _, ip := range ips {
		if inwardIP(ip) {
			return refuse(ip)
		}
	}
	return nil
}

// inwardIP reports whether ip is one the webhook may not reach.
func inwardIP(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok || !backuprepo.PublicAddr(a) {
		return true
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return true // cannot tell: refuse
	}
	for _, x := range addrs {
		if n, ok := x.(*net.IPNet); ok && n.IP.Equal(ip) {
			return true
		}
	}
	return false
}
