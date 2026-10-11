package accounts

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/throttle"
)

// allowsExactly reports whether w admits n events for one key and not one
// more.
func allowsExactly(w *throttle.Window, n int) bool {
	for range n {
		if !w.Allow("k") {
			return false
		}
	}
	return !w.Allow("k")
}

// waitFor polls cond for up to five seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// who marks a test request's context with the party it stands for, so the
// Sleep hook can tell the attacker's delays from the owner's.
type whoKey struct{}

func as(party string) context.Context { return context.WithValue(bg, whoKey{}, party) }

func partyOf(ctx context.Context) string { s, _ := ctx.Value(whoKey{}).(string); return s }

const (
	attackerNet = "192.0.2.66"
	ownerNet    = "203.0.113.7"
)

// Parallel attempts from one network each wait their own, growing, delay,
// spaced one after the other (no faster than sequential guesses) and
// holding no place in the e-mail's queue while they sleep; one more from
// that network, within the minute, is not refused, it waits longer;
// meanwhile the owner, from another network, waits only the
// e-mail's capped delay and gets in (SPEC §2.3 step 3).
func TestDelaysAreSleptOutsideTheQueue(t *testing.T) {
	f := newFixture(t)
	f.user(t, "victim@example.com", "admin")
	for range DelayAfter {
		f.sv.Limits.Delays.Fail("victim@example.com", attackerNet)
	}
	release := make(chan struct{})
	var mu sync.Mutex
	slept := map[string][]time.Duration{}
	f.sv.Sleep = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		slept[partyOf(ctx)] = append(slept[partyOf(ctx)], d)
		mu.Unlock()
		if partyOf(ctx) == "owner" {
			return nil
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	sleeping := func(party string) []time.Duration {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(slept[party])
	}

	var wg sync.WaitGroup
	errs := make(chan error, AccountQueue+1)
	attack := func() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.sv.Login(as("attacker"), SignIn{Email: "victim@example.com", Password: "wrong-password", Address: attackerNet})
			errs <- err
		}()
	}
	for range AccountQueue {
		attack()
	}
	waitFor(t, "the attempts to sleep their delays", func() bool { return len(sleeping("attacker")) == AccountQueue })
	if n := f.sv.turns.InFlight("victim@example.com"); n != 0 {
		t.Fatalf("%d sleeping attempts hold a place in the e-mail's queue", n)
	}
	attack()
	waitFor(t, "a fifth attempt to sleep", func() bool { return len(sleeping("attacker")) == AccountQueue+1 })
	got := sleeping("attacker")
	slices.Sort(got)
	if want := []time.Duration{time.Second, 3 * time.Second, 7 * time.Second, 15 * time.Second, 31 * time.Second}; !slices.Equal(got, want) {
		t.Fatalf("the attacker's parallel attempts wait %v, want each its own, spaced: %v", got, want)
	}

	start := time.Now()
	if _, err := f.sv.Login(as("owner"), SignIn{Email: "victim@example.com", Password: pw, Address: ownerNet}); err != nil {
		t.Fatalf("the owner, from another network, was kept out: %v", err)
	}
	if got := sleeping("owner"); len(got) != 1 || got[0] != NameMaxDelay {
		t.Fatalf("the owner waited %v, want the e-mail's cap %v", got, NameMaxDelay)
	}
	t.Logf("the owner got in after %v (its delay not slept here) while five attempts slept", time.Since(start))

	// Released together, the five queue for their checks at once: one
	// network holds at most AccountQueue places, so one of them may be
	// refused, for its own network's backlog.
	close(release)
	wg.Wait()
	close(errs)
	checked := 0
	for err := range errs {
		if ref, _ := IsRefusal(err); ref != nil && ref.Msg == msgAccountBusy {
			continue
		}
		wantRefusal(t, err, Unauthorized, msgBadLogin)
		checked++
	}
	if checked < AccountQueue {
		t.Fatalf("%d of the attacker's attempts were checked, want at least %d", checked, AccountQueue)
	}
	// The attacker's network keeps its failures, and its delay.
	if n := f.sv.Limits.Delays.ScopedFailures("victim@example.com", attackerNet); n != DelayAfter+checked {
		t.Fatalf("the attacker's network has %d failures, want %d", n, DelayAfter+checked)
	}
	if f.sv.turns.Keys() != 0 {
		t.Fatal("a queue was kept after every attempt ended")
	}
	if _, inFlight := f.sv.Limits.Delays.Size(); inFlight != 0 {
		t.Fatalf("%d attempts still counted in flight", inFlight)
	}
}

