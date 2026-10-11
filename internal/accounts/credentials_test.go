package accounts

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"
)

// RFC 6238 appendix B, the SHA-1 rows, cut to six digits (RFC 4226 takes
// the low digits). The seed is "12345678901234567890".
func TestTOTPVectors(t *testing.T) {
	const seed = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	for unix, want := range map[int64]string{
		59:          "287082",
		1111111109:  "081804",
		1111111111:  "050471",
		1234567890:  "005924",
		2000000000:  "279037",
		20000000000: "353130",
	} {
		got, ok := TOTPCode(seed, TOTPStep(time.Unix(unix, 0)))
		if !ok || got != want {
			t.Errorf("T=%d: %q %v, want %q", unix, got, ok, want)
		}
	}
	// Padding and case do not matter; a secret that is not base32 fails.
	if got, _ := TOTPCode(strings.ToLower(seed)+"====", 1); got != "287082" {
		t.Errorf("lowercase padded secret: %q", got)
	}
	if _, ok := TOTPCode("not base32!", 1); ok {
		t.Error("a malformed secret produced a code")
	}
}

func TestMatchTOTP(t *testing.T) {
	secret := NewTOTPSecret()
	if len(secret) != 32 || strings.Contains(secret, "=") {
		t.Fatalf("secret %q: want 32 base32 characters, unpadded", secret)
	}
	at := time.Unix(1_800_000_015, 0)
	step := TOTPStep(at)
	for _, d := range []int64{-1, 0, 1} {
		code, _ := TOTPCode(secret, uint64(int64(step)+d))
		got, ok := MatchTOTP(secret, " "+code+" ", at)
		if !ok || got != uint64(int64(step)+d) {
			t.Errorf("step %+d: matched %d %v", d, got, ok)
		}
	}
	for _, d := range []int64{-2, 2} {
		code, _ := TOTPCode(secret, uint64(int64(step)+d))
		if _, ok := MatchTOTP(secret, code, at); ok {
			t.Errorf("a code %d steps away was accepted", d)
		}
	}
	for _, bad := range []string{"", "12345", "1234567", "abcdef"} {
		if _, ok := MatchTOTP(secret, bad, at); ok {
			t.Errorf("%q accepted", bad)
		}
	}
	if NewTOTPSecret() == secret {
		t.Fatal("two secrets alike")
	}
}

func TestTOTPURI(t *testing.T) {
	got := TOTPURI("ABC", "a+b@example.com")
	want := "otpauth://totp/CloudGround:a+b@example.com?algorithm=SHA1&digits=6&issuer=CloudGround&period=30&secret=ABC"
	if got != want {
		t.Fatalf("%s\nwant %s", got, want)
	}
}

func TestPasswords(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(h, "$")
	if len(parts) != 3 || parts[0] != "argon2id" || len(parts[1]) != 22 || len(parts[2]) != 43 {
		t.Fatalf("hash format %q", h)
	}
	if !VerifyPassword("correct horse battery", h) || VerifyPassword("correct horse batterY", h) {
		t.Fatal("verify")
	}
	if h2, _ := HashPassword("correct horse battery"); h2 == h {
		t.Fatal("no salt")
	}
	for _, bad := range []string{"", "argon2id$x", "bcrypt$a$b", "argon2id$!!$" + parts[2], h + "$extra"} {
		if VerifyPassword("correct horse battery", bad) {
			t.Errorf("verified against %q", bad)
		}
	}
	// The stored format, built by hand from the documented parameters (SPEC
	// §2.1): hashes already in a database keep verifying.
	salt := []byte("0123456789abcdef")
	key := argon2.IDKey([]byte("an existing password"), salt, 3, 64*1024, 2, 32)
	existing := "argon2id$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(key)
	if !VerifyPassword("an existing password", existing) {
		t.Fatal("a hash in the documented format does not verify")
	}

	for pw, ok := range map[string]bool{
		strings.Repeat("a", 11): false, strings.Repeat("a", 12): true,
		strings.Repeat("a", 1024): true, strings.Repeat("a", 1025): false,
	} {
		if err := CheckNewPassword(pw); (err == nil) != ok {
			t.Errorf("%d characters: %v", len(pw), err)
		}
	}
}

// Two argon2 computations at a time, no more: the third waits for a slot.
func TestArgonGate(t *testing.T) {
	if cap(argonGate) != 2 {
		t.Fatalf("gate of %d", cap(argonGate))
	}
	argonGate <- struct{}{}
	argonGate <- struct{}{}
	done := make(chan struct{})
	go func() { HashPassword("waiting-for-a-slot"); close(done) }()
	select {
	case <-done:
		t.Fatal("a hash ran with both slots taken")
	case <-time.After(200 * time.Millisecond):
	}
	<-argonGate
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the hash did not run once a slot was free")
	}
	<-argonGate
}

func TestNormalizeEmail(t *testing.T) {
	for in, want := range map[string]string{
		"a@example.com":         "a@example.com",
		"  Mixed@Example.COM  ": "mixed@example.com",
	} {
		if got, err := NormalizeEmail(in); err != nil || got != want {
			t.Errorf("%q = %q %v", in, got, err)
		}
	}
	for _, in := range []string{"X <x@y.z>", "<x@y.z>", `"X" <x@y.z>`, "x@y.z (c)", "nope", "", strings.Repeat("a", 250) + "@y.zz"} {
		if got, err := NormalizeEmail(in); err == nil {
			t.Errorf("%q accepted as %q", in, got)
		}
	}
}

func TestCleanName(t *testing.T) {
	if got, ok := CleanName("  Ada  "); !ok || got != "Ada" {
		t.Fatalf("%q %v", got, ok)
	}
	if got, ok := CleanName("Zoë Ångström"); !ok || got != "Zoë Ångström" {
		t.Fatalf("%q %v", got, ok)
	}
	for _, bad := range []string{"a\nb", "a\x00", "a\x7f", strings.Repeat("x", 65)} {
		if _, ok := CleanName(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

// Read off a terminal and typed into a browser.
func TestGeneratePassword(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		p, err := GeneratePassword()
		if err != nil {
			t.Fatal(err)
		}
		if len(p) != 24 || strings.ContainsAny(p, "l1IO0") || seen[p] {
			t.Fatalf("%q", p)
		}
		seen[p] = true
	}
}

func TestClient(t *testing.T) {
	for ua, want := range map[string][2]string{
		"Mozilla/5.0 (Windows NT 10.0) AppleWebKit/537.36 Chrome/120.0 Safari/537.36 Edg/120.0":       {"Edge", "Windows"},
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0) AppleWebKit/605.1.15 Version/17.0 Safari/605.1": {"Safari", "macOS"},
		"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) Safari/604.1":                         {"Safari", "iOS"},
		"Mozilla/5.0 (Linux; Android 14) Chrome/120.0 Mobile Safari/537.36":                           {"Chrome", "Android"},
		"Mozilla/5.0 (X11; Linux x86_64; rv:120.0) Gecko/20100101 Firefox/120.0":                      {"Firefox", "Linux"},
		"Mozilla/5.0 (X11; Linux x86_64) Chrome/120.0 Safari/537.36 OPR/105.0":                        {"Opera", "Linux"},
		"curl/8.5.0": {"curl", ""},
		"":           {"", ""},
	} {
		if b, o := Client(ua); b != want[0] || o != want[1] {
			t.Errorf("%q = %s/%s, want %s/%s", ua, b, o, want[0], want[1])
		}
	}
}
