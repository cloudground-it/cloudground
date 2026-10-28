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

// A /48 is bounded as a whole, and passkey sign-ins together cannot take
// the room of signed-in accounts' challenges.
func TestCeremoniesBoundSignInsPerWideAndOverall(t *testing.T) {
	c := newCeremonies()
	now := time.Unix(1_800_000_000, 0)
	put := func(cer *ceremony, ch string) error {
		cer.session = webauthn.SessionData{Challenge: ch}
		return c.put(cer, now)
	}
	// One /48, every /64 of it new: the /48 keeps perWideSignIns.
	for i := range 4 * perWideSignIns {
		net := fmt.Sprintf("2001:db8:1:%x::/64", i)
		if err := put(&ceremony{purpose: purposeSignIn, network: net, wide: "2001:db8:1::/48"}, fmt.Sprintf("w-%d", i)); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	if len(c.m) != perWideSignIns {
		t.Fatalf("%d challenges from one /48, want %d", len(c.m), perWideSignIns)
	}
	// Many /48s: sign-ins stop at MaxSignIns, a signed-in proof still fits.
	var refused bool
	for i := 0; i < MaxCeremonies && !refused; i++ {
		wide := fmt.Sprintf("2001:db8:%x::/48", 0x100+i/perWideSignIns)
		net := fmt.Sprintf("2001:db8:%x:%x::/64", 0x100+i/perWideSignIns, i%perWideSignIns)
		refused = put(&ceremony{purpose: purposeSignIn, network: net, wide: wide}, fmt.Sprintf("m-%d", i)) != nil
	}
	if !refused || len(c.m) != MaxSignIns {
		t.Fatalf("sign-ins: refused %v at %d, want refused at %d", refused, len(c.m), MaxSignIns)
	}
	if err := put(&ceremony{purpose: purposeProof, userID: 7}, "proof"); err != nil {
		t.Fatalf("a signed-in account's proof: %v", err)
	}
}
