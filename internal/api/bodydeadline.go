package api

import (
	"io"
	"net/http"
	"sync"
	"time"
)

// The panel's http.Server bounds the headers (ReadHeaderTimeout) and has no
// ReadTimeout: in net/http that deadline covers the whole connection, and
// once a body has been read the server's background read hits it and
// cancels the request's context, whatever the handler is doing (a sign-in
// delay, a Quarry query, a repository test). A body is bounded here instead,
// per request (SPEC.md §1.6): it must arrive within bodyReadBase plus one
// second per bodyMinRate bytes it may hold, and the deadline is lifted as
// soon as it has been read, so it never reaches the handler's work.
var bodyReadBase = 30 * time.Second // a var for tests

// bodyMinRate is the slowest link a body is waited for, in bytes per second.
const bodyMinRate = 64 << 10

// NewHTTPServer is the panel's http.Server for h: headers within 5 s, no
// ReadTimeout (see above), a 10 min WriteTimeout (the event stream moves
// it along), idle connections closed after 90 s.
func NewHTTPServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Minute, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 1 << 20,
	}
}

// bodyBudget is how long a body of at most n bytes may take to arrive.
func bodyBudget(n int64) time.Duration {
	return bodyReadBase + time.Duration(n/bodyMinRate)*time.Second
}

// withBodyDeadline sets r's body read deadline for a body of at most limit
// bytes (its declared length when smaller), and lifts it once the body has
// been read to its end. A request without a body gets no deadline. A body
// that fails (too slow, too large) keeps it: net/http then reads what is
// left of it before answering, and that read must stay bounded too.
func withBodyDeadline(w http.ResponseWriter, r *http.Request, limit int64) {
	if r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0 {
		return
	}
	n := limit
	if r.ContentLength > 0 && r.ContentLength < n {
		n = r.ContentLength
	}
	rc := http.NewResponseController(w)
	if rc.SetReadDeadline(time.Now().Add(bodyBudget(n))) != nil {
		return // the writer cannot (a test recorder): nothing to lift
	}
	r.Body = &deadlineBody{ReadCloser: r.Body, lift: func() { _ = rc.SetReadDeadline(time.Time{}) }}
}

// deadlineBody lifts the read deadline when its body has been read whole.
// net/http starts its background read of the connection at that moment;
// the deadline in place then is the one it waits under.
type deadlineBody struct {
	io.ReadCloser
	once sync.Once
	lift func()
}

func (b *deadlineBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		b.once.Do(b.lift)
	}
	return n, err
}
