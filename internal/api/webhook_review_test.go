package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/state"
)

// The webhook is refused when it is saved, with the send-time dialer's rule,
// not only when an alert is sent: a private or local URL was accepted, and
// every alert was then dropped with a line in the journal nobody reads.
func TestWebhookInwardTargetsRefusedOnSave(t *testing.T) {
	s, _, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	resolves := map[string][]net.IP{
		"inward.example.com": {net.ParseIP("203.0.113.10"), net.ParseIP("10.1.2.3")},
		"hooks.example.com":  {net.ParseIP("203.0.113.10")},
	}
	s.webhookLookup = func(_ context.Context, host string) ([]net.IP, error) {
		if ips, ok := resolves[host]; ok {
			return ips, nil
		}
		return nil, errors.New("no such host")
	}
	put := func(hook string) (int, string) {
		resp, body := c.do("PUT", "/api/settings", map[string]string{"notify_webhook": hook})
		return resp.StatusCode, string(body)
	}
	for _, hook := range []string{
		"https://127.0.0.1/hook",
		"https://[::1]:8443/hook",
		"https://10.0.0.5/hook",
		"https://192.168.1.1/hook",
		"https://169.254.169.254/latest",
		"https://100.64.0.1/hook",
		"https://inward.example.com/hook", // one of its addresses is private
	} {
		code, body := put(hook)
		if code != http.StatusBadRequest || !strings.Contains(body, "notify_webhook must reach a public address") {
			t.Errorf("%s: %d %s", hook, code, body)
		}
	}
	if v, _ := s.Store.Secret(state.SecretNotifyWebhook); v != "" {
		t.Fatalf("a refused webhook was saved: %q", v)
	}
	for _, hook := range []string{
		"https://hooks.example.com/hook",
		"https://203.0.113.10/hook",
		"https://not-yet.example.com/hook", // not resolving yet: the dialer still checks
	} {
		if code, body := put(hook); code != http.StatusOK {
			t.Errorf("%s: %d %s", hook, code, body)
		}
	}
}
