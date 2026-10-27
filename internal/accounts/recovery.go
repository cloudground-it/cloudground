package accounts

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/cloudground-it/cloudground/internal/state"
)

// Recovery codes (SPEC §2.11): ten single-use codes that stand in for the
// second factor when the phone or the passkeys are gone. Each is 16
// characters of Crockford's base32, 80 random bits: enough that the SHA-256
// the store keeps is no shortcut for whoever copies the database, and far
// beyond what the code cap lets anyone guess online.

const (
	// RecoveryCodeCount is how many codes a set has.
	RecoveryCodeCount = 10
	recoveryCodeLen   = 16
	crockford         = "0123456789abcdefghjkmnpqrstvwxyz" // Crockford's base32
)

// NewRecoveryCode returns a random code as it is shown: xxxx-xxxx-xxxx-xxxx.
func NewRecoveryCode() string {
	raw := make([]byte, recoveryCodeLen)
	if _, err := rand.Read(raw); err != nil {
		panic(err) // crypto/rand does not fail on a working system
	}
	var b strings.Builder
	for i, c := range raw {
		if i > 0 && i%4 == 0 {
			b.WriteByte('-')
		}
		b.WriteByte(crockford[c&31]) // 256 is a multiple of 32: no bias
	}
	return b.String()
}

// NormalizeRecoveryCode reads a code as typed: any case, dashes and spaces
// ignored, o read as 0 and i, l as 1. False when what is left is not a code.
func NormalizeRecoveryCode(typed string) (string, bool) {
	var b strings.Builder
	for _, r := range strings.ToLower(typed) {
		switch {
		case r == '-' || r == ' ' || r == '\t':
			continue
		case r == 'o':
			r = '0'
		case r == 'i' || r == 'l':
			r = '1'
		}
		if !strings.ContainsRune(crockford, r) {
			return "", false
		}
		b.WriteRune(r)
	}
	code := b.String()
	return code, len(code) == recoveryCodeLen
}

// RecoveryCodeHash is what the store keeps of a code: the hex SHA-256 of
// its normal form, "" for text that is not a code.
func RecoveryCodeHash(typed string) string {
	code, ok := NormalizeRecoveryCode(typed)
	if !ok {
		return ""
	}
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// issueRecoveryCodes makes a fresh set for the account, in place of any
// earlier one, and returns the codes to show once.
func (sv *Service) issueRecoveryCodes(userID int64) ([]string, error) {
	codes := make([]string, RecoveryCodeCount)
	hashes := make([]string, RecoveryCodeCount)
	for i := range codes {
		codes[i] = NewRecoveryCode()
		hashes[i] = RecoveryCodeHash(codes[i])
	}
	if err := sv.Store.ReplaceRecoveryCodes(userID, hashes); err != nil {
		return nil, err
	}
	return codes, nil
}

// spendRecoveryCode spends one of the account's unused codes and returns
// how many are left; false when the code is not one of them.
func (sv *Service) spendRecoveryCode(u state.User, typed string) (int, bool, error) {
	hash := RecoveryCodeHash(typed)
	if hash == "" {
		return 0, false, nil
	}
	ok, err := sv.Store.SpendRecoveryCode(u.ID, hash)
	if err != nil || !ok {
		return 0, false, err
	}
	left := 0
	if fresh, err := sv.Store.GetUser(u.ID); err == nil {
		left = fresh.RecoveryCodesLeft
	}
	return left, true, nil
}

// codesLeft is the audit detail of a spent code.
func codesLeft(n int) string { return fmt.Sprintf("%d recovery codes left", n) }

// RegenerateRecoveryCodes replaces the account's recovery codes once the
// proof checks out (SPEC §2.11); the earlier ones stop working.
func (sv *Service) RegenerateRecoveryCodes(ctx context.Context, from Origin, u state.User, rp RP, actor string, p Proof) ([]string, error) {
	if !u.HasSecondFactor() {
		return nil, refuse(Invalid, "turn on two-factor sign-in first")
	}
	a, err := sv.gate(ctx, from, u.Email)
	if err != nil {
		return nil, err
	}
	defer a.end()
	if err := sv.confirm(ctx, a, u, rp, p, confirmation{refusal: msgConfirmFactor, purpose: "make new recovery codes", actor: actor}); err != nil {
		return nil, err
	}
	codes, err := sv.issueRecoveryCodes(u.ID)
	if err != nil {
		return nil, err
	}
	sv.audit(actor, "recovery_codes.generated", "self", "")
	return codes, nil
}
