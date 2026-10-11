package accounts

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Time-based one-time passwords, RFC 6238 over RFC 4226: HMAC-SHA1, six
// digits, 30-second steps, the parameters every authenticator app assumes.
// Small enough to keep in-house rather than take a dependency for.

const (
	totpPeriod    = 30
	totpDigits    = 6
	totpModulus   = 1_000_000
	totpSecretLen = 20
	// Issuer names the panel in the authenticator app.
	Issuer = "CloudGround"
)

var totpEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret returns a random 160-bit secret in unpadded base32.
func NewTOTPSecret() string {
	raw := make([]byte, totpSecretLen)
	if _, err := rand.Read(raw); err != nil {
		panic(err) // crypto/rand does not fail on a working system
	}
	return totpEncoding.EncodeToString(raw)
}

// TOTPStep is the time step a moment falls in.
func TOTPStep(at time.Time) uint64 { return uint64(at.Unix() / totpPeriod) }

// TOTPCode is the code of a base32 secret (padded or not, any case) at a
// step; false when the secret does not decode.
func TOTPCode(secret string, step uint64) (string, bool) {
	key, err := totpEncoding.DecodeString(strings.TrimRight(strings.ToUpper(secret), "="))
	if err != nil {
		return "", false
	}
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], step)
	mac := hmac.New(sha1.New, key)
	mac.Write(counter[:])
	sum := mac.Sum(nil)
	at := sum[len(sum)-1] & 0x0f // RFC 4226 §5.3, dynamic truncation
	value := binary.BigEndian.Uint32(sum[at:at+4]) & 0x7fffffff
	code := strconv.FormatUint(uint64(value%totpModulus), 10)
	return strings.Repeat("0", totpDigits-len(code)) + code, true
}

// MatchTOTP checks a code against the step of at and the steps either side
// of it, and returns the step it matched: the caller records it, so the
// code is not accepted twice (RFC 6238 §5.2).
func MatchTOTP(secret, code string, at time.Time) (uint64, bool) {
	code = strings.TrimSpace(code)
	if len(code) != totpDigits {
		return 0, false
	}
	now := TOTPStep(at)
	for _, step := range []uint64{now - 1, now, now + 1} {
		want, ok := TOTPCode(secret, step)
		if ok && subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return step, true
		}
	}
	return 0, false
}

// TOTPURI is the otpauth:// URI an authenticator app enrols from.
func TOTPURI(secret, account string) string {
	q := url.Values{
		"secret":    {secret},
		"issuer":    {Issuer},
		"algorithm": {"SHA1"},
		"digits":    {strconv.Itoa(totpDigits)},
		"period":    {strconv.Itoa(totpPeriod)},
	}
	return "otpauth://totp/" + url.PathEscape(Issuer) + ":" + url.PathEscape(account) + "?" + q.Encode()
}
