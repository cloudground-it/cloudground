package rpc

import (
	"strings"
	"testing"
)

// H-5-1: a literal host the agent would never connect is refused at once;
// loopback, private addresses and names pass (a name is checked by the
// agent, on what it resolves to).
func TestCheckDBServerHostAddresses(t *testing.T) {
	for _, h := range []string{"169.254.169.254", "169.254.0.1", "fe80::1", "[fe80::1]", "0.0.0.0", "::", "224.0.0.9", "ff02::1", "::ffff:169.254.169.254"} {
		if err := CheckDBServerHost(h); err == nil || !strings.Contains(err.Error(), "never connects there") {
			t.Errorf("%s: %v", h, err)
		}
	}
	for _, h := range []string{"127.0.0.1", "::1", "10.0.0.5", "192.168.1.20", "db.example.com", "localhost", "[2001:db8::5]"} {
		if err := CheckDBServerHost(h); err != nil {
			t.Errorf("%s refused: %v", h, err)
		}
	}
}
