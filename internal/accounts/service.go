package accounts

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/state"
	"github.com/cloudground-it/cloudground/internal/throttle"
)

// Kind is what sort of refusal an Error is; the API maps each to one
// status (SPEC §2).
type Kind uint8

const (
	Invalid Kind = iota + 1
	Unauthorized
	Forbidden
	NotFound
	Conflict
	Throttled
	// Unavailable: too many password checks queued (503, Retry-After).
	Unavailable
)

// Error is a refusal the caller may read: the message is meant for them.
// Any other error a method returns is a failure of the store, or the
// request's context ending.
type Error struct {
	Kind Kind
	Msg  string
	// SecondFactor marks a sign-in refused over the second factor, so the
	// UI asks for (another) code.
	SecondFactor bool
}

func (e *Error) Error() string { return e.Msg }

func refuse(k Kind, msg string) error { return &Error{Kind: k, Msg: msg} }

// The messages refusals carry more than once.
const (
	msgWrongPassword = "password is incorrect"
	msgNoUser        = "user not found"
	msgBadName       = "invalid name"
	msgBadSites      = "invalid site assignment"
	msgCodesCapped   = "too many wrong two-factor codes for this account — try again in an hour"
)

// Limits are the sign-in counters (SPEC §2.3).
type Limits struct {
	// Address: password checks from one client network (an IPv4 address,
	// an IPv6 /64), any e-mail: sign-ins and the account routes that take
	// the password.
	Address *throttle.Window
	// Wide: the same from one IPv6 /48, a higher bound, so that the 2^16
	// /64s of one allocation cannot take the whole argon2 queue.
	Wide *throttle.Window
	// AddressAccount: attempts from one network at one e-mail.
	AddressAccount *throttle.Window
	// Delays: failed password and code checks for one e-mail in 15
	// minutes, per client network (and IPv6 /48) and from anywhere. An
	// attempt waits, before it takes its turn, the longer of its network's
	// delay (NetworkDelay, up to a minute) and the e-mail's (NameDelay, a
	// few seconds at most): a delay, never a lock, and what someone else's
	// failures add for the owner signing in from elsewhere is bounded.
	Delays *throttle.Delays
	// Codes: wrong second-factor codes for one e-mail in an hour. At
	// CodeCap no code is checked for the account until the hour passes.
	Codes *throttle.Window
}

const (
	// DelayAfter failures, each attempt waits 1 s, then 2 s, 4 s … up to
	// MaxDelay for failures from its own network, up to NameMaxDelay for
	// those from anywhere.
	DelayAfter   = 5
	MaxDelay     = time.Minute
	NameMaxDelay = 2 * time.Second
	// FailureSpan is how long a failure counts towards the delays.
	FailureSpan = 15 * time.Minute
	// CodeCap wrong codes an hour stop the second factor being checked.
	CodeCap = 10
)

// The two schedules of the sign-in delay (SPEC §2.3 step 3), which Quarry's
// sign-in shares (SPEC §6.4).
var (
	NetworkDelay = throttle.Schedule{After: DelayAfter, First: time.Second, Max: MaxDelay}
	NameDelay    = throttle.Schedule{After: DelayAfter, First: time.Second, Max: NameMaxDelay}
)

// NewDelays returns the sign-in delays: NetworkDelay per name and network,
// NameDelay per name, over FailureSpan.
func NewDelays() *throttle.Delays {
	return throttle.NewDelays(FailureSpan, NetworkDelay, NameDelay)
}

// DefaultLimits are the panel's: 30 a minute per network, 120 a minute per
// IPv6 /48, 8 a minute per network and e-mail, the failure delays over 15
// minutes and the code cap over an hour.
func DefaultLimits() Limits {
	return Limits{
		Address:        throttle.New(30, time.Minute),
		Wide:           throttle.New(120, time.Minute),
		AddressAccount: throttle.New(8, time.Minute),
		Delays:         NewDelays(),
		Codes:          throttle.New(CodeCap, time.Hour),
	}
}

// Service is the accounts domain over one store.
type Service struct {
	Store  *state.Store
	Limits Limits
	// Audit records an event in the audit log; nil records nothing.
	Audit func(actor, action, subject, detail string)
	// Failed is told the client address of every wrong password and wrong
	// second-factor code at sign-in, for the block list (#21); nil is
	// nobody.
	Failed func(address string)
	// Now is the clock the second factor reads; nil is time.Now.
	Now func() time.Time
	// Sleep waits out a failure delay, or returns ctx's error; nil is a
	// timer.
	Sleep func(ctx context.Context, d time.Duration) error

	// turns evaluates one check per e-mail at a time (attempt.go).
	turns *throttle.Lanes
}

