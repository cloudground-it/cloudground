package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/accounts"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

// passwordAgent answers DBStudioOpen as the database server would: a
// session for the right password, a refusal for any other.
type passwordAgent struct {
	AgentCaller
	password string
}

func (p passwordAgent) Call(ctx context.Context, method string, params, out any) error {
	if method != agentrpc.DBStudioOpen.Name() {
		return p.AgentCaller.Call(ctx, method, params, out)
	}
	res := out.(*rpc.DBStudioOpenResult)
	if params.(rpc.DBStudioOpenParams).Password == p.password {
		*res = rpc.DBStudioOpenResult{Login: strings.Repeat("ef", 32), Databases: []string{"wp_studio"}}
	} else {
		*res = rpc.DBStudioOpenResult{Refused: true}
	}
	return nil
}

type partyKey struct{}

// studioSignInFrom sends a Quarry sign-in from addr straight to the
// handler, its context marked with party.
func studioSignInFrom(s *Server, party, addr, user, password string) *httptest.ResponseRecorder {
	body := `{"user":"` + user + `","password":"` + password + `"}`
	r := httptest.NewRequest("POST", "/api/quarry/login", strings.NewReader(body))
	r.RemoteAddr = addr + ":40000"
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(context.WithValue(r.Context(), partyKey{}, party))
	w := httptest.NewRecorder()
	s.handleStudioLogin(w, r)
	return w
}

// SPEC.md §6.4 step 3: someone failing a database user's sign-in from one
// network, with as many attempts in flight as the windows let them, slows
// their own guesses and never keeps the user out: the user, from another
// network, waits at most the name's capped delay and is never refused for
// the attacker's backlog. The attacker's attempts sleep their delays
// holding no place in the user's queue, and its network keeps its failures
// after the user's sign-in.
func TestStudioLoginAttackerCannotKeepTheUserOut(t *testing.T) {
	s, _, _, _ := loginSetup(t)
	s.Agent = passwordAgent{AgentCaller: s.Agent, password: "reader-password-1"}
	const attacker, owner = "192.0.2.66", "203.0.113.7"
	g := s.studioGate
	at := time.Now()
	g.delays.WithClock(func() time.Time { return at }) // the waits read exactly
	for range accounts.DelayAfter {
		g.delays.Fail("reader", attacker)
	}
	release := make(chan struct{})
	var mu sync.Mutex
	slept := map[string][]time.Duration{}
	g.sleep = func(ctx context.Context, d time.Duration) error {
		party, _ := ctx.Value(partyKey{}).(string)
		mu.Lock()
		slept[party] = append(slept[party], d)
		mu.Unlock()
		if party == "owner" {
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
	codes := make(chan int, studioQueue)
	for range studioQueue {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- studioSignInFrom(s, "attacker", attacker, "reader", "wrong-password").Code
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(sleeping("attacker")) < studioQueue {
		if time.Now().After(deadline) {
			t.Fatal("the attacker's attempts never slept their delays")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := g.lanes.InFlight("reader"); n != 0 {
		t.Fatalf("%d sleeping attempts hold a place in the user's queue", n)
	}

	start := time.Now()
	w := studioSignInFrom(s, "owner", owner, "reader", "reader-password-1")
	if w.Code != http.StatusOK {
		t.Fatalf("the user, from another network, during the attack: %d %s", w.Code, w.Body)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the user's sign-in took %v", took)
	}
	if got := sleeping("owner"); len(got) != 1 || got[0] != accounts.NameMaxDelay {
		t.Fatalf("the user waited %v, want the name's cap %v", got, accounts.NameMaxDelay)
	}

	close(release)
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != http.StatusUnauthorized && c != http.StatusTooManyRequests {
			t.Fatalf("an attacker's attempt answered %d", c)
		}
	}
	got := sleeping("attacker")
	slices.Sort(got)
	if !slices.Equal(got, []time.Duration{time.Second, 3 * time.Second, 7 * time.Second, 15 * time.Second}) {
		t.Fatalf("the attacker's parallel attempts waited %v, want each its own, spaced", got)
	}
	next, err := g.delays.Begin("reader", attacker)
	if err != nil {
		t.Fatalf("the attacker's next attempt, nothing in flight: %v", err)
	}
	next.End()
	if next.Wait() < 8*time.Second {
		t.Fatalf("after the user's sign-in the attacker's next attempt waits %v", next.Wait())
	}
	if g.lanes.Keys() != 0 {
		t.Fatal("a queue was kept")
	}
	if _, inFlight := g.delays.Size(); inFlight != 0 {
		t.Fatalf("%d attempts still counted in flight", inFlight)
	}
}

// SPEC.md §6.4 step 3 (S-H3 review): with an attempt from one network
// already waiting most of a minute for a database user, one more from that
// network is refused at once, 429, and the user from another network
// still signs in.
func TestStudioLoginParallelGuessesAreNoFaster(t *testing.T) {
	s, _, _, _ := loginSetup(t)
	s.Agent = passwordAgent{AgentCaller: s.Agent, password: "reader-password-1"}
	const attacker, owner = "192.0.2.66", "203.0.113.7"
	g := s.studioGate
	for range 2 * accounts.DelayAfter {
		g.delays.Fail("reader", attacker)
	}
	release := make(chan struct{})
	asleep := make(chan time.Duration, 1)
	g.sleep = func(ctx context.Context, d time.Duration) error {
		if party, _ := ctx.Value(partyKey{}).(string); party != "attacker" {
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
	first := make(chan int, 1)
	go func() { first <- studioSignInFrom(s, "attacker", attacker, "reader", "wrong-password").Code }()
	if d := <-asleep; d != 32*time.Second {
		t.Fatalf("the attacker's attempt after ten refusals waits %v, want 32s", d)
	}
	if w := studioSignInFrom(s, "attacker", attacker, "reader", "wrong-password"); w.Code != http.StatusTooManyRequests ||
		!strings.Contains(w.Body.String(), msgStudioBusy) {
		t.Fatalf("a second attempt from the waiting network: %d %s", w.Code, w.Body)
	}
	if n := g.delays.ScopedFailures("reader", attacker); n != 2*accounts.DelayAfter {
		t.Fatalf("the refused attempt counted: %d", n)
	}
	if w := studioSignInFrom(s, "owner", owner, "reader", "reader-password-1"); w.Code != http.StatusOK {
		t.Fatalf("the user, from another network: %d %s", w.Code, w.Body)
	}
	close(release)
	if c := <-first; c != http.StatusUnauthorized {
		t.Fatalf("the attacker's first attempt answered %d", c)
	}
}