// R-4.3 (S-H3 review): someone hammering one e-mail from one network, as
// fast and as parallel as the limits let them, slows their own guesses and
// never keeps the owner out. Time runs fast for the limits and the delays
// (a minute is 1.2 s), and the delays are really slept: the owner signs in
// from another network while the attack runs, each time within a bound,
// never refused; the attacker's delays have grown; nothing is left behind.
func TestAttackerFromOneNetworkCannotKeepTheOwnerOut(t *testing.T) {
	if testing.Short() {
		t.Skip("runs an attack for a few seconds")
	}
	f := newFixture(t)
	f.user(t, "victim@example.com", "admin")

	// One credential check, timed, for the bound (argon2 under the race
	// detector is slow, and the box is not this machine).
	t0 := time.Now()
	f.sv.Login(bg, SignIn{Email: "someone-else@example.com", Password: "wrong-password", Address: "198.51.100.1"})
	check := time.Since(t0)

	const speed = 50
	origin, begun := f.at, time.Now()
	f.sv.Now = func() time.Time { return origin.Add(time.Since(begun) * speed) }
	for _, w := range []*throttle.Window{f.sv.Limits.Address, f.sv.Limits.Wide, f.sv.Limits.AddressAccount, f.sv.Limits.Codes} {
		w.WithClock(f.sv.Now)
	}
	f.sv.Limits.Delays.WithClock(f.sv.Now)
	var mu sync.Mutex
	slept := map[string][]time.Duration{}
	f.sv.Sleep = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		slept[partyOf(ctx)] = append(slept[partyOf(ctx)], d)
		mu.Unlock()
		select {
		case <-time.After(d / speed):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	stop := make(chan struct{})
	var guesses, throttled atomic.Int64
	var wg sync.WaitGroup
	for range 2 * AccountQueue {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, err := f.sv.Login(as("attacker"), SignIn{Email: "victim@example.com", Password: "wrong-password", Address: attackerNet})
				if ref, ok := IsRefusal(err); ok && ref.Kind == Throttled {
					throttled.Add(1)
					time.Sleep(time.Millisecond)
				} else {
					guesses.Add(1)
				}
			}
		}()
	}

	// The owner, once the attack has built its delays up, three times.
	bound := NameMaxDelay/speed + time.Duration(AccountQueue+2)*check + 500*time.Millisecond
	time.Sleep(1500 * time.Millisecond)
	for i := range 3 {
		start := time.Now()
		_, err := f.sv.Login(as("owner"), SignIn{Email: "victim@example.com", Password: pw, Address: ownerNet})
		took := time.Since(start)
		if err != nil {
			t.Fatalf("sign-in %d of the owner during the attack: %v", i+1, err)
		}
		if took > bound {
			t.Fatalf("sign-in %d of the owner took %v, over the bound %v (one check takes %v)", i+1, took, bound, check)
		}
		time.Sleep(300 * time.Millisecond)
	}
	close(stop)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	for _, d := range slept["owner"] {
		if d > NameMaxDelay {
			t.Fatalf("the owner slept %v, over the e-mail's cap", d)
		}
	}
	longest := slices.Max(append(slept["attacker"], 0))
	t.Logf("%d guesses and %d throttled attempts by the attacker; its longest delay %v; one check %v", guesses.Load(), throttled.Load(), longest, check)
	if longest < 16*time.Second {
		t.Fatalf("the attacker's delays stayed at %v, want them grown to 16s or more", longest)
	}
	// Sequential guesses would get five through at once, then one at 1, 3,
	// 7, 15, 31, 63, 123 and 183 s: parallel ones are no faster (the build
	// before let 8 a minute through, the per-pair window, once every delay
	// had reached its minute).
	if guesses.Load() > 20 {
		t.Fatalf("%d guesses got through in about 4 s (some 200 s of the limits' time)", guesses.Load())
	}
	// Its network's delay outlives the owner's sign-ins.
	next, err := f.sv.Limits.Delays.Begin("victim@example.com", attackerNet)
	if err != nil {
		t.Fatalf("the attacker's next attempt, nothing in flight: %v", err)
	}
	next.End()
	if next.Wait() < 16*time.Second {
		t.Fatalf("after the owner's sign-ins the attacker's next attempt waits %v", next.Wait())
	}
	if f.sv.turns.Keys() != 0 {
		t.Fatal("a queue was kept after the attack")
	}
	// The e-mail and the attacker's network, and the timed check's two.
	if failures, inFlight := f.sv.Limits.Delays.Size(); inFlight != 0 || failures > 4 {
		t.Fatalf("after the attack the delays hold %d failure keys and %d in flight", failures, inFlight)
	}
}

