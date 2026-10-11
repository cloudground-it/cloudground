//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// streamEvent is one message of GET /api/events.
type streamEvent struct {
	ID     string
	Type   string          `json:"type"`
	SiteID int64           `json:"site_id"`
	Data   json.RawMessage `json:"data"`
	At     time.Time       // when this suite read it
}

// eventStream reads /api/events in the background and keeps everything.
type eventStream struct {
	mu     sync.Mutex
	events []streamEvent
	status int
	header http.Header
	ended  chan struct{}
	cancel context.CancelFunc
	err    error
}

// openEvents opens the stream with hc (which carries the session cookie).
func openEvents(hc *http.Client, url string, hdr ...string) *eventStream {
	ctx, cancel := context.WithCancel(context.Background())
	st := &eventStream{ended: make(chan struct{}), cancel: cancel}
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	for _, kv := range hdr {
		k, v, _ := strings.Cut(kv, ":")
		req.Header.Set(strings.TrimSpace(k), strings.TrimSpace(v))
	}
	resp, err := hc.Do(req)
	if err != nil {
		st.err = err
		close(st.ended)
		return st
	}
	st.status, st.header = resp.StatusCode, resp.Header
	go func() {
		defer close(st.ended)
		defer resp.Body.Close()
		rd := bufio.NewReader(resp.Body)
		var ev streamEvent
		var data string
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				st.mu.Lock()
				st.err = err
				st.mu.Unlock()
				return
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case line == "":
				if data != "" && json.Unmarshal([]byte(data), &ev) == nil {
					ev.At = time.Now()
					st.mu.Lock()
					st.events = append(st.events, ev)
					st.mu.Unlock()
				}
				ev, data = streamEvent{}, ""
			case strings.HasPrefix(line, "id: "):
				ev.ID = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	return st
}

func (st *eventStream) close() { st.cancel() }

func (st *eventStream) snapshot() []streamEvent {
	st.mu.Lock()
	defer st.mu.Unlock()
	return append([]streamEvent(nil), st.events...)
}

// wait polls the stream until match finds an event, up to timeout.
func (st *eventStream) wait(timeout time.Duration, match func(streamEvent) bool) (streamEvent, bool) {
	for end := time.Now().Add(timeout); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		for _, ev := range st.snapshot() {
			if match(ev) {
				return ev, true
			}
		}
		select {
		case <-st.ended:
			return streamEvent{}, false
		default:
		}
	}
	return streamEvent{}, false
}

// streamClient is for streams: the API client's own transport and
// cookies, without its five-minute timeout.
func (a *apiClient) streamClient() *http.Client {
	return &http.Client{Transport: a.hc.Transport, Jar: a.hc.Jar}
}

