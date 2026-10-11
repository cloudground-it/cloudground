package e2e

import (
	"net/http"
	"net/url"
	"sort"
	"sync"
	"time"
)

// Jar is a cookie jar for one host, like the files curl's -b/-c keep in the
// bash suite. Unlike net/http/cookiejar it ignores the Secure attribute: the
// panel's session cookie is Secure, and the suite talks to the API over its
// Unix socket with a plain http:// URL, exactly as nginx does.
type Jar struct {
	mu sync.Mutex
	c  map[string]*http.Cookie
}

// NewJar returns an empty jar.
func NewJar() *Jar { return &Jar{c: map[string]*http.Cookie{}} }

// SetCookies stores cookies by name; MaxAge<0 or a past Expires removes one.
func (j *Jar) SetCookies(_ *url.URL, cookies []*http.Cookie) {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := time.Now()
	for _, c := range cookies {
		if c.MaxAge < 0 || (!c.Expires.IsZero() && c.Expires.Before(now) && c.MaxAge == 0) {
			delete(j.c, c.Name)
			continue
		}
		cp := *c
		j.c[c.Name] = &cp
	}
}

// Cookies returns every stored cookie, sorted by name.
func (j *Jar) Cookies(_ *url.URL) []*http.Cookie {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]*http.Cookie, 0, len(j.c))
	for _, c := range j.c {
		out = append(out, &http.Cookie{Name: c.Name, Value: c.Value})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

// Len is the number of cookies held.
func (j *Jar) Len() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.c)
}
