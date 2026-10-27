package accounts

import (
	"fmt"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

// One network's passkey sign-ins cannot crowd out another's: past
// perNetworkSignIns the network's oldest challenge goes, nobody else's.
func TestCeremoniesBoundSignInsPerNetwork(t *testing.T) {
	c := newCeremonies()
	now := time.Unix(1_800_000_000, 0)
	signIn := func(network string, i int) string {
		ch := fmt.Sprintf("%s-%d", network, i)
		if err := c.put(&ceremony{purpose: purposeSignIn, network: network, session: webauthn.SessionData{Challenge: ch}}, now); err != nil {
			t.Fatalf("put %s: %v", ch, err)
		}
		return ch
	}
	other := signIn("198.51.100.7", 0)
	for i := 0; i < 3*perNetworkSignIns; i++ {
		signIn("203.0.113.9", i)
	}
	if got := len(c.m); got != perNetworkSignIns+1 {
		t.Fatalf("%d challenges wait, want %d", got, perNetworkSignIns+1)
	}
	if c.take(other, now) == nil {
		t.Fatal("another network's challenge was dropped")
	}
	if c.take("203.0.113.9-0", now) != nil {
		t.Fatal("the flooding network's oldest challenge was kept")
	}
	if c.take(fmt.Sprintf("203.0.113.9-%d", 3*perNetworkSignIns-1), now) == nil {
		t.Fatal("the flooding network's newest challenge was dropped")
	}
}