// events (F8): the event stream, as the browser gets it. An operator's stream
// carries its own site's events and nothing of another site; a task's
// progress and a site's status change arrive at once; the stream lives
// through nginx on :8443 with its heartbeats.
func (h *harness) events(t *testing.T) {
	// Site A, the operator's.
	aid := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"static"}`, h.dom("ev-a")))
	h.check(t, "F8 control: the operator's site provisions", h.api.waitActive(aid, 120*time.Second), "active")
	if aid == 0 {
		return
	}
	ope, opw := fmt.Sprintf("e2e-ev-op-%d@example.com", h.pid), fmt.Sprintf("e2e-ev-operator-pw-%d", h.pid)
	_, ub := h.api.post("/api/users", fmt.Sprintf(`{"email":%q,"password":%q,"role":"operator","site_ids":[%d]}`, ope, opw, aid))
	opid := jsonField(ub, "id")
	defer h.undo("delete the events operator account", func() {
		h.api.relogin()
		if opid != "" {
			h.api.del("/api/users/" + opid)
		} else {
			h.deleteUserByEmail(ope)
		}
	})()
	op := newAPIClient(h, ope, opw)
	h.assert(t, "F8 control: the operator signs in", op.login(), "operator login failed")

	opSt := openEvents(op.streamClient(), h.Panel+"/api/events")
	defer opSt.close()
	adSt := openEvents(h.api.streamClient(), h.Panel+"/api/events")
	defer adSt.close()
	h.check(t, "F8: the event stream answers the operator", strconv.Itoa(opSt.status), "200")
	h.check(t, "F8: the event stream answers the admin", strconv.Itoa(adSt.status), "200")
	h.check(t, "F8: the event stream is text/event-stream", adSt.header.Get("Content-Type"), "text/event-stream")

	// Site B, created by the admin while both streams are open. Its status
	// is polled through the API alongside, to time the event against it.
	bid := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"static"}`, h.dom("ev-b")))
	var activeSeen time.Time
	for end := time.Now().Add(120 * time.Second); bid != 0 && time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
		if st := h.api.site(bid).Status; st == "active" || st == "error" {
			activeSeen = time.Now()
			break
		}
	}
	ev, ok := adSt.wait(5*time.Second, func(e streamEvent) bool {
		return e.Type == "site.changed" && e.SiteID == bid && bytes.Contains(e.Data, []byte(`"status":"active"`))
	})
	lag := time.Duration(0)
	if ok && !activeSeen.IsZero() {
		lag = ev.At.Sub(activeSeen)
	}
	h.assert(t, "F8: a site's change to active reaches the admin stream within 2 s", ok && !activeSeen.IsZero() && lag < 2*time.Second,
		"site.changed active for %d: seen=%v, %v after the API said so", bid, ok, lag)
	if ok {
		t.Logf("(the event arrived %v after the API reported active)", lag)
	}

	// The marker: a task on A, which both streams must carry.
	_, wb := h.api.post(fmt.Sprintf("/api/sites/%d/warm", aid), `{}`)
	tid := taskID(wb)
	isTask := func(typ string) func(streamEvent) bool {
		return func(e streamEvent) bool {
			return e.Type == typ && e.SiteID == aid && bytes.Contains(e.Data, []byte(fmt.Sprintf(`"id":%d,`, tid)))
		}
	}
	_, progressed := adSt.wait(30*time.Second, isTask("task.progress"))
	h.assert(t, "F8: a task's progress arrives on the stream", tid != 0 && progressed, "task %d: no task.progress on the admin stream", tid)
	_, opFinished := opSt.wait(120*time.Second, isTask("task.finished"))
	h.assert(t, "F8: the operator's stream carries its own site's task", opFinished, "task %d: no task.finished on the operator stream", tid)

	if bid != 0 {
		h.check(t, "F8 control: the admin deletes the other site", h.api.deleteSite(bid), "202")
		h.api.teardownGone(bid)
		_, sawB := adSt.wait(5*time.Second, func(e streamEvent) bool { return e.Type == "site.deleted" && e.SiteID == bid })
		h.assert(t, "F8 control: the admin stream carries the other site's events", sawB, "no site.deleted for %d on the admin stream", bid)
	}
	var leaked []string
	for _, e := range opSt.snapshot() {
		if e.Type == "ping" || e.Type == "resync" {
			continue
		}
		if e.SiteID != aid {
			leaked = append(leaked, fmt.Sprintf("%s site=%d", e.Type, e.SiteID))
		}
	}
	h.assert(t, "F8: an operator's stream carries no event of a site it is not assigned", len(leaked) == 0 && bid != 0,
		"operator stream carried %v", leaked)
	h.assert(t, "F8: the operator's stream is not ended by the other site's events", func() bool {
		select {
		case <-opSt.ended:
			return false
		default:
			return true
		}
	}(), "operator stream ended: %v", opSt.err)

	h.eventsThroughNginx(t, aid)
	h.check(t, "F8 control: the operator's site is deleted", h.api.deleteSite(aid), "202")
	h.api.teardownGone(aid)
}

// eventsThroughNginx: the browser's stream goes through nginx on :8443, whose
// buffering and read timeout could hold or cut it. It must stay open past a
// minute, with heartbeats, and still deliver an event at the end.
func (h *harness) eventsThroughNginx(t *testing.T, aid int64) {
	base := "https://" + h.IP + ":8443"
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Jar: jar, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // the panel's self-signed IP certificate
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, "127.0.0.1:8443")
		},
	}}
	login, _ := json.Marshal(map[string]string{"email": h.Email, "password": h.PW})
	lreq, _ := http.NewRequest("POST", base+"/api/login", bytes.NewReader(login))
	lreq.Header.Set("Content-Type", "application/json")
	lreq.Header.Set("Origin", base)
	lreq.Header.Set("Sec-Fetch-Site", "same-origin")
	lc := 0
	if resp, err := hc.Do(lreq); err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		lc = resp.StatusCode
	}
	h.assert(t, "F8 control: the admin signs in through nginx on :8443", lc == 200, "login through :8443: %d", lc)

	// As a browser opens it: same origin.
	st := openEvents(hc, base+"/api/events", "Sec-Fetch-Site: same-origin", "Accept: text/event-stream")
	defer st.close()
	h.check(t, "F8: the event stream answers through nginx", strconv.Itoa(st.status), "200")
	time.Sleep(65 * time.Second)
	pings := 0
	for _, e := range st.snapshot() {
		if e.Type == "ping" {
			pings++
		}
	}
	alive := func() bool {
		select {
		case <-st.ended:
			return false
		default:
			return true
		}
	}()
	h.assert(t, "F8: the stream through nginx is open after 65 s, with heartbeats", alive && pings >= 2,
		"after 65 s: open=%v, %d pings, err=%v", alive, pings, st.err)
	// And it still delivers, unbuffered: a task's events within seconds.
	_, wb := h.api.post(fmt.Sprintf("/api/sites/%d/warm", aid), `{}`)
	tid := taskID(wb)
	_, got := st.wait(10*time.Second, func(e streamEvent) bool {
		return e.Type == "task.created" && bytes.Contains(e.Data, []byte(fmt.Sprintf(`"id":%d,`, tid)))
	})
	h.assert(t, "F8: an event reaches the stream through nginx at once", tid != 0 && got, "task %d: no task.created through :8443 within 10 s", tid)
	h.api.waitTask(tid, 120*time.Second)

	// A page of another origin is refused, through nginx too.
	x := openEvents(hc, base+"/api/events", "Sec-Fetch-Site: cross-site")
	x.close()
	h.check(t, "F8: a cross-site request for the stream is refused", strconv.Itoa(x.status), "403")
}
