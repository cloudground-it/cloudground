// Package accounts is the panel's accounts domain (internal/api/SPEC.md §2):
// sign-in with its limits and second factor, the account's own changes,
// user management and host recovery, over the state store. It knows nothing
// of HTTP; the API's handlers call one method each.
package accounts

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"net/mail"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/argon2"
)

// Bounds on what an account is given (SPEC §2.1).
const (
	MaxEmailLen    = 254
	MinPasswordLen = 12
	MaxPasswordLen = 1024
	maxNameLen     = 64
)

var (
	errBadEmail       = errors.New("invalid email: give a bare address such as name@example.com")
	errPasswordBounds = fmt.Errorf("password must be %d to %d characters", MinPasswordLen, MaxPasswordLen)
)

// NormalizeEmail is how every address enters the panel: a bare address,
// identical to itself once trimmed, lowercased as sign-in looks it up.
// "Name <a@b>" parses as an address too, and stored verbatim it named an
// account nobody could sign in to.
func NormalizeEmail(raw string) (string, error) {
	in := strings.TrimSpace(raw)
	if len(in) > MaxEmailLen {
		return "", errBadEmail
	}
	parsed, err := mail.ParseAddress(in)
	if err != nil || parsed.Name != "" || parsed.Address != in {
		return "", errBadEmail
	}
	return strings.ToLower(in), nil
}

// CheckNewPassword bounds a password being set. The upper bound keeps the
// argon2 input, and the request carrying it, small.
func CheckNewPassword(pw string) error {
	if n := len(pw); n < MinPasswordLen || n > MaxPasswordLen {
		return errPasswordBounds
	}
	return nil
}

// CleanName trims a first or last name and refuses a long one or one with
// control characters.
func CleanName(raw string) (string, bool) {
	name := strings.TrimSpace(raw)
	if len(name) > maxNameLen || strings.IndexFunc(name, isASCIIControl) >= 0 {
		return "", false
	}
	return name, true
}

func isASCIIControl(r rune) bool { return r < 0x20 || r == 0x7f }

// argon2id parameters. Changing them changes the stored format's meaning:
// existing hashes would stop verifying.
const (
	argonTime    = 3
	argonMemory  = 64 << 10 // KiB
	argonLanes   = 2
	argonKeyLen  = 32
	argonSaltLen = 16
	hashScheme   = "argon2id"
)

// argonGate admits two argon2 computations at a time. Each allocates
// 64 MiB and sign-in is open to anyone: unbounded, a burst of requests would
// take more memory than the small servers this panel runs on have.
var argonGate = make(chan struct{}, 2)

// At most argonQueue requests wait for a slot, each at most argonMaxWait;
// past either the answer is ErrBusy (503 with Retry-After), so a flood of
// sign-ins cannot pile up goroutines without bound behind the gate.
var (
	argonQueue   int64 = 32
	argonMaxWait       = 20 * time.Second
	argonWaiting atomic.Int64
)

// ErrBusy is a password check refused because too many are queued.
var ErrBusy = errors.New("the panel is busy checking other passwords — try again in a few seconds")

// acquire takes an argon2 slot, waiting in the bounded queue; it gives up
// when ctx ends.
func acquire(ctx context.Context) error {
	select {
	case argonGate <- struct{}{}:
		return nil
	default:
	}
	if argonWaiting.Add(1) > argonQueue {
		argonWaiting.Add(-1)
		return ErrBusy
	}
	defer argonWaiting.Add(-1)
	timer := time.NewTimer(argonMaxWait)
	defer timer.Stop()
	select {
	case argonGate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ErrBusy
	}
}

func derive(ctx context.Context, pw string, salt []byte) ([]byte, error) {
	if err := acquire(ctx); err != nil {
		return nil, err
	}
	defer func() { <-argonGate }()
	return argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonLanes, argonKeyLen), nil
}

var b64 = base64.RawStdEncoding

// HashPassword returns pw's argon2id hash as argon2id$<salt>$<key>.
func HashPassword(pw string) (string, error) {
	return hashPassword(context.Background(), pw)
}

func hashPassword(ctx context.Context, pw string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := derive(ctx, pw, salt)
	if err != nil {
		return "", err
	}
	return hashScheme + "$" + b64.EncodeToString(salt) + "$" + b64.EncodeToString(key), nil
}

// VerifyPassword reports whether pw matches a HashPassword hash.
func VerifyPassword(pw, hash string) bool {
	ok, _ := verifyPassword(context.Background(), pw, hash)
	return ok
}

// verifyPassword is VerifyPassword in the bounded queue: ErrBusy or ctx's
// error when it could not check at all.
func verifyPassword(ctx context.Context, pw, hash string) (bool, error) {
	scheme, rest, _ := strings.Cut(hash, "$")
	saltText, keyText, ok := strings.Cut(rest, "$")
	if scheme != hashScheme || !ok || strings.Contains(keyText, "$") {
		return false, nil
	}
	salt, err1 := b64.DecodeString(saltText)
	key, err2 := b64.DecodeString(keyText)
	if err1 != nil || err2 != nil {
		return false, nil
	}
	got, err := derive(ctx, pw, salt)
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(got, key) == 1, nil
}

// decoy is the hash an unknown e-mail's password is checked against, so it
// takes as long to refuse as a wrong password.
var decoy = sync.OnceValue(func() string {
	h, err := HashPassword(rand.Text())
	if err != nil {
		panic(err)
	}
	return h
})

// recoveryAlphabet leaves out l, 1, I, O and 0: the password is read off a
// terminal and typed into a browser.
const recoveryAlphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

const recoveryLen = 24

// GeneratePassword returns a random 24-character password from an
// alphabet that cannot be misread.
func GeneratePassword() (string, error) {
	var b strings.Builder
	limit := big.NewInt(int64(len(recoveryAlphabet)))
	for range recoveryLen {
		n, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return "", fmt.Errorf("generating a password: %w", err)
		}
		b.WriteByte(recoveryAlphabet[n.Int64()])
	}
	return b.String(), nil
}
