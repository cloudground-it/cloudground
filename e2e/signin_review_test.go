//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// signinReview (R-4.3 review), on the box with a read-only account of its
// own:
//
//   - requests marked X-CG-Background: 1 (the UI's polls) authenticate but
//     do not count as use of the session, so an open tab reaches the idle
//     timeout; a request without the mark does count (the control that the
//     measurement works). A use is recorded at most once a minute, hence the
//     wait;
//   - parallel attempts for one account from one network each wait their
//     own delay, spaced as if they came one after the other, and sleep it
//     outside the account's queue: with five failures on record, four
//     parallel wrong passwords wait 1, 2, 4 and 8 s each after the one
//     before (the last answers after about 15 s: not about 8 s, as when
//     they slept at once and one network guessed as fast as the
//     per-address windows let it), and a fifth while they sleep is not
//     refused (it was 429 when the delays were slept inside the queue): it
//     waits its own 16 s after them, about 31 s.
func (h *harness) signinReview(t *testing.T) {
	email, pw := fmt.Sprintf("e2e-signin-%d@example.com", h.pid), fmt.Sprintf("e2e-signin-password-%d", h.pid)
	_, ub := h.api.post("/api/users", map[string]any{"email": email, "password": pw, "role": "readonly"})
	uid := jsonField(ub, "id")
	h.assert(t, "control: the section's account is created", uid != "", "create: %s", ub)
	if uid == "" {
		return
	}
	defer h.undo("delete the sign-in review account", func() {
		h.api.relogin()
		h.api.del("/api/users/" + uid)
	})()
	u := newAPIClient(h, email, pw)
	if !u.login() {
		h.fail(t, "control: the account signs in", "sign-in refused")
		return
	}
	h.backgroundRequests(t, u)
	h.parallelAttempts(t, u, email, pw)
}

// lastSeen reads the session's last use without making one.
func lastSeen(u *apiClient) (string, []byte) {
	_, b := u.do("GET", "/api/account/sessions", nil, "X-CG-Background: 1")
	var list []struct {
		Current  bool   `json:"current"`
		LastSeen string `json:"last_seen"`
	}
	_ = json.Unmarshal(b, &list)
	for _, se := range list {
		if se.Current {
			return se.LastSeen, b
		}
	}
	return "", b
}

func (h *harness) backgroundRequests(t *testing.T, u *apiClient) {
	before, b := lastSeen(u)
	if before == "" {
		h.fail(t, "control: the session list shows the current session", "%s", head(b, 300))
		return
	}
	time.Sleep(65 * time.Second) // past the once-a-minute write
	for _, p := range []string{"/api/sites", "/api/system/alerts", "/api/tasks"} {
		c, _ := u.do("GET", p, nil, "X-CG-Background: 1")
		h.check(t, "R-4.3 review: a background GET "+p+" still authenticates", fmt.Sprint(c), "200")
	}
	after, _ := lastSeen(u)
	h.check(t, "R-4.3 review: background requests leave the session's last use alone", after, before)
	h.check(t, "control: a request without the mark", code(u.get("/api/sites")), "200")
	used, _ := lastSeen(u)
	h.assert(t, "control: a request without the mark counts as a use", used != "" && used != before, "last use %s, before %s", used, before)
}

func (h *harness) parallelAttempts(t *testing.T, u *apiClient, email, pw string) {
	// Five failures on record, through a session route (the per-pair
	// sign-in limit, 8 a minute, then has room for what follows).
	for i := range 5 {
		c, b := u.post("/api/account/2fa/begin", map[string]string{"password": "wrong-password-e2e"})
		if c != 403 {
			h.fail(t, "control: a wrong password on 2FA enrolment is refused", "attempt %d: %d %s", i+1, c, head(b, 200))
			return
		}
	}
	type outcome struct {
		code int
		took time.Duration
	}
	var wg sync.WaitGroup
	results := make([]outcome, 4)
	start := time.Now()
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, _ := newAPIClient(h, email, pw).post("/api/login", map[string]string{"email": email, "password": "wrong-password-e2e"})
			results[i] = outcome{c, time.Since(start)}
		}()
	}
	time.Sleep(400 * time.Millisecond) // the four are in flight, starting at 1, 3, 7 and 15 s
	c, b := newAPIClient(h, email, pw).post("/api/login", map[string]string{"email": email, "password": "wrong-password-e2e"})
	fifth := time.Since(start)
	h.assert(t, "R-4.3 review: a fifth attempt while four sleep their delays is not refused for the account: it waits its own 16 s after theirs",
		c == 401 && fifth >= 30*time.Second, "%d after %v: %s", c, fifth, head(b, 200))
	wg.Wait()
	var slowest time.Duration
	codes := []string{}
	for _, r := range results {
		codes = append(codes, fmt.Sprint(r.code))
		slowest = max(slowest, r.took)
	}
	h.check(t, "R-4.3 review: the four parallel wrong passwords are each refused", strings.Join(codes, ","), "401,401,401,401")
	h.assert(t, "S-H3 review: parallel attempts for one account from one network are no faster than sequential ones (1, 2, 4, 8 s, one after the other)",
		slowest >= 14500*time.Millisecond && slowest < 22*time.Second, "the last answered after %v", slowest)
}
