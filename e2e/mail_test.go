//go:build e2e

package e2e

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// smtpSink is a relay on loopback that accepts every message, without TLS
// or a sign-in, and keeps them: what the panel handed over, as the relay
// saw it.
type smtpSink struct {
	ln   net.Listener
	mu   sync.Mutex
	msgs []sunkMessage
}

type sunkMessage struct {
	From string
	To   []string
	Data string
}

func startSMTPSink() (*smtpSink, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &smtpSink{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s, nil
}

func (s *smtpSink) port() int { return s.ln.Addr().(*net.TCPAddr).Port }

func (s *smtpSink) serve(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Minute))
	rd := bufio.NewReader(c)
	say := func(line string) { fmt.Fprint(c, line+"\r\n") }
	say("220 cg-e2e sink")
	var cur sunkMessage
	var data strings.Builder
	inData := false
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return
		}
		if inData {
			if line == ".\r\n" {
				inData = false
				cur.Data = data.String()
				s.mu.Lock()
				s.msgs = append(s.msgs, cur)
				s.mu.Unlock()
				cur, data = sunkMessage{}, strings.Builder{}
				say("250 kept")
				continue
			}
			data.WriteString(strings.TrimPrefix(line, "."))
			continue
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			say("250 cg-e2e")
		case strings.HasPrefix(cmd, "MAIL FROM:"):
			cur.From = strings.Trim(strings.TrimSpace(line[len("MAIL FROM:"):]), "<>\r\n")
			say("250 ok")
		case strings.HasPrefix(cmd, "RCPT TO:"):
			cur.To = append(cur.To, strings.Trim(strings.TrimSpace(line[len("RCPT TO:"):]), "<>\r\n"))
			say("250 ok")
		case cmd == "DATA":
			inData = true
			say("354 go on")
		case cmd == "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

// waitFor is the first message whose data contains marker, within 30 s.
func (s *smtpSink) waitFor(marker string) (sunkMessage, bool) {
	for end := time.Now().Add(30 * time.Second); time.Now().Before(end); time.Sleep(500 * time.Millisecond) {
		s.mu.Lock()
		for _, m := range s.msgs {
			if strings.Contains(m.Data, marker) {
				s.mu.Unlock()
				return m, true
			}
		}
		s.mu.Unlock()
	}
	return sunkMessage{}, false
}

func randMarker() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return fmt.Sprintf("cg-e2e-%x", b)
}

