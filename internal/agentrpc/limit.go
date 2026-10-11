package agentrpc

import (
	"fmt"
	"net/http"
)

// DefaultMaxInFlight is how many requests the agent serves at once; one
// more is refused at once as unavailable (503, SPEC.md §3.7).
const DefaultMaxInFlight = 64

func newSlots(n int) chan struct{} {
	if n <= 0 {
		n = DefaultMaxInFlight
	}
	return make(chan struct{}, n)
}

// take holds one of the server's slots for an authenticated request, or
// answers 503 and returns false when all are taken.
func (s *Server) take(w http.ResponseWriter, r *http.Request) (func(), bool) {
	select {
	case s.slots <- struct{}{}:
		return func() { <-s.slots }, true
	default:
		s.log.Warn("agentrpc: refused a request: too many in flight", "limit", cap(s.slots), "path", r.URL.Path)
		writeJSON(w, http.StatusServiceUnavailable, &Error{Code: CodeUnavailable, Message: "The agent is busy; try again in a moment",
			Detail: fmt.Sprintf("%d requests in flight", cap(s.slots))})
		return nil, false
	}
}