// Origin is where a request comes from (SPEC §2.7): the address, for the
// log, and the networks the per-address limits count. Network is the
// address for IPv4 and its /64 for IPv6 (empty: the address); Wide is an
// IPv6 address's /48, empty for IPv4.
type Origin struct {
	Address, Network, Wide string
}

func (o Origin) network() string {
	if o.Network == "" {
		return o.Address
	}
	return o.Network
}

// allowFrom counts a password check against the origin's network, then its
// /48: a request the first refuses does not count for the second.
func (sv *Service) allowFrom(o Origin) bool {
	if !sv.Limits.Address.Allow(o.network()) {
		return false
	}
	return o.Wide == "" || sv.Limits.Wide.Allow(o.Wide)
}

// gate is how every check of an account's password on its own routes
// begins: the origin's limits, then the attempt (attempt.go).
func (sv *Service) gate(ctx context.Context, o Origin, email string) (*attempt, error) {
	if !sv.allowFrom(o) {
		return nil, refuse(Throttled, msgTooMany)
	}
	return sv.begin(ctx, o, email)
}

// New returns a service with the default limits.
func New(store *state.Store, audit func(actor, action, subject, detail string)) *Service {
	decoy() // hash the decoy now, not during the first sign-in
	return &Service{Store: store, Limits: DefaultLimits(), Audit: audit, turns: throttle.NewLanes(AccountQueue)}
}

// failed reports a failed sign-in from address to Failed.
func (sv *Service) failed(address string) {
	if sv.Failed != nil {
		sv.Failed(address)
	}
}

func (sv *Service) audit(actor, action, subject, detail string) {
	if sv.Audit != nil {
		sv.Audit(actor, action, subject, detail)
	}
}

func (sv *Service) now() time.Time {
	if sv.Now != nil {
		return sv.Now()
	}
	return time.Now()
}

// busy turns a full password queue into its refusal.
func busy(err error) error {
	if errors.Is(err, ErrBusy) {
		return refuse(Unavailable, ErrBusy.Error())
	}
	return err
}

// checkPassword checks the password of one's own account in attempt a
// (gate): a wrong password is a failure.
func (sv *Service) checkPassword(ctx context.Context, a *attempt, u state.User, pw, wrong string) error {
	if len(pw) > MaxPasswordLen {
		a.Fail()
		return refuse(Forbidden, wrong)
	}
	ok, err := verifyPassword(ctx, pw, u.PasswordHash)
	if err != nil {
		return busy(err)
	}
	if !ok {
		a.Fail()
		return refuse(Forbidden, wrong)
	}
	return nil
}

// codeResult is what checking a second-factor code found.
type codeResult uint8

const (
	codeOK codeResult = iota
	codeWrong
	codeReused
)

// checkCode spends a code of the account's active secret: its step is
// recorded and must be later than any accepted before. At the code cap no
// code is checked (a refusal); a wrong code counts as a failure of a and
// towards the cap, and the one that reaches the cap is audited as
// login.2fa_brute.
func (sv *Service) checkCode(a *attempt, u state.User, secret, code, address string) (codeResult, error) {
	if sv.Limits.Codes.Blocked(u.Email) {
		return codeWrong, &Error{Kind: Throttled, Msg: msgCodesCapped, SecondFactor: true}
	}
	if step, ok := MatchTOTP(secret, code, sv.now()); ok {
		if fresh, err := sv.Store.MarkTOTPStepUsed(u.ID, step); err == nil && fresh {
			return codeOK, nil
		}
		return codeReused, nil
	}
	a.Fail()
	sv.Limits.Codes.Allow(u.Email)
	if sv.Limits.Codes.Blocked(u.Email) {
		sv.audit(u.Email, "login.2fa_brute", "panel",
			fmt.Sprintf("%d wrong two-factor codes within an hour (last from %s): codes refused until the hour passes", CodeCap, address))
	}
	return codeWrong, nil
}

// printable is s with control characters replaced, for an audit field a
// stranger chose (the e-mail of a failed sign-in).
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if isASCIIControl(r) || (r >= 0x80 && r < 0xa0) || r == 0x2028 || r == 0x2029 {
			return '?'
		}
		return r
	}, s)
}

// IsRefusal reports whether err is a refusal, and which.
func IsRefusal(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}