// mailRelay (#77): a site's mail() and wp_mail() reach the relay through the
// panel's mail gate, from the site's sender; the test buttons send; the gate
// refuses a user that is no site's and a staging copy, whose pool has no
// sendmail_path.
func (h *harness) mailRelay(t *testing.T) {
	f, stg, ok := h.featureStaging(t)
	if !ok {
		return
	}
	sink, err := startSMTPSink()
	if err != nil {
		h.fail(t, "#77 control: an SMTP sink on loopback", "%v", err)
		return
	}
	defer sink.ln.Close()
	_, sb := h.api.get("/api/settings")
	prev := map[string]string{}
	for _, k := range []string{"mail_host", "mail_port", "mail_security", "mail_username", "mail_from", "alert_emails"} {
		prev[k] = jsonField(sb, k)
	}
	restore := h.undo("restore the mail relay settings", func() {
		h.api.relogin()
		body, _ := json.Marshal(prev)
		h.api.put("/api/settings", body)
		h.api.put(f.api("/mail"), `{"from":"","off":false}`)
	})
	defer restore()
	set, _ := json.Marshal(map[string]string{"mail_host": "127.0.0.1", "mail_port": fmt.Sprint(sink.port()), "mail_security": "none",
		"mail_username": "", "mail_from": "panel@" + f.fs})
	h.check(t, "#77 control: the server-wide relay is accepted", code(h.api.put("/api/settings", set)), "200")

	// The server's test button.
	marker := randMarker()
	c, b := h.api.post("/api/settings/mail/test", fmt.Sprintf(`{"to":"%s@example.com"}`, marker))
	m, got := sink.waitFor("CloudGround")
	h.assert(t, "#77: the settings' test message reaches the relay", c == 200 && got && len(m.To) == 1 && m.To[0] == marker+"@example.com",
		"#77: HTTP %d %s; sink %+v", c, head(b, 200), m)

	// The site's sender reaches the pool (a render) and the connector.
	sender := "shop@" + f.fs
	h.check(t, "#77: the site's sender is accepted", code(h.api.put(f.api("/mail"), fmt.Sprintf(`{"from":%q}`, sender))), "200")
	pool, _ := os.ReadFile("/etc/cloudground-php/" + f.fsu + ".conf")
	h.assert(t, "#77: the site's pool points mail() at the gate and carries the sender",
		strings.Contains(string(pool), "php_admin_value[sendmail_path] = /usr/local/bin/panel-agent sendmail -t -i") && strings.Contains(string(pool), "env[CLOUDGROUND_MAIL_FROM] = "+sender),
		"#77: pool %s", head(pool, 4000))

	// PHP's mail(), through PHP-FPM.
	page := f.fp + "/cg-e2e-mail.php"
	marker = randMarker()
	h.mustWriteAs(t, page, fmt.Sprintf(`<?php echo mail('ann@example.com', %q, "body") ? 'sent' : 'failed';`, marker), f.fsu)
	wpPage := f.fp + "/cg-e2e-wpmail.php"
	wpMarker := randMarker()
	h.mustWriteAs(t, wpPage, fmt.Sprintf(`<?php require __DIR__ . '/wp-load.php'; echo wp_mail('bob@example.com', %q, 'body') ? 'sent' : 'failed';`, wpMarker), f.fsu)
	cleanup := h.undo("remove the mail pages", func() { h.remove(page, wpPage) })
	defer cleanup()
	r := h.get(f.fs, "/cg-e2e-mail.php?n="+marker)
	m, got = sink.waitFor(marker)
	h.assert(t, "#77: PHP's mail() on the site reaches the relay", strings.Contains(string(r.Body), "sent") && got && strings.Join(m.To, ",") == "ann@example.com",
		"#77: page %s %q; sink %+v", r.CodeStr(), head(r.Body, 200), m)
	h.check(t, "#77: the envelope sender is the site's", m.From, sender)

	// wp_mail(), From the site's sender (the connector's filter).
	r = h.get(f.fs, "/cg-e2e-wpmail.php?n="+wpMarker)
	m, got = sink.waitFor(wpMarker)
	h.assert(t, "#77: wp_mail() reaches the relay From the site's sender", strings.Contains(string(r.Body), "sent") && got && strings.Contains(m.Data, "From: ") &&
		strings.Contains(m.Data, sender), "#77: page %s %q; sink %+v", r.CodeStr(), head(r.Body, 200), m)

	// The site's test button.
	marker = randMarker()
	c, b = h.api.post(f.api("/mail/test"), fmt.Sprintf(`{"to":"%s@example.com"}`, marker))
	h.check(t, "#77: the site's test message is sent", fmt.Sprint(c), "200")
	_ = b

	// The gate knows the sender by its uid: a user that is no site's is
	// refused, and so is a staging copy.
	msg := "To: carl@example.com\nSubject: " + randMarker() + "\n\nx\n"
	nobody := h.runIn(msg, "sudo", "-u", "nobody", "/usr/local/bin/panel-agent", "sendmail", "-t", "-i")
	h.assert(t, "#77: the gate refuses a user that is no site's", nobody.Code == 75 && strings.Contains(nobody.Err+nobody.Out, "not a site's"),
		"#77: exit %d %s %s", nobody.Code, nobody.Out, nobody.Err)
	stgr := h.runIn(msg, "sudo", "-u", stg.SystemUser, "/usr/local/bin/panel-agent", "sendmail", "-t", "-i")
	h.assert(t, "#77: the gate refuses a staging copy", stgr.Code == 75 && strings.Contains(stgr.Err+stgr.Out, "staging"),
		"#77: exit %d %s %s", stgr.Code, stgr.Out, stgr.Err)
	stgPool, _ := os.ReadFile("/etc/cloudground-php/" + stg.SystemUser + ".conf")
	h.assert(t, "#77: the staging copy's pool has no sendmail_path", len(stgPool) > 0 && !strings.Contains(string(stgPool), "sendmail_path"),
		"#77: staging pool (%d bytes) %s", len(stgPool), head(stgPool, 400))

	// Mail off: mail() fails again, and nothing reaches the relay.
	h.check(t, "#77: the site's mail is turned off", code(h.api.put(f.api("/mail"), `{"off":true}`)), "200")
	marker = randMarker()
	h.mustWriteAs(t, page, fmt.Sprintf(`<?php echo mail('ann@example.com', %q, "body") ? 'sent' : 'failed';`, marker), f.fsu)
	r = h.get(f.fs, "/cg-e2e-mail.php?n="+marker)
	_, got = sink.waitFor(marker)
	h.assert(t, "#77: with mail off nothing is sent", strings.Contains(string(r.Body), "failed") && !got, "#77: page %q, delivered %v", head(r.Body, 200), got)
}
