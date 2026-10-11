package main

import (
	"net/http"
	"net/url"
	"sync"
	"time"
)

// socketJar is the cookie jar of a client that talks to the panel over its
// unix socket (SPEC.md §2). The panel's session cookie is a __Host- cookie
// marked Secure, which net/http/cookiejar never sends to the plain-HTTP
// URL the socket transport uses, so a password sign-in would be forgotten
// at the next request. The socket has exactly one peer, the panel itself,
// reachable only by root and nginx: there is no network on which a Secure
// cookie could leak, so this jar keeps every cookie the panel sets and
// returns them all, Secure or not. It is never used for an https:// or
// http:// panel URL, where the standard jar's rules apply.
type socketJar struct {
	mu      sync.Mutex
	cookies map[string]*http.Cookie
	now     func() time.Time
}

func newSocketJar() *socketJar {
	return &socketJar{cookies: map[string]*http.Cookie{}, now: time.Now}
}

// SetCookies records what the panel set; a cookie that is deleted (MaxAge
// below zero, or expired) is dropped.
func (j *socketJar) SetCookies(_ *url.URL, cookies []*http.Cookie) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, c := range cookies {
		if c.MaxAge < 0 || (!c.Expires.IsZero() && !c.Expires.After(j.now())) {
			delete(j.cookies, c.Name)
			continue
		}
		j.cookies[c.Name] = &http.Cookie{Name: c.Name, Value: c.Value, Expires: c.Expires}
	}
}

// Cookies returns every live cookie, as name and value only.
func (j *socketJar) Cookies(_ *url.URL) []*http.Cookie {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]*http.Cookie, 0, len(j.cookies))
	for name, c := range j.cookies {
		if !c.Expires.IsZero() && !c.Expires.After(j.now()) {
			delete(j.cookies, name)
			continue
		}
		out = append(out, &http.Cookie{Name: c.Name, Value: c.Value})
	}
	return out
}
