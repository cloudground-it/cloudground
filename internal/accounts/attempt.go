package accounts

import (
	"context"
	"errors"
	"time"

	"github.com/cloudground-it/cloudground/internal/throttle"
)

// An attempt on an account (SPEC §2.3 step 3) first waits out its delay,
// holding nothing but its own request: the longer of the failures of the
// e-mail from the client's network (and IPv6 /48), which grow to a minute
// and are spaced (parallel attempts from one network wait one after the
// other's delay; one that would wait over a minute is refused), and those
// of the e-mail from anywhere, which stop at NameMaxDelay. Only then does
// it take the e-mail's turn, which is held for the check alone. So someone
// failing an e-mail from one network slows their own guesses to the
// schedule, however parallel, and the owner signing in from another waits
// at most NameMaxDelay plus the checks already queued ahead of it, never a
// refusal for their backlog.

// AccountQueue is how many attempts for one e-mail from one client network
// may be in the e-mail's queue at once: the one being checked and those
// waiting for it. One more from that network is refused (429) rather than
// queued, so a flood from one network cannot pile up held requests; other
// networks still queue.
const AccountQueue = 4

const msgAccountBusy = "too many attempts for this account at once — wait a moment and try again"

// attempt is one check of an account's credentials: its verdict (Fail,
// Succeed, or End without one) and its turn.
type attempt struct {
	*throttle.Attempt
	leave func()
}

// end leaves the turn and ends the attempt; a verdict given before stays.
func (a *attempt) end() {
	a.leave()
	a.End()
}

// begin waits out the attempt's delay, then takes the e-mail's turn. The
// caller ends what it returns.
func (sv *Service) begin(ctx context.Context, o Origin, email string) (*attempt, error) {
	d, err := sv.Limits.Delays.Begin(email, o.network(), o.Wide)
	if err != nil {
		return nil, refuse(Throttled, msgAccountBusy)
	}
	// A wait cut short (the client went away, or a deadline of the
	// server's) is the delay's refusal, never an internal error: the
	// attempt was throttled, and no verdict was given.
	if err := sv.sleep(ctx, d.Wait()); err != nil {
		d.End()
		return nil, refuse(Throttled, msgTooMany)
	}
	leave, err := sv.turns.Enter(ctx, email, o.network())
	if err != nil {
		d.End()
		if errors.Is(err, throttle.ErrQueueFull) {
			return nil, refuse(Throttled, msgAccountBusy)
		}
		if ctx.Err() != nil {
			return nil, refuse(Throttled, msgTooMany)
		}
		return nil, err
	}
	return &attempt{Attempt: d, leave: leave}, nil
}

// sleep waits d, or returns ctx's error.
func (sv *Service) sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	if sv.Sleep != nil {
		return sv.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
