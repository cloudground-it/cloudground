package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// A redirect_uri validRedirect admits is https, or http to this machine's
// loopback, with a host, and no user part or fragment.
func FuzzValidRedirect(f *testing.F) {
	for _, s := range []string{"https://claude.ai/api/mcp/auth_callback", "http://127.0.0.1:33418/cb", "http://localhost/cb",
		"http://[::1]:8080/x", "http://127.0.0.1.evil.example/cb", "https://u@evil.example/", "https://a.example/#x", "javascript:alert(1)",
		"https:///x", "HTTP://LOCALHOST/x", "http://0x7f000001/", "https://a.example\\@b.example/"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		if !validRedirect(raw) {
			return
		}
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
			t.Fatalf("admitted %q", raw)
		}
		switch u.Scheme {
		case "https":
		case "http":
			h := u.Hostname()
			ip := net.ParseIP(h)
			if h != "localhost" && (ip == nil || !ip.IsLoopback()) {
				t.Fatalf("admitted plain http to %q: %q", h, raw)
			}
		default:
			t.Fatalf("admitted scheme %q: %q", u.Scheme, raw)
		}
	})
}

// redirectWith only adds to the query: the address it answers for an
// admitted redirect_uri has the same scheme, host and path, and the
// parameters given read back as given.
func FuzzRedirectWith(f *testing.F) {
	f.Add("https://a.example/cb", "c0de", "st")
	f.Add("https://a.example/cb?x=1&code=old", "c", "s&t=u")
	f.Add("http://127.0.0.1:9/cb?", "#", "%")
	f.Fuzz(func(t *testing.T, uri, code, st string) {
		if !validRedirect(uri) {
			return
		}
		got := redirectWith(uri, url.Values{"code": {code}, "state": {st}})
		a, _ := url.Parse(uri)
		b, err := url.Parse(got)
		if err != nil || b.Scheme != a.Scheme || b.Host != a.Host || b.Path != a.Path || b.User != nil || b.Fragment != "" {
			t.Fatalf("redirectWith(%q) = %q", uri, got)
		}
		q := b.Query()
		if (code != "" && q.Get("code") != code) || (st != "" && q.Get("state") != st) {
			t.Fatalf("redirectWith(%q) = %q: code %q state %q", uri, got, q.Get("code"), q.Get("state"))
		}
	})
}

// pkceOK holds for the challenge of a well-formed verifier and for no
// other: a changed byte of either fails.
func FuzzPKCE(f *testing.F) {
	f.Add(strings.Repeat("a", 43), "")
	f.Add("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk", "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM")
	f.Add(strings.Repeat("~", 128), "x")
	f.Fuzz(func(t *testing.T, verifier, challenge string) {
		sum := sha256.Sum256([]byte(verifier))
		right := base64.RawURLEncoding.EncodeToString(sum[:])
		ok := pkceOK(verifier, challenge)
		if ok && subtle.ConstantTimeCompare([]byte(challenge), []byte(right)) != 1 {
			t.Fatalf("pkceOK(%q, %q) with the wrong challenge", verifier, challenge)
		}
		wellFormed := len(verifier) >= 43 && len(verifier) <= 128 && strings.Trim(verifier, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~") == ""
		if wellFormed != pkceOK(verifier, right) {
			t.Fatalf("pkceOK(%q, its own challenge) = %v, well formed %v", verifier, !wellFormed, wellFormed)
		}
	})
}

// A delivery passes verifyGitSignature only with the right MAC (or token)
// for its body and the secret; the first header present decides.
func FuzzVerifyGitSignature(f *testing.F) {
	f.Add([]byte(`{"ref":"refs/heads/main"}`), "s3cret", "sha256=00", uint8(0))
	f.Add([]byte("x"), "k", "", uint8(1))
	f.Add([]byte{}, "k", "k", uint8(3))
	f.Fuzz(func(t *testing.T, body []byte, secret, value string, which uint8) {
		if secret == "" {
			return // the handler never checks with an empty secret
		}
		m := hmac.New(sha256.New, []byte(secret))
		m.Write(body)
		mac := hex.EncodeToString(m.Sum(nil))
		names := []string{"X-Hub-Signature-256", "X-Gitea-Signature", "X-Forgejo-Signature", "X-Gitlab-Token"}
		name := names[int(which)%len(names)]
		right := map[string]string{"X-Hub-Signature-256": "sha256=" + mac, "X-Gitea-Signature": mac, "X-Forgejo-Signature": mac, "X-Gitlab-Token": secret}[name]
		h := http.Header{}
		h.Set(name, right)
		if _, ok := verifyGitSignature(h, body, secret); !ok {
			t.Fatalf("%s: the right value %q was refused", name, right)
		}
		h = http.Header{}
		h.Set(name, value)
		_, ok := verifyGitSignature(h, body, secret)
		if value == "" {
			if ok {
				t.Fatalf("%s: an empty header passed", name)
			}
			return
		}
		// hex accepts either case; the Hub prefix is exact.
		var correct bool
		switch name {
		case "X-Gitlab-Token":
			correct = value == secret
		case "X-Hub-Signature-256":
			hexv, has := strings.CutPrefix(value, "sha256=")
			correct = has && strings.EqualFold(hexv, mac)
		default:
			correct = strings.EqualFold(value, mac)
		}
		if ok != correct {
			t.Fatalf("%s: %q gave %v, the right one is %q", name, value, ok, right)
		}
	})
}