// The account routes that check the password count against the client's
// network like sign-ins, and an IPv6 /48 is bounded as a whole.
func TestPasswordChecksAreLimitedPerNetwork(t *testing.T) {
	f := newFixture(t)
	u := f.user(t, "self@example.com", "admin")
	from := Origin{Address: "203.0.113.9", Network: "203.0.113.9"}
	for range 30 {
		f.sv.Login(bg, SignIn{Email: "someone@example.com", Password: "x", Address: from.Address, Network: from.Network})
	}
	_, err := f.sv.BeginTOTP(bg, from, u, pw, "")
	wantRefusal(t, err, Throttled, msgTooMany)
	err = f.sv.ChangePassword(bg, from, u, u.Email, pw, "brand-new-password")
	wantRefusal(t, err, Throttled, msgTooMany)
	err = f.sv.DisableTOTP(bg, from, u, u.Email, pw, "", "")
	wantRefusal(t, err, Throttled, msgTooMany)
	// From elsewhere the same account is fine.
	if _, err := f.sv.BeginTOTP(bg, Origin{Address: "203.0.113.10"}, u, pw, ""); err != nil {
		t.Fatalf("begin from another network = %v", err)
	}

	// Per /48, whatever /64 they come from (120 a minute; 5 here, to keep
	// the argon2 work of the test small).
	if got := DefaultLimits().Wide; got.Count("x") != 0 || !allowsExactly(got, 120) {
		t.Fatal("the default /48 bound is not 120 a minute")
	}
	f = newFixture(t)
	f.sv.Limits.Wide = throttle.New(5, time.Minute).WithClock(f.sv.Now)
	for i := range 5 {
		n := fmt.Sprintf("2001:db8:1:%x::/64", i)
		f.sv.Login(bg, SignIn{Email: fmt.Sprintf("g%d@example.com", i), Password: "x", Address: "2001:db8:1::1", Network: n, Wide: "2001:db8:1::/48"})
	}
	_, err = f.sv.Login(bg, SignIn{Email: "fresh@example.com", Password: "x", Address: "2001:db8:1:ffff::1", Network: "2001:db8:1:ffff::/64", Wide: "2001:db8:1::/48"})
	wantRefusal(t, err, Throttled, msgTooMany)
	_, err = f.sv.Login(bg, SignIn{Email: "fresh@example.com", Password: "x", Address: "2001:db8:2::1", Network: "2001:db8:2::/64", Wide: "2001:db8:2::/48"})
	wantRefusal(t, err, Unauthorized, msgBadLogin)
}

// The failures of an IPv6 client's /48 delay a fresh /64 of it: an
// allocation's 65,536 /64s do not each start without a delay.
func TestWideNetworkSharesItsDelay(t *testing.T) {
	f := newFixture(t)
	for i := range 7 {
		f.sv.Limits.Delays.Fail("victim@example.com", fmt.Sprintf("2001:db8:1:%x::/64", i), "2001:db8:1::/48")
	}
	f.slept = nil
	_, err := f.sv.Login(bg, SignIn{Email: "victim@example.com", Password: "x", Address: "2001:db8:1:ff::1", Network: "2001:db8:1:ff::/64", Wide: "2001:db8:1::/48"})
	wantRefusal(t, err, Unauthorized, msgBadLogin)
	if len(f.slept) != 1 || f.slept[0] != 4*time.Second {
		t.Fatalf("a fresh /64 of a failing /48 waited %v, want the /48's 4s", f.slept)
	}
}

// S-H3 review: one network cannot guess faster by sending in parallel.
// With an attempt of its own already waiting most of a minute, one more
// from that network is refused at once (429, no failure counted, no
// request held), while the owner from another network still gets in.
func TestParallelGuessesFromOneNetworkAreNoFaster(t *testing.T) {
	f := newFixture(t)
	f.user(t, "victim@example.com", "admin")
	for range 2 * DelayAfter {
		f.sv.Limits.Delays.Fail("victim@example.com", attackerNet)
	}
	release := make(chan struct{})
	asleep := make(chan time.Duration, 1)
	f.sv.Sleep = func(ctx context.Context, d time.Duration) error {
		if partyOf(ctx) != "attacker" {
			return nil
		}
		asleep <- d
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	first := make(chan error, 1)
	go func() {
		_, err := f.sv.Login(as("attacker"), SignIn{Email: "victim@example.com", Password: "wrong-password", Address: attackerNet})
		first <- err
	}()
	if d := <-asleep; d != 32*time.Second {
		t.Fatalf("the attacker's attempt after ten failures waits %v, want 32s", d)
	}
	_, err := f.sv.Login(as("attacker"), SignIn{Email: "victim@example.com", Password: "wrong-password", Address: attackerNet})
	wantRefusal(t, err, Throttled, msgAccountBusy)
	if n := f.sv.Limits.Delays.ScopedFailures("victim@example.com", attackerNet); n != 2*DelayAfter {
		t.Fatalf("the refused attempt counted: %d failures, want %d", n, 2*DelayAfter)
	}
	if _, err := f.sv.Login(as("owner"), SignIn{Email: "victim@example.com", Password: pw, Address: ownerNet}); err != nil {
		t.Fatalf("the owner, from another network: %v", err)
	}
	close(release)
	wantRefusal(t, <-first, Unauthorized, msgBadLogin)
}
